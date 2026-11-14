package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Профиль устройства в панели (ТЗ 16): одобрение, заморозка, цепочка выхода.

// hasPeer — есть ли в списке устройство с адресом ip.
func hasPeer(list []peer, ip string) bool {
	for _, p := range list {
		if p.IP == ip {
			return true
		}
	}
	return false
}

// nextPeers — следующий список устройств (welcome не в счёт).
func (w *wireConn) nextPeers(t *testing.T) []peer {
	t.Helper()
	return w.next(t, "peers").Peers
}

// waitPeers ждёт список, для которого ok — true.
func (w *wireConn) waitPeers(t *testing.T, ok func([]peer) bool) []peer {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l := w.nextPeers(t); ok(l) {
			return l
		}
	}
	t.Fatal("не дождались нужного списка устройств")
	return nil
}

// dialFrom — звонок устройства dev к to:port; ответ meshd (connected или error).
func dialFrom(t *testing.T, addr, dev, to string, port int) (*wireConn, msg) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	writeMsg(c, msg{Op: "dial", Net: "k", Device: dev, To: to, Port: port})
	w := &wireConn{c, nil}
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := readRaw(c)
	if err != nil {
		t.Fatalf("звонок %s → %s: %v", dev, to, err)
	}
	var m msg
	json.Unmarshal(line, &m)
	return w, m
}

// readRaw — одна строка без буфера (дальше идут сырые байты).
func readRaw(c net.Conn) ([]byte, error) {
	var line []byte
	var b [1]byte
	for {
		if _, err := c.Read(b[:]); err != nil {
			return nil, err
		}
		if b[0] == '\n' {
			return line, nil
		}
		line = append(line, b[0])
	}
}

// answer — устройство dev принимает вызов: дальше по соединению сырые байты.
func answerCall(t *testing.T, addr, dev, call string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	writeMsg(c, msg{Op: "accept", Net: "k", Device: dev, Call: call, OK: true})
	return c
}

func approvalServer(t *testing.T) (*server, string) {
	t.Helper()
	s, _ := testServer(t)
	s.connUser = func(net.Conn) string { return "u" }
	return s, serve(t, s)
}

func TestОдобрениеНовогоУстройства(t *testing.T) {
	s, addr := approvalServer(t)
	pc, wpc := helloConn(t, addr, msg{Net: "k", Device: "pc", Name: "pc", Since: 1})
	if wpc.Hold != "" {
		t.Fatalf("знакомое устройство ждёт: %q", wpc.Hold)
	}
	nid := netID("k")
	if err := s.setApproval(nid, true); err != nil {
		t.Fatal(err)
	}

	nw, wn := helloConn(t, addr, msg{Net: "k", Device: "new", Name: "new", Since: 1})
	if wn.Hold != "pending" {
		t.Fatalf("новое устройство не ждёт одобрения: %+v", wn)
	}
	if len(wn.Peers) != 1 || wn.Peers[0].IP != wn.IP {
		t.Fatalf("ждущее видит других: %+v", wn.Peers)
	}
	for _, p := range pc.nextPeers(t) {
		if p.IP == wn.IP {
			t.Fatal("ждущее устройство есть в списке у других")
		}
	}

	// Никуда не звонит, ему не звонят, напрямую не сводят.
	if _, m := dialFrom(t, addr, "new", wpc.IP, 22); m.Op != "error" || !strings.Contains(m.Error, "одобрения") {
		t.Fatalf("ждущего пустили: %+v", m)
	}
	if _, m := dialFrom(t, addr, "new", s.self.IP, 22); m.Op != "error" {
		t.Fatalf("ждущего пустили к серверу: %+v", m)
	}
	if _, m := dialFrom(t, addr, "pc", wn.IP, 22); m.Op != "error" || !strings.Contains(m.Error, "нет такого устройства") {
		t.Fatalf("к ждущему пустили: %+v", m)
	}
	writeMsg(pc.c, msg{Op: "p2p", To: wn.IP, Call: "c1", FP: "x"})
	writeMsg(pc.c, msg{Op: "echo", To: wn.IP, Call: "e1"})
	res, _ := s.check(nid, "pc", "new", 22)
	if res.Allowed || res.Reason != "pending" || res.Visible {
		t.Fatalf("check: %+v", res)
	}
	evs := eventTypes(s)
	if !strings.Contains(evs, "new-device pending connect") {
		t.Fatalf("журнал: %s", evs)
	}

	// Одобрили — оно узнаёт, и все видят друг друга.
	if err := s.approve(nid, "new"); err != nil {
		t.Fatal(err)
	}
	if h := nw.next(t, "hold"); h.Hold != "" {
		t.Fatalf("hold после одобрения: %+v", h)
	}
	pc.waitPeers(t, func(l []peer) bool { return hasPeer(l, wn.IP) })
	_, m := dialFrom(t, addr, "pc", wn.IP, 22)
	_ = m
	if in := nw.next(t, "incoming"); in.From != wpc.IP {
		t.Fatalf("вызов после одобрения: %+v", in)
	}
	// p2p и echo, отправленные, пока оно ждало, к нему не дошли.
	nw.c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		line, err := nw.r.ReadBytes('\n')
		if err != nil {
			break
		}
		if strings.Contains(string(line), `"c1"`) || strings.Contains(string(line), `"e1"`) {
			t.Fatalf("до ждущего дошло: %s", line)
		}
	}
}

func eventTypes(s *server) string {
	_, evs := s.eventsAfter(0, 0)
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return strings.Join(out, " ")
}

func TestЗаморозкаРвётСоединенияИСписки(t *testing.T) {
	s, addr := approvalServer(t)
	a, wa := helloConn(t, addr, msg{Net: "k", Device: "a", Name: "a", Since: 1})
	b, wb := helloConn(t, addr, msg{Net: "k", Device: "b", Name: "b", Since: 1})
	a.waitPeers(t, func(l []peer) bool { return hasPeer(l, wb.IP) })

	// Идущее через сервер соединение a → b.
	call := make(chan string, 1)
	go func() {
		w, m := dialFrom(t, addr, "a", wb.IP, 80)
		if m.Op != "connected" {
			call <- "звонок: " + m.Op + " " + m.Error
			return
		}
		w.c.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err := io.ReadAll(w.c) // ждём, пока заморозка не порвёт
		if err != nil {
			call <- "чтение: " + err.Error()
			return
		}
		call <- ""
	}()
	in := b.next(t, "incoming")
	peerSide := answerCall(t, addr, "b", in.Call)
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.nets[netID("k")].Devices["b"].streams)
		s.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("соединение не записалось")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := s.freeze(netID("k"), "b", true); err != nil {
		t.Fatal(err)
	}
	if got := <-call; got != "" {
		t.Fatal(got)
	}
	peerSide.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(peerSide); err != nil {
		t.Fatalf("у замороженного соединение не закрылось: %v", err)
	}
	if h := b.next(t, "hold"); h.Hold != "frozen" {
		t.Fatalf("hold: %+v", h)
	}
	a.waitPeers(t, func(l []peer) bool { return !hasPeer(l, wb.IP) })
	if l := b.waitPeers(t, func(l []peer) bool { return len(l) == 1 }); l[0].IP != wb.IP {
		t.Fatalf("замороженный видит не себя: %+v", l)
	}
	// Адрес и имя остались.
	st := s.adminState()
	var dev adminDevice
	for _, d := range st.Networks[0].Devices {
		if d.ID == "b" {
			dev = d
		}
	}
	if !dev.Frozen || dev.IP != wb.IP || dev.Host != wb.Host || !dev.Online {
		t.Fatalf("замороженное в панели: %+v", dev)
	}
	if _, m := dialFrom(t, addr, "b", wa.IP, 22); m.Op != "error" || !strings.Contains(m.Error, "заморожено") {
		t.Fatalf("замороженный звонит: %+v", m)
	}
	if _, m := dialFrom(t, addr, "a", wb.IP, 22); m.Op != "error" {
		t.Fatalf("замороженному звонят: %+v", m)
	}
	if !strings.Contains(eventTypes(s), "freeze") {
		t.Fatal("нет события freeze")
	}

	// Разморозили — снова видят друг друга.
	s.freeze(netID("k"), "b", false)
	if h := b.next(t, "hold"); h.Hold != "" {
		t.Fatalf("hold после разморозки: %+v", h)
	}
	a.waitPeers(t, func(l []peer) bool { return hasPeer(l, wb.IP) })
}

func TestЖдущийНеОтнимаетАдрес(t *testing.T) {
	s, _ := testServer(t)
	nid := "n"
	n := emptyNet(s, nid)
	old := claim(s, nid, n, "old", "дом", "198.19.7.7", "dom", s.now().Unix()-10)
	n.Approval = true
	stranger := &device{ID: "stranger", Name: "дом", Pending: true}
	n.Devices["stranger"] = stranger
	s.claimLocked(nid, n, stranger, "198.19.7.7", "dom", 1) // «держу с 1970 года»
	if old.IP != "198.19.7.7" || stranger.IP == old.IP {
		t.Fatalf("ждущее отняло адрес: old=%s stranger=%s", old.IP, stranger.IP)
	}
	if old.Host != "dom" || stranger.Host == "dom" {
		t.Fatalf("ждущее отняло имя: old=%s stranger=%s", old.Host, stranger.Host)
	}
}

func TestЦепочкаВыходаИзПанели(t *testing.T) {
	s, clk := testServer(t)
	s.connUser = func(net.Conn) string { return "u" }
	addr := serve(t, s)
	nid := netID("k")
	d, _ := helloConn(t, addr, msg{Net: "k", Device: "pc", Name: "pc", Since: 1})

	if _, err := s.setExit(nid, "pc", "fra.example:22", nil); err == nil || !strings.Contains(err.Error(), "новая версия") {
		t.Fatalf("без отчёта о серверах: %v", err)
	}
	writeMsg(d.c, msg{Op: "profiles", Profiles: []profRef{
		{Key: "FRA.example:22", Name: "Франкфурт", Active: true},
		{Key: "lon.example:2222", Name: "Лондон"},
		{Key: "плохой ключ"},
	}})
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.nets[nid].Devices["pc"].Profiles) == 2
	})
	for _, bad := range [][]string{{"fra.example:22"}, {"a:1", "b:2", "c:3"}, {"x"}} {
		if _, err := s.setExit(nid, "pc", "fra.example:22", bad); err == nil {
			t.Errorf("цепочка %v принята", bad)
		}
	}
	if _, err := s.setExit(nid, "pc", "nope.example:22", nil); err == nil {
		t.Error("вход через сервер без профиля принят")
	}
	o, err := s.setExit(nid, "pc", "fra.example:22", []string{"lon.example:2222", "war.example:22"})
	if err != nil {
		t.Fatal(err)
	}
	got := d.next(t, "exit").Exit
	if got == nil || got.Entry != "fra.example:22" || len(got.Via) != 2 || got.T != o.T || got.By == "" || got.ByID != s.self.ID {
		t.Fatalf("цепочка на устройстве: %+v", got)
	}

	// Устройство ещё не применило — при переподключении получит снова.
	d.c.Close()
	d, _ = helloConn(t, addr, msg{Net: "k", Device: "pc", Name: "pc", Since: 1})
	if again := d.next(t, "exit").Exit; again == nil || again.T != o.T {
		t.Fatalf("повтор: %+v", again)
	}
	// Применило (отчёт с тем же временем) — больше не повторяется.
	writeMsg(d.c, msg{Op: "profiles", Profiles: []profRef{{Key: "fra.example:22", Active: true,
		Via: []string{"lon.example:2222", "war.example:22"}, T: o.T, By: got.By}}})
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.exitDueLocked(s.nets[nid].Devices["pc"]) == nil
	})
	// Человек на устройстве поменял позже, а часы устройства впереди:
	// следующая правка панели всё равно новее его правки.
	writeMsg(d.c, msg{Op: "profiles", Profiles: []profRef{{Key: "fra.example:22", T: o.T + 3600}}})
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		p, _ := profileOf(s.nets[nid].Devices["pc"], "fra.example:22")
		return p.T == o.T+3600
	})
	clk.add(time.Minute)
	o2, err := s.setExit(nid, "pc", "fra.example:22", nil)
	if err != nil || o2.T <= o.T+3600 {
		t.Fatalf("новая правка панели не новее: %+v %v", o2, err)
	}
	if got := d.next(t, "exit").Exit; got.T != o2.T || len(got.Via) != 0 {
		t.Fatalf("очистка цепочки: %+v", got)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("не дождались")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestОдобрениеИЗаморозкаСохраняются(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := newServer(path)
	n := addNet(s, "n", map[string]string{"1": "u", "2": "u"})
	for _, d := range n.Devices {
		d.LastSeen = time.Now() // иначе при загрузке забудется как давнее
	}
	n.Approval = true
	n.Devices["1"].Pending = true
	n.Devices["2"].Frozen = true
	n.Devices["2"].ExitSet = &exitOrder{Entry: "a:22", T: 5}
	n.Devices["2"].Profiles = []profRef{{Key: "a:22", T: 4}}
	s.save()
	s2 := newServer(path)
	n2 := s2.nets["n"]
	if !n2.Approval || !n2.Devices["1"].Pending || !n2.Devices["2"].Frozen ||
		n2.Devices["2"].ExitSet == nil || len(n2.Devices["2"].Profiles) != 1 {
		t.Fatalf("после перезапуска: %+v %+v %+v", n2, n2.Devices["1"], n2.Devices["2"])
	}
}

func TestAdminHTTPОдобрениеЗаморозкаЦепочка(t *testing.T) {
	s, _ := testServer(t)
	n := addNet(s, "n", map[string]string{"pc": "u"})
	n.Devices["pc"].Profiles = []profRef{{Key: "fra:22"}}
	for _, c := range []struct{ path, body string }{
		{"/v1/net/approval", `{"net":"n","on":true}`},
		{"/v1/freeze", `{"net":"n","device":"pc","on":true}`},
		{"/v1/freeze", `{"net":"n","device":"pc","on":false}`},
		{"/v1/approve", `{"net":"n","device":"pc"}`},
		{"/v1/exit", `{"net":"n","device":"pc","entry":"fra:22","via":[]}`},
	} {
		if code, out := admin(t, s, http.MethodPost, c.path, c.body); code != 200 || out["ok"] != true {
			t.Errorf("%s: %d %v", c.path, code, out)
		}
	}
	if code, _ := admin(t, s, http.MethodPost, "/v1/freeze", `{"net":"n","device":"x","on":true}`); code != 400 {
		t.Errorf("заморозка чужого: %d", code)
	}
	_, st := admin(t, s, http.MethodGet, "/v1/state", "")
	net0 := st["networks"].([]any)[0].(map[string]any)
	dev := net0["devices"].([]any)[0].(map[string]any)
	if net0["approval"] != true || dev["exitSet"] == nil || dev["profiles"] == nil {
		t.Fatalf("state: %v", net0)
	}
	if got := eventTypes(s); got != "approval freeze unfreeze exit" {
		t.Fatalf("журнал: %s", got)
	}
}
