package main

import (
	"bufio"
	"encoding/json"
	"os"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock — часы для тестов: время двигается только руками.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testServer(t *testing.T) (*server, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	s := newServer("")
	s.now = clk.now
	return s, clk
}

// addNet кладёт в сервер сеть с устройствами (user — пользователь устройства).
func addNet(s *server, id string, devs map[string]string) *network {
	n := &network{Devices: map[string]*device{}, Created: s.now().Unix()}
	for did, user := range devs {
		n.Devices[did] = &device{ID: did, Name: did, User: user, IP: "198.19.0." + did}
	}
	s.nets[id] = n
	return n
}

func TestСетьИмяПроверяется(t *testing.T) {
	s, _ := testServer(t)
	addNet(s, "n1", map[string]string{"a": "u"})
	if err := s.renameNet("n1", "  Дом  "); err != nil {
		t.Fatal(err)
	}
	if got := s.adminState().Networks[0].Name; got != "Дом" {
		t.Fatalf("имя %q, ждали обрезанное «Дом»", got)
	}
	if err := s.renameNet("n1", strings.Repeat("я", 65)); err == nil {
		t.Error("65 символов приняты")
	}
	if err := s.renameNet("n1", strings.Repeat("я", 64)); err != nil {
		t.Errorf("64 символа отвергнуты: %v", err)
	}
	if err := s.renameNet("n1", "a\nb"); err == nil {
		t.Error("управляющий символ принят")
	}
	if err := s.renameNet("нет", "x"); err == nil {
		t.Error("переименована несуществующая сеть")
	}
	if err := s.renameNet("n1", ""); err != nil || s.nets["n1"].Name != "" {
		t.Errorf("имя не сбросилось: %v", err)
	}
}

func TestСетьУдаляетсяСОтключениемУстройств(t *testing.T) {
	s, _ := testServer(t)
	n := addNet(s, "n1", map[string]string{"a": "u"})
	srvSide, cliSide := net.Pipe()
	defer cliSide.Close()
	n.Devices["a"].ctrl = newCtrlConn(srvSide)
	addNet(s, "n2", map[string]string{"b": "u"})

	if err := s.deleteNet("n1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.nets["n1"]; ok {
		t.Error("сеть осталась")
	}
	if _, ok := s.nets["n2"]; !ok {
		t.Error("удалена чужая сеть")
	}
	cliSide.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := cliSide.Read(make([]byte, 1)); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Errorf("соединение устройства не закрыто: %v", err)
	}
	if err := s.deleteNet("n1"); err == nil {
		t.Error("повторное удаление не вернуло ошибку")
	}
	_, evs := s.eventsAfter(0, 0)
	if len(evs) != 1 || evs[0].Type != "net-delete" || evs[0].Net != "n1" {
		t.Errorf("события: %+v", evs)
	}
}

func TestПустаяСетьУбираетсяЧерезСемьДней(t *testing.T) {
	s, clk := testServer(t)
	addNet(s, "full", map[string]string{"a": "u"})
	addNet(s, "gone", map[string]string{"b": "u"})
	if err := s.forget("gone", "b"); err != nil {
		t.Fatal(err)
	}
	if s.nets["gone"].EmptySince != clk.now().Unix() {
		t.Fatalf("emptySince не выставлен: %d", s.nets["gone"].EmptySince)
	}

	clk.add(7*24*time.Hour - time.Second)
	s.prune()
	if _, ok := s.nets["gone"]; !ok {
		t.Fatal("сеть убрана раньше срока")
	}
	clk.add(2 * time.Second)
	s.prune()
	if _, ok := s.nets["gone"]; ok {
		t.Error("сеть не убрана через 7 дней")
	}
	// Сеть с устройствами не трогаем, сколько бы ни прошло.
	clk.add(365 * 24 * time.Hour)
	s.prune()
	if _, ok := s.nets["full"]; !ok {
		t.Error("убрана сеть с устройствами")
	}
}

func TestВозвратУстройстваСбрасываетEmptySince(t *testing.T) {
	s, clk := testServer(t)
	n := addNet(s, "n", map[string]string{"a": "u"})
	s.forget("n", "a")
	clk.add(24 * time.Hour)
	n.Devices["b"] = &device{ID: "b"} // устройство вернулось
	s.prune()
	if n.EmptySince != 0 {
		t.Errorf("emptySince остался: %d", n.EmptySince)
	}
	clk.add(30 * 24 * time.Hour)
	s.prune()
	if _, ok := s.nets["n"]; !ok {
		t.Error("сеть с устройством убрана")
	}
}

func TestСтарыйФайлСостоянияЗагружается(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	old := `{"n1":{"devices":{"a":{"id":"a","name":"a","ip":"198.19.0.1","firstSeen":"2026-10-01T00:00:00Z","lastSeen":"` +
		time.Now().Format(time.RFC3339) + `"}}},"empty":{"devices":{}}}`
	if err := writeFile(path, old); err != nil {
		t.Fatal(err)
	}
	s := newServer(path)
	n1, empty := s.nets["n1"], s.nets["empty"]
	if n1 == nil || empty == nil {
		t.Fatalf("сети не загрузились: %v", s.nets)
	}
	if n1.Created == 0 || n1.EmptySince != 0 || n1.Name != "" {
		t.Errorf("n1: %+v", n1)
	}
	if empty.EmptySince == 0 {
		t.Error("пустая сеть из старого файла без emptySince")
	}
	// Новые поля переживают сохранение.
	s.renameNet("n1", "Дом")
	s.save()
	s2 := newServer(path)
	if s2.nets["n1"].Name != "Дом" || s2.nets["n1"].Created != n1.Created {
		t.Errorf("не сохранилось: %+v", s2.nets["n1"])
	}
}

func TestЖурналСобытий(t *testing.T) {
	s, clk := testServer(t)
	for i := 0; i < maxEvents+50; i++ {
		clk.add(time.Second)
		s.mu.Lock()
		s.emitLocked(event{Type: "connect", Net: "n"})
		s.mu.Unlock()
	}
	seq, evs := s.eventsAfter(0, 5000)
	if seq != maxEvents+50 || len(evs) != maxEvents {
		t.Fatalf("seq=%d, событий %d", seq, len(evs))
	}
	if evs[0].Seq != 51 || evs[len(evs)-1].Seq != maxEvents+50 {
		t.Errorf("кольцо: %d..%d", evs[0].Seq, evs[len(evs)-1].Seq)
	}
	_, evs = s.eventsAfter(maxEvents+45, 0)
	if len(evs) != 5 || evs[0].Seq != maxEvents+46 {
		t.Errorf("after: %+v", evs)
	}
	_, evs = s.eventsAfter(0, 3) // лимит — самые новые, последними
	if len(evs) != 3 || evs[2].Seq != maxEvents+50 {
		t.Errorf("limit: %+v", evs)
	}
	_, evs = s.eventsAfter(0, 0) // по умолчанию 200
	if len(evs) != 200 {
		t.Errorf("limit по умолчанию: %d", len(evs))
	}
}

func TestПроверкаДоступа(t *testing.T) {
	s, _ := testServer(t)
	addNet(s, "n", map[string]string{"pc": "tun_pc", "tv": "tun_tv"})
	s.relayRecs = map[string]*relayRecord{"srv": {IP: "198.19.255.1"}}
	chk := func(from, to string, port int) checkResult {
		t.Helper()
		r, err := s.check("n", from, to, port)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := chk("pc", "tv", 22); !r.Allowed || r.Reason != "policy off" || r.Rule != -1 {
		t.Errorf("права выключены: %+v", r)
	}
	s.policy = policy{Enabled: true,
		Users: map[string][]string{"tun_pc": {"Мои"}, "tun_tv": {"ТВ"}},
		Rules: []policyRule{{From: "ТВ", To: "ТВ"}, {From: "Мои", To: "*", Ports: "22,80"}}}
	if r := chk("pc", "tv", 22); !r.Allowed || r.Rule != 1 || r.Reason != "rule" || !r.Visible {
		t.Errorf("по правилу 1: %+v", r)
	}
	if r := chk("pc", "tv", 443); r.Allowed || r.Rule != -1 || r.Reason != "no rule" || !r.Visible {
		t.Errorf("порт не из правила: %+v", r)
	}
	if r := chk("tv", "pc", 22); r.Allowed || r.Reason != "no rule" {
		t.Errorf("телевизор → ПК: %+v", r)
	}
	if r := chk("pc", "nobody", 22); r.Allowed || r.Reason != "unknown device" {
		t.Errorf("неизвестное устройство: %+v", r)
	}
	if r := chk("srv", "pc", 22); r.Allowed || r.Reason != "server cannot initiate" {
		t.Errorf("сервер звонит: %+v", r)
	}
	if r := chk("pc", "srv", 22); !r.Allowed || r.Rule != 1 {
		t.Errorf("ПК → сервер (правило «*»): %+v", r)
	}
	// Тот же ответ, что у allowed — единственного судьи для dial.
	if got := s.policy.allowed("tun_pc", "tun_tv", 22); got != chk("pc", "tv", 22).Allowed {
		t.Error("check и allowed разошлись")
	}
	if _, err := s.check("нет", "pc", "tv", 22); err == nil {
		t.Error("несуществующая сеть принята")
	}
	if _, err := s.check("n", "pc", "tv", 70000); err == nil {
		t.Error("порт 70000 принят")
	}
}

func writeFile(path, data string) error {
	return os.WriteFile(path, []byte(data), 0o600)
}

// admin — вызов admin-API через httptest, без сокета.
func admin(t *testing.T, s *server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.adminHandler().ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestAdminHTTP(t *testing.T) {
	s, _ := testServer(t)
	addNet(s, "n", map[string]string{"pc": "tun_pc", "tv": "tun_tv"})

	if code, out := admin(t, s, http.MethodPost, "/v1/net/rename", `{"net":"n","name":"Дом"}`); code != 200 || out["ok"] != true {
		t.Fatalf("rename: %d %v", code, out)
	}
	_, st := admin(t, s, http.MethodGet, "/v1/state", "")
	nets := st["networks"].([]any)
	first := nets[0].(map[string]any)
	if first["name"] != "Дом" || first["created"] == nil || first["emptySince"] == nil {
		t.Errorf("state: %v", first)
	}
	if code, out := admin(t, s, http.MethodPost, "/v1/net/rename", `{"net":"x","name":"a"}`); code != 400 || out["error"] == nil {
		t.Errorf("rename чужой: %d %v", code, out)
	}
	if code, _ := admin(t, s, http.MethodGet, "/v1/net/delete", ""); code != 400 {
		t.Errorf("delete по GET: %d", code)
	}

	code, out := admin(t, s, http.MethodPost, "/v1/check", `{"net":"n","from":"pc","to":"tv","port":22}`)
	if code != 200 || out["allowed"] != true || out["rule"].(float64) != -1 || out["reason"] != "policy off" {
		t.Errorf("check: %d %v", code, out)
	}

	_, ev := admin(t, s, http.MethodGet, "/v1/events?after=0&limit=10", "")
	if ev["seq"].(float64) != 1 || len(ev["events"].([]any)) != 1 {
		t.Errorf("events: %v", ev)
	}
	e := ev["events"].([]any)[0].(map[string]any)
	if e["type"] != "rename" || e["net"] != "n" || e["text"] != "Дом" || e["seq"].(float64) != 1 {
		t.Errorf("event: %v", e)
	}
	_, ev = admin(t, s, http.MethodGet, "/v1/events?after=1", "")
	if len(ev["events"].([]any)) != 0 {
		t.Errorf("after=1: %v", ev)
	}

	if code, _ := admin(t, s, http.MethodPost, "/v1/net/delete", `{"net":"n"}`); code != 200 {
		t.Errorf("delete: %d", code)
	}
	_, st = admin(t, s, http.MethodGet, "/v1/state", "")
	if len(st["networks"].([]any)) != 0 {
		t.Errorf("сеть осталась: %v", st)
	}
}

// TestСобытияНаСоединениях — по-настоящему: подключение, отказ по правилам,
// отключение и забывание дают события в нужном порядке.
func TestСобытияНаСоединениях(t *testing.T) {
	s, _ := testServer(t)
	var mu sync.Mutex
	users := map[string]string{}
	s.connUser = func(c net.Conn) string {
		mu.Lock()
		defer mu.Unlock()
		return users[c.RemoteAddr().String()]
	}
	s.policy = policy{Enabled: true,
		Users: map[string][]string{"tun_pc": {"Мои"}, "tun_tv": {"ТВ"}},
		Rules: []policyRule{{From: "Мои", To: "*"}}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	open := func(user string, m msg) (net.Conn, *bufio.Reader) {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		mu.Lock()
		users[c.LocalAddr().String()] = user
		mu.Unlock()
		writeMsg(c, m)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		return c, bufio.NewReader(c)
	}
	readMsg := func(r *bufio.Reader) msg {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m msg
		json.Unmarshal(line, &m)
		return m
	}
	types := func() []string {
		_, evs := s.eventsAfter(0, 0)
		var out []string
		for _, e := range evs {
			out = append(out, e.Type)
		}
		return out
	}
	want := func(exp ...string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			got := types()
			if strings.Join(got, ",") == strings.Join(exp, ",") {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("события %v, ждали %v", got, exp)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	pcC, pcR := open("tun_pc", msg{Op: "hello", Net: "k", Device: "pc", Name: "pc"})
	pcW := readMsg(pcR)
	want("new-device", "connect")
	_, tvR := open("tun_tv", msg{Op: "hello", Net: "k", Device: "tv", Name: "tv"})
	readMsg(tvR)
	want("new-device", "connect", "new-device", "connect")

	// Телевизору к ПК нельзя: событие denied с from, peer и портом.
	_, r := open("tun_tv", msg{Op: "dial", Net: "k", Device: "tv", To: pcW.IP, Port: 8080})
	if m := readMsg(r); m.Op != "error" {
		t.Fatalf("пустили: %+v", m)
	}
	_, evs := s.eventsAfter(0, 0)
	d := evs[len(evs)-1]
	if d.Type != "denied" || d.Device != "tv" || d.Peer != "pc" || d.Port != 8080 || d.Net != netID("k") {
		t.Errorf("denied: %+v", d)
	}

	// Переименование и забывание.
	if err := s.rename(netID("k"), "tv", "Телек"); err != nil {
		t.Fatal(err)
	}
	pcC.Close()
	want("new-device", "connect", "new-device", "connect", "denied", "rename", "disconnect")
	if err := s.forget(netID("k"), "tv"); err != nil {
		t.Fatal(err)
	}
	want("new-device", "connect", "new-device", "connect", "denied", "rename", "disconnect", "forget")
}

func TestСобытияСерверов(t *testing.T) {
	s, _ := testServer(t)
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.relayLink(a, msg{Op: "server", Device: "srv1", Name: "Второй"})
	}()
	br := bufio.NewReader(b)
	b.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadBytes('\n'); err != nil { // welcome
		t.Fatal(err)
	}
	_, evs := s.eventsAfter(0, 0)
	if len(evs) != 1 || evs[0].Type != "server-up" || evs[0].Device != "srv1" {
		t.Fatalf("up: %+v", evs)
	}
	b.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relayLink не закончился")
	}
	_, evs = s.eventsAfter(0, 0)
	if len(evs) != 2 || evs[1].Type != "server-down" {
		t.Fatalf("down: %+v", evs)
	}
}
