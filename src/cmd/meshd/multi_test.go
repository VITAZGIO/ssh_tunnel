package main

import (
	"bufio"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Несколько серверов без главного (ТЗ 12): споры за адрес и имя решаются
// одинаково на любом сервере, без связи между серверами.

// claim — устройство dev называет серверу s адрес ip с давностью since.
func claim(s *server, nid string, n *network, dev, name, ip, host string, since int64) *device {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := n.Devices[dev]
	if d == nil {
		d = &device{ID: dev}
		n.Devices[dev] = d
	}
	d.Name = name
	s.claimLocked(nid, n, d, ip, host, since)
	return d
}

func emptyNet(s *server, nid string) *network {
	n := &network{Devices: map[string]*device{}}
	s.nets[nid] = n
	return n
}

func TestСпорЗаАдресОдинаковНаВсехСерверах(t *testing.T) {
	const ip = "198.19.7.7"
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).Unix()
	// На первом сервере сначала пришло старое устройство, на втором —
	// новое: итог должен совпасть.
	s1, _ := testServer(t)
	s2, _ := testServer(t)
	n1, n2 := emptyNet(s1, "net"), emptyNet(s2, "net")

	claim(s1, "net", n1, "old", "ПК", ip, "", now-1000)
	claim(s1, "net", n1, "young", "Ноутбук", ip, "", now-10)

	claim(s2, "net", n2, "young", "Ноутбук", ip, "", now-10)
	claim(s2, "net", n2, "old", "ПК", ip, "", now-1000)

	for i, n := range []*network{n1, n2} {
		if got := n.Devices["old"].IP; got != ip {
			t.Fatalf("сервер %d: адрес у старого %s, ждали %s", i+1, got, ip)
		}
		if got := n.Devices["young"].IP; got == ip || !deviceAddr(got) {
			t.Fatalf("сервер %d: у проигравшего %q", i+1, got)
		}
	}
	if a, b := n1.Devices["young"].IP, n2.Devices["young"].IP; a != b {
		t.Fatalf("проигравший получил разные адреса: %s и %s", a, b)
	}
}

func TestСпорЗаАдресРавнаяДавностьМеньшийID(t *testing.T) {
	s, clk := testServer(t)
	n := emptyNet(s, "net")
	since := clk.now().Unix() - 50
	claim(s, "net", n, "b", "b", "198.19.1.1", "", since)
	claim(s, "net", n, "a", "a", "198.19.1.1", "", since)
	if n.Devices["a"].IP != "198.19.1.1" || n.Devices["b"].IP == "198.19.1.1" {
		t.Fatalf("при равной давности адрес должен достаться меньшему id: a=%s b=%s", n.Devices["a"].IP, n.Devices["b"].IP)
	}
}

func TestНовоеУстройствоОдинАдресНаРазныхСерверах(t *testing.T) {
	s1, _ := testServer(t)
	s2, _ := testServer(t)
	a := claim(s1, "net", emptyNet(s1, "net"), "dev", "Телефон", "", "", 0)
	b := claim(s2, "net", emptyNet(s2, "net"), "dev", "Телефон", "", "", 0)
	if a.IP != b.IP || a.IP != addrSeq("net", "dev", 0) {
		t.Fatalf("новое устройство: %s и %s, ждали %s", a.IP, b.IP, addrSeq("net", "dev", 0))
	}
	if a.Host != "telefon" || b.Host != "telefon" {
		t.Fatalf("имя %q / %q", a.Host, b.Host)
	}
}

func TestСпорЗаИмяХвостИзID(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).Unix()
	var hosts [2][2]string
	for i := 0; i < 2; i++ {
		s, _ := testServer(t)
		n := emptyNet(s, "net")
		if i == 0 {
			claim(s, "net", n, "old", "Ноутбук", "198.19.1.1", "", now-100)
			claim(s, "net", n, "new", "Ноутбук", "198.19.1.2", "", now-5)
		} else {
			claim(s, "net", n, "new", "Ноутбук", "198.19.1.2", "", now-5)
			claim(s, "net", n, "old", "Ноутбук", "198.19.1.1", "", now-100)
		}
		hosts[i] = [2]string{n.Devices["old"].Host, n.Devices["new"].Host}
	}
	if hosts[0] != hosts[1] {
		t.Fatalf("имена разошлись: %v и %v", hosts[0], hosts[1])
	}
	if hosts[0][0] != "noutbuk" || !strings.HasPrefix(hosts[0][1], "noutbuk-") {
		t.Fatalf("имена %v: старому — noutbuk, новому — с хвостом", hosts[0])
	}
	// Хвост устройство приносит с собой — и сохраняет его.
	s, _ := testServer(t)
	d := claim(s, "net", emptyNet(s, "net"), "new", "Ноутбук", "198.19.1.2", hosts[0][1], now-5)
	if d.Host != hosts[0][1] {
		t.Fatalf("своё имя с хвостом не принято: %q", d.Host)
	}
	// Чужое имя не от своего имени — нельзя.
	d = claim(s, "net", s.nets["net"], "new", "Ноутбук", "198.19.1.2", "router", now-5)
	if d.Host != "noutbuk" {
		t.Fatalf("приняли чужое имя: %q", d.Host)
	}
}

func TestСтарыйКлиентДержитАдрес(t *testing.T) {
	s, _ := testServer(t)
	n := emptyNet(s, "net")
	n.Devices["pc"] = &device{ID: "pc", Name: "pc", IP: "198.19.0.5", FirstSeen: s.now().Add(-time.Hour)}
	d := claim(s, "net", n, "pc", "pc", "", "", 0)
	if d.IP != "198.19.0.5" {
		t.Fatalf("старый клиент без заявки потерял адрес: %s", d.IP)
	}
	// Новый клиент с тем же адресом и более поздней давностью его не
	// отберёт: старый держит адрес с первого появления.
	x := claim(s, "net", n, "x", "x", "198.19.0.5", "", s.now().Unix()-60)
	if x.IP == "198.19.0.5" || n.Devices["pc"].IP != "198.19.0.5" {
		t.Fatalf("адрес старого устройства отобран: pc=%s x=%s", n.Devices["pc"].IP, x.IP)
	}
}

func TestИмяИзПанелиПобеждаетПоследнее(t *testing.T) {
	s, clk := testServer(t)
	d := &device{ID: "d"}
	s.aliasLocked(d, "Старое", clk.now().Unix()-100)
	s.aliasLocked(d, "Новое", clk.now().Unix()-10)
	s.aliasLocked(d, "Опоздавшее", clk.now().Unix()-50)
	s.aliasLocked(d, "Из будущего", clk.now().Unix()+3600)
	if d.Alias != "Новое" {
		t.Fatalf("имя %q, ждали «Новое»", d.Alias)
	}
}

func TestАдресСервера(t *testing.T) {
	fresh := newServer("")
	if fresh.self.ID == "" || fresh.self.IP == legacySelfIP || !serverBlock.Contains(mustAddr(fresh.self.IP)) {
		t.Fatalf("новый сервер: id %q, адрес %q", fresh.self.ID, fresh.self.IP)
	}
	// Обновлённый главный (есть прежнее состояние без id) оставляет .254.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte(`{}`), 0o600)
	old := newServer(path)
	if old.self.IP != legacySelfIP {
		t.Fatalf("обновлённый сервер получил %s, ждали %s", old.self.IP, legacySelfIP)
	}
	old.save()
	again := newServer(path)
	if again.self.ID != old.self.ID || again.self.IP != legacySelfIP {
		t.Fatalf("id/адрес не сохранились: %+v", again.self)
	}
	if err := again.setSelfIP("198.19.0.1"); err == nil {
		t.Fatal("адрес устройства принят как адрес сервера")
	}
}

// serve запускает s на случайном порту.
func serve(t *testing.T, s *server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	return ln.Addr().String()
}

type wireConn struct {
	c net.Conn
	r *bufio.Reader
}

func helloConn(t *testing.T, addr string, m msg) (*wireConn, msg) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	m.Op = "hello"
	writeMsg(c, m)
	w := &wireConn{c, bufio.NewReader(c)}
	return w, w.next(t, "welcome")
}

// next — следующая строка с операцией op (ping и peers пропускаются).
func (w *wireConn) next(t *testing.T, op string) msg {
	t.Helper()
	w.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := w.r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("ждали %s: %v", op, err)
		}
		var m msg
		json.Unmarshal(line, &m)
		if m.Op == op {
			return m
		}
	}
}

func TestПроводНовыйКлиентПолучаетReaddress(t *testing.T) {
	s, _ := testServer(t)
	addr := serve(t, s)
	now := time.Now().Unix()
	young, wy := helloConn(t, addr, msg{Net: "k", Device: "young", Name: "y", IP: "198.19.9.9", Since: now - 5})
	if wy.IP != "198.19.9.9" || wy.Server == nil || wy.Server.ID != s.self.ID || wy.Server.IP != s.self.IP {
		t.Fatalf("welcome: %+v", wy)
	}
	_, wo := helloConn(t, addr, msg{Net: "k", Device: "old", Name: "o", IP: "198.19.9.9", Since: now - 500})
	if wo.IP != "198.19.9.9" {
		t.Fatalf("старшее устройство не получило свой адрес: %s", wo.IP)
	}
	ra := young.next(t, "readdress")
	if ra.IP == "" || ra.IP == "198.19.9.9" {
		t.Fatalf("readdress: %+v", ra)
	}
	// Устройство называет новый адрес (op self) — сервер подтверждает.
	writeMsg(young.c, msg{Op: "self", IP: ra.IP, Host: ra.Host, Since: ra.Since})
	if got := young.next(t, "self"); got.IP != ra.IP {
		t.Fatalf("self: %+v", got)
	}
	// Отчёт о других серверах — в списке устройств и в панели.
	writeMsg(young.c, msg{Op: "on", On: []serverSeen{{ID: "london", Name: "London", Pol: "other"}, {ID: s.self.ID}}})
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := s.adminState()
		if len(st.Peers) == 1 && st.Peers[0].ID == "london" && !st.Peers[0].Same {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("панель не узнала о другом сервере: %+v", st.Peers)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestНетОбщегоСервераВОтказе(t *testing.T) {
	s, _ := testServer(t)
	s.connUser = func(net.Conn) string { return "u" }
	addr := serve(t, s)
	a, _ := helloConn(t, addr, msg{Net: "k", Device: "a", Name: "a", Since: 1})
	b, wb := helloConn(t, addr, msg{Net: "k", Device: "b", Name: "b", Since: 1})
	writeMsg(b.c, msg{Op: "on", On: []serverSeen{{ID: "london", Name: "London"}}})
	time.Sleep(100 * time.Millisecond)
	b.c.Close()
	_ = a
	time.Sleep(100 * time.Millisecond)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeMsg(c, msg{Op: "dial", Net: "k", Device: "a", To: wb.IP, Port: 80})
	r := &wireConn{c, bufio.NewReader(c)}
	got := r.next(t, "error")
	if !strings.Contains(got.Error, "нет общего сервера") || !strings.Contains(got.Error, "London") {
		t.Fatalf("отказ: %q", got.Error)
	}
}

func mustAddr(s string) netip.Addr { a, _ := netip.ParseAddr(s); return a }
