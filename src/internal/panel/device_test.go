package panel

// Профиль устройства в панели (ТЗ 16): одобрение, заморозка и цепочка
// выхода через настоящий и через старый meshd; простой доступ одним запросом.

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sshtunnel/internal/meshsvc"
)

func (fakeDownAdmin) SetApproval(context.Context, string, bool) error { return meshsvc.ErrNotRunning }
func (fakeDownAdmin) Approve(context.Context, string, string) error   { return meshsvc.ErrNotRunning }
func (fakeDownAdmin) Freeze(context.Context, string, string, bool) error {
	return meshsvc.ErrNotRunning
}
func (fakeDownAdmin) SetExit(context.Context, string, string, string, []string) (meshsvc.ExitOrder, error) {
	return meshsvc.ExitOrder{}, meshsvc.ErrNotRunning
}

// meshDevice — устройство на настоящем meshd: управляющее соединение и
// чтение его строк.
type meshDevice struct {
	c net.Conn
	r *bufio.Reader
}

func helloDevice(t *testing.T, addr, id string) (*meshDevice, map[string]any) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.Write([]byte(`{"op":"hello","net":"key","device":"` + id + `","name":"` + id + `","since":1}` + "\n"))
	d := &meshDevice{c, bufio.NewReader(c)}
	return d, d.next(t, "welcome")
}

func (d *meshDevice) next(t *testing.T, op string) map[string]any {
	t.Helper()
	d.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := d.r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("ждали %s: %v", op, err)
		}
		var m map[string]any
		json.Unmarshal(line, &m)
		if m["op"] == op {
			return m
		}
	}
}

func TestПрофильУстройстваНаНастоящемMeshd(t *testing.T) {
	addr, sock := startTestMeshd(t)
	a := meshsvc.NewAdmin(sock)
	s, pass := newTestServer(t)
	h := s.WithMesh(newTestMesh(t, newFakeMeshSystem(), a, "main.example", 22)).Handler()
	cookies := loggedInCookies(t, h, pass)
	post := func(path string, body any) map[string]any {
		t.Helper()
		rec, out := doJSON(t, h, http.MethodPost, path, body, cookies)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %v", path, rec.Code, out)
		}
		return out
	}

	pc, _ := helloDevice(t, addr, "pc")
	st, err := a.State(context.Background())
	if err != nil || len(st.Networks) != 1 {
		t.Fatalf("state: %+v %v", st, err)
	}
	nid := st.Networks[0].ID

	// Одобрение: включили — новое устройство ждёт, одобрили — в сети.
	post("/api/mesh/net/approval", map[string]any{"net": nid, "on": true})
	nw, w := helloDevice(t, addr, "new")
	if w["hold"] != "pending" {
		t.Fatalf("welcome нового: %v", w)
	}
	st, _ = a.State(context.Background())
	if !st.Networks[0].Approval {
		t.Fatal("сеть без одобрения")
	}
	for _, d := range st.Networks[0].Devices {
		if d.ID == "new" && !d.Pending {
			t.Fatalf("в панели не ждёт: %+v", d)
		}
	}
	post("/api/mesh/approve", map[string]any{"net": nid, "device": "new"})
	if m := nw.next(t, "hold"); m["hold"] != nil {
		t.Fatalf("hold после одобрения: %v", m)
	}

	// Заморозка и разморозка.
	post("/api/mesh/freeze", map[string]any{"net": nid, "device": "new", "on": true})
	if m := nw.next(t, "hold"); m["hold"] != "frozen" {
		t.Fatalf("hold: %v", m)
	}
	rec, body := doJSON(t, h, http.MethodPost, "/api/mesh/check", map[string]any{"net": nid, "from": "pc", "to": "new", "port": 22}, cookies)
	if rec.Code != 200 || body["allowed"] != false || body["reason"] != "frozen" {
		t.Fatalf("check замороженного: %v", body)
	}
	post("/api/mesh/freeze", map[string]any{"net": nid, "device": "new", "on": false})

	// Цепочка: устройство рассказало о серверах — панель задаёт, оно получает.
	pc.c.Write([]byte(`{"op":"profiles","profiles":[{"k":"fra.example:22","a":true},{"k":"lon.example:22"}]}` + "\n"))
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, _ = a.State(context.Background())
		got := 0
		for _, d := range st.Networks[0].Devices {
			if d.ID == "pc" {
				got = len(d.Profiles)
			}
		}
		if got == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("meshd не принял отчёт о серверах")
		}
		time.Sleep(20 * time.Millisecond)
	}
	out := post("/api/mesh/exit", map[string]any{"net": nid, "device": "pc", "entry": "fra.example:22", "via": []string{"lon.example:22"}})
	if e, _ := out["exit"].(map[string]any); e == nil || e["entry"] != "fra.example:22" {
		t.Fatalf("ответ: %v", out)
	}
	m := pc.next(t, "exit")
	e, _ := m["exit"].(map[string]any)
	if e == nil || e["entry"] != "fra.example:22" || e["by"] == nil {
		t.Fatalf("устройство получило: %v", m)
	}
	rec, body = doJSON(t, h, http.MethodPost, "/api/mesh/exit", map[string]any{"net": nid, "device": "pc", "entry": "nope:22"}, cookies)
	if rec.Code != http.StatusBadRequest || body["error"] == nil {
		t.Fatalf("сервер без профиля: %d %v", rec.Code, body)
	}

	ev, _ := a.Events(context.Background(), 0, 0)
	var types []string
	for _, e := range ev.Events {
		types = append(types, e.Type)
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{"approval", "pending", "approve", "freeze", "unfreeze", "exit"} {
		if !strings.Contains(joined, want) {
			t.Errorf("нет события %s: %s", want, joined)
		}
	}
}

// Новая панель + старый meshd: новые пути — понятная ошибка, не паника.
func TestПрофильУстройстваСтарыйMeshd(t *testing.T) {
	dir, err := os.MkdirTemp("", "pm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("нет unix-сокетов: %v", err)
	}
	old := http.NewServeMux()
	old.HandleFunc("/v1/state", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"4","startedAt":1,"networks":[{"id":"n","devices":[{"id":"d"}]}],"servers":[],"self":{}}`))
	})
	srv := &http.Server{Handler: old}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	a := meshsvc.NewAdmin(sock)
	ctx := context.Background()
	st, err := a.State(ctx)
	if err != nil || st.Networks[0].Approval || st.Networks[0].Devices[0].Pending || st.Networks[0].Devices[0].ExitSet != nil {
		t.Fatalf("старый state: %+v %v", st, err)
	}
	if err := a.SetApproval(ctx, "n", true); err != meshsvc.ErrUnsupported {
		t.Errorf("approval: %v", err)
	}
	if err := a.Approve(ctx, "n", "d"); err != meshsvc.ErrUnsupported {
		t.Errorf("approve: %v", err)
	}
	if err := a.Freeze(ctx, "n", "d", true); err != meshsvc.ErrUnsupported {
		t.Errorf("freeze: %v", err)
	}
	if _, err := a.SetExit(ctx, "n", "d", "a:22", nil); err != meshsvc.ErrUnsupported {
		t.Errorf("exit: %v", err)
	}
}

func accessServer(t *testing.T) (*Server, http.Handler, []*http.Cookie, Client, Client) {
	t.Helper()
	s, pass := newTestServer(t)
	st, err := OpenSettings(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.WithSettings(st)
	a, err := s.clients.CreateClient("Ноутбук", DeviceLinux)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.clients.CreateClient("Телефон", DeviceAndroid)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	return s, h, loggedInCookies(t, h, pass), a, b
}

func TestПростойДоступОднимЗапросом(t *testing.T) {
	s, h, cookies, a, b := accessServer(t)
	// Было: ноутбук в старой группе «Дом».
	s.settings.Update(func(st *Settings) error {
		st.Access = AccessSettings{Groups: []string{"Дом"}}
		return nil
	})
	s.clients.Update(a.ID, func(c *Client) { c.Groups = []string{"Дом"} })

	req := SimpleAccess{
		AccessSettings: AccessSettings{Enabled: true, Groups: []string{"~mine", "~guest"},
			Rules: []AccessRule{{From: "~mine", To: "*"}}},
		Clients: map[string][]string{a.ID: {"~mine"}, b.ID: {"~guest"}},
	}
	rec, body := doJSON(t, h, http.MethodPost, "/api/access/simple", req, cookies)
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("сохранение: %d %v", rec.Code, body)
	}
	got := s.settings.Get().Access
	if !got.Enabled || strings.Join(got.Groups, ",") != "~guest,~mine" || len(got.Rules) != 1 {
		t.Fatalf("права: %+v", got)
	}
	ca, _ := s.clients.Get(a.ID)
	cb, _ := s.clients.Get(b.ID)
	if strings.Join(ca.Groups, ",") != "~mine" || strings.Join(cb.Groups, ",") != "~guest" {
		t.Fatalf("группы: %v %v", ca.Groups, cb.Groups)
	}

	// Ошибка в любой части — не меняется ничего.
	for name, bad := range map[string]SimpleAccess{
		"нет группы": {AccessSettings: AccessSettings{Groups: []string{"~mine"}},
			Clients: map[string][]string{a.ID: {"~mine"}, b.ID: {"~чужая"}}},
		"нет клиента": {AccessSettings: AccessSettings{Groups: []string{"~mine", "~guest"}},
			Clients: map[string][]string{"tun_ghost": {"~mine"}}},
		"группа пропала у оставшегося": {AccessSettings: AccessSettings{Groups: []string{"~mine"}},
			Clients: map[string][]string{a.ID: {"~mine"}}},
		"плохое правило": {AccessSettings: AccessSettings{Groups: []string{"~mine", "~guest"},
			Rules: []AccessRule{{From: "~нет", To: "*"}}}, Clients: map[string][]string{a.ID: {}}},
	} {
		rec, body := doJSON(t, h, http.MethodPost, "/api/access/simple", bad, cookies)
		if rec.Code != http.StatusBadRequest || body["error"] == nil {
			t.Errorf("%s: %d %v", name, rec.Code, body)
		}
		ca, _ := s.clients.Get(a.ID)
		cb, _ := s.clients.Get(b.ID)
		if g := s.settings.Get().Access; strings.Join(g.Groups, ",") != "~guest,~mine" ||
			strings.Join(ca.Groups, ",") != "~mine" || strings.Join(cb.Groups, ",") != "~guest" {
			t.Fatalf("%s: что-то изменилось: %+v %v %v", name, g, ca.Groups, cb.Groups)
		}
	}
}
