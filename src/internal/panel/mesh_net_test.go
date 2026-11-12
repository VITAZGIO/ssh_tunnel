package panel

// Сети, журнал событий и проверка доступа: четыре прокси панели к meshd,
// клиент в meshsvc и их работа с настоящим и со старым meshd.

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sshtunnel/internal/meshsvc"
)

// recAdmin запоминает вызовы новых методов; остальное — «meshd не отвечает».
type recAdmin struct {
	fakeDownAdmin
	calls []string
	err   error
}

func (a *recAdmin) RenameNetwork(_ context.Context, netID, name string) error {
	a.calls = append(a.calls, "rename:"+netID+":"+name)
	return a.err
}

func (a *recAdmin) DeleteNetwork(_ context.Context, netID string) error {
	a.calls = append(a.calls, "delete:"+netID)
	return a.err
}

func (a *recAdmin) Events(_ context.Context, after int64, limit int) (meshsvc.Events, error) {
	a.calls = append(a.calls, "events:"+itoaT(after)+":"+itoaT(int64(limit)))
	return meshsvc.Events{Seq: 7, Events: []meshsvc.Event{{Seq: 7, Type: "connect", Net: "n", Device: "d"}}}, a.err
}

func (a *recAdmin) Check(_ context.Context, netID, from, to string, port int) (meshsvc.CheckResult, error) {
	a.calls = append(a.calls, "check:"+netID+":"+from+":"+to+":"+itoaT(int64(port)))
	return meshsvc.CheckResult{Allowed: true, Rule: 2, Reason: "rule", Visible: true}, a.err
}

func itoaT(v int64) string { return strconv.FormatInt(v, 10) }

func TestСетиПрокситЧерезПанель(t *testing.T) {
	s, pass := newTestServer(t)
	adm := &recAdmin{}
	m := newTestMesh(t, newFakeMeshSystem(), adm, "main.example", 22)
	h := s.WithMesh(m).Handler()

	// Без входа — 401, и до meshd дело не доходит.
	for _, p := range []string{"/api/mesh/net/rename", "/api/mesh/net/delete", "/api/mesh/events", "/api/mesh/check"} {
		method := http.MethodPost
		if strings.HasSuffix(p, "events") {
			method = http.MethodGet
		}
		if rec, _ := doJSON(t, h, method, p, map[string]string{}, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s без входа: %d", p, rec.Code)
		}
	}
	if len(adm.calls) != 0 {
		t.Fatalf("meshd дёрнули без входа: %v", adm.calls)
	}
	cookies := loggedInCookies(t, h, pass)

	rec, body := doJSON(t, h, http.MethodPost, "/api/mesh/net/rename", map[string]string{"net": "n1", "name": "Дом"}, cookies)
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("rename: %d %v", rec.Code, body)
	}
	rec, body = doJSON(t, h, http.MethodPost, "/api/mesh/net/delete", map[string]string{"net": "n1"}, cookies)
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("delete: %d %v", rec.Code, body)
	}
	rec, body = doJSON(t, h, http.MethodGet, "/api/mesh/events?after=5&limit=50", nil, cookies)
	evs, _ := body["events"].([]any)
	if rec.Code != http.StatusOK || body["seq"] != float64(7) || len(evs) != 1 {
		t.Fatalf("events: %d %v", rec.Code, body)
	}
	if e := evs[0].(map[string]any); e["type"] != "connect" || e["device"] != "d" {
		t.Fatalf("событие: %v", e)
	}
	rec, body = doJSON(t, h, http.MethodPost, "/api/mesh/check",
		map[string]any{"net": "n1", "from": "a", "to": "b", "port": 22}, cookies)
	if rec.Code != http.StatusOK || body["allowed"] != true || body["rule"] != float64(2) || body["reason"] != "rule" {
		t.Fatalf("check: %d %v", rec.Code, body)
	}
	want := []string{"rename:n1:Дом", "delete:n1", "events:5:50", "check:n1:a:b:22"}
	if strings.Join(adm.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("вызовы meshd: %v, ждали %v", adm.calls, want)
	}

	// Неверный метод и ошибка meshd — понятным ответом, без паники.
	if rec, _ := doJSON(t, h, http.MethodGet, "/api/mesh/net/delete", nil, cookies); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET на delete: %d", rec.Code)
	}
	if rec, _ := doJSON(t, h, http.MethodPost, "/api/mesh/events", map[string]string{}, cookies); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST на events: %d", rec.Code)
	}
	adm.err = meshsvc.ErrNotRunning
	rec, body = doJSON(t, h, http.MethodPost, "/api/mesh/net/delete", map[string]string{"net": "n1"}, cookies)
	if rec.Code != http.StatusBadRequest || body["error"] == "" {
		t.Errorf("ошибка meshd: %d %v", rec.Code, body)
	}
}

// Новая панель + старый meshd: на новые пути он отвечает «404 page not found».
func TestСтарыйMeshdНовыеЗапросыПонятнаяОшибка(t *testing.T) {
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
	old := http.NewServeMux() // знает только то, что знал старый meshd
	old.HandleFunc("/v1/state", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"2","startedAt":1,"networks":[{"id":"n","devices":[]}],"servers":[],"self":{}}`))
	})
	srv := &http.Server{Handler: old}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	a := meshsvc.NewAdmin(sock)
	ctx := context.Background()
	st, err := a.State(ctx)
	if err != nil || len(st.Networks) != 1 || st.Networks[0].Name != "" || st.Networks[0].Created != 0 {
		t.Fatalf("старый state: %+v %v", st, err)
	}
	if err := a.RenameNetwork(ctx, "n", "x"); err != meshsvc.ErrUnsupported {
		t.Errorf("rename: %v", err)
	}
	if err := a.DeleteNetwork(ctx, "n"); err != meshsvc.ErrUnsupported {
		t.Errorf("delete: %v", err)
	}
	if _, err := a.Events(ctx, 0, 0); err != meshsvc.ErrUnsupported {
		t.Errorf("events: %v", err)
	}
	if _, err := a.Check(ctx, "n", "a", "b", 22); err != meshsvc.ErrUnsupported {
		t.Errorf("check: %v", err)
	}

	// Через панель это тот же понятный текст, статус 400.
	s, pass := newTestServer(t)
	h := s.WithMesh(newTestMesh(t, newFakeMeshSystem(), a, "main.example", 22)).Handler()
	cookies := loggedInCookies(t, h, pass)
	rec, body := doJSON(t, h, http.MethodPost, "/api/mesh/net/delete", map[string]string{"net": "n"}, cookies)
	msg, _ := body["error"].(string)
	if rec.Code != http.StatusBadRequest || !strings.Contains(msg, "обновите meshd") {
		t.Errorf("панель: %d %v", rec.Code, body)
	}
	rec, _ = doJSON(t, h, http.MethodGet, "/api/mesh/events", nil, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("events через панель: %d", rec.Code)
	}
}

// Настоящий meshd: устройство подключается, сеть получает имя, события
// пишутся, check отвечает, удаление сети отключает устройство.
func TestСетиНаНастоящемMeshd(t *testing.T) {
	addr, sock := startTestMeshd(t)
	a := meshsvc.NewAdmin(sock)
	ctx := context.Background()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte(`{"op":"hello","net":"key","device":"d1","name":"ноут"}` + "\n"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	if line, err := br.ReadString('\n'); err != nil || !strings.Contains(line, "welcome") {
		t.Fatalf("welcome: %q %v", line, err)
	}

	st, err := a.State(ctx)
	if err != nil || len(st.Networks) != 1 {
		t.Fatalf("state: %+v %v", st, err)
	}
	nid := st.Networks[0].ID
	if st.Networks[0].Created == 0 || st.Networks[0].EmptySince != 0 || st.Networks[0].Name != "" {
		t.Fatalf("новая сеть: %+v", st.Networks[0])
	}
	if err := a.RenameNetwork(ctx, nid, "  Дом "); err != nil {
		t.Fatal(err)
	}
	if err := a.RenameNetwork(ctx, "нет", "x"); err == nil || err == meshsvc.ErrUnsupported {
		t.Errorf("rename чужой сети: %v", err)
	}
	if st, _ = a.State(ctx); st.Networks[0].Name != "Дом" {
		t.Errorf("имя: %q", st.Networks[0].Name)
	}

	ev, err := a.Events(ctx, 0, 0)
	var types []string
	for _, e := range ev.Events {
		types = append(types, e.Type)
	}
	if err != nil || strings.Join(types, ",") != "new-device,connect,rename" || ev.Seq != 3 {
		t.Fatalf("события: %v %+v %v", types, ev, err)
	}
	if ev2, _ := a.Events(ctx, 2, 10); len(ev2.Events) != 1 || ev2.Events[0].Type != "rename" {
		t.Errorf("after=2: %+v", ev2)
	}

	res, err := a.Check(ctx, nid, "d1", "d1", 22)
	if err != nil || !res.Allowed || res.Reason != "policy off" || res.Rule != -1 {
		t.Errorf("check: %+v %v", res, err)
	}
	if res, _ := a.Check(ctx, nid, "d1", "ghost", 22); res.Allowed || res.Reason != "unknown device" {
		t.Errorf("check неизвестного: %+v", res)
	}

	if err := a.DeleteNetwork(ctx, nid); err != nil {
		t.Fatal(err)
	}
	// Сообщения, успевшие прийти до закрытия, пропускаем: ждём конец связи.
	for {
		if _, err := br.ReadString('\n'); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Error("соединение устройства осталось открытым после удаления сети")
			}
			break
		}
	}
	if st, _ = a.State(ctx); len(st.Networks) != 0 {
		t.Errorf("сеть осталась: %+v", st.Networks)
	}
}
