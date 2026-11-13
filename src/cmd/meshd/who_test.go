package main

import (
	"bufio"
	"encoding/json"
	"net"
	"strconv"
	"testing"
	"time"
)

// askWho — служба спрашивает meshd, кто к ней пришёл.
func askWho(t *testing.T, addr string, port, src int) msg {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeMsg(c, msg{Op: "who", Port: port, Src: src})
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m msg
	json.Unmarshal(line, &m)
	return m
}

func TestКтоЗвонитСлужбеСервера(t *testing.T) {
	s, _ := testServer(t)
	s.connUser = func(net.Conn) string { return "tun_pc" }
	owner := true
	s.sameOwner = func(net.Conn, int) bool { return owner }

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
	addr := ln.Addr().String()

	// Служба на сервере: на каждое соединение спрашивает meshd и пишет ответ.
	svc, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	port := svc.Addr().(*net.TCPAddr).Port
	got := make(chan msg, 4)
	go func() {
		for {
			c, err := svc.Accept()
			if err != nil {
				return
			}
			src := c.RemoteAddr().(*net.TCPAddr).Port
			got <- askWho(t, addr, port, src)
			c.Close()
		}
	}()

	ctrl, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	writeMsg(ctrl, msg{Op: "hello", Net: "ключ", Device: "pc", Name: "Рабочий ПК"})
	ctrl.SetReadDeadline(time.Now().Add(5 * time.Second))
	bufio.NewReader(ctrl).ReadBytes('\n')

	call, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer call.Close()
	writeMsg(call, msg{Op: "dial", Net: "ключ", Device: "pc", To: selfIP, Port: port})
	select {
	case m := <-got:
		if !m.OK || m.Op != "whois" || m.Net != netID("ключ") || m.Device != "pc" || m.User != "tun_pc" || m.IP == "" || m.Name != "Рабочий ПК" {
			t.Fatalf("whois: %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("служба не дождалась ответа")
	}

	// Соединение не из meshd: такого src meshd не открывал.
	other, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	select {
	case m := <-got:
		if m.OK || m.Op != "error" {
			t.Fatalf("чужое соединение опознано: %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("нет ответа на чужое соединение")
	}

	// Спрашивает не владелец порта службы — отказ сразу.
	owner = false
	if m := askWho(t, addr, port, 1234); m.OK {
		t.Fatalf("ответили не владельцу: %+v", m)
	}
}

func TestListenUID(t *testing.T) {
	table := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:BAC6 00000000:0000 0A 00000000:00000000 00:00000000 00000000   997        0 1 1 0 100 0 0 10 0
   1: 0100007F:BAC6 0100007F:D431 01 00000000:00000000 00:00000000 00000000   999        0 2 1 0 20 4 30 10 -1
`
	if uid, ok := listenUID(table, 0xBAC6); !ok || uid != "997" {
		t.Fatalf("listenUID: %q %v", uid, ok)
	}
	if _, ok := listenUID(table, 80); ok {
		t.Fatal("нашёлся порт, которого нет")
	}
}
