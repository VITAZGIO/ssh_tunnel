package core

// Цепочка выхода на телефоне (ТЗ 20): пока сервер цепочки недоступен,
// трафик не уходит ни через входной сервер, ни напрямую.

import (
	"net"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"sshtunnel/internal/events"
	"sshtunnel/internal/tunnel"
)

func TestЦепочкаБезУтечкиПокаСерверЛежит(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	entry := newTestSSHServer(t, pub)
	host, portStr, _ := net.SplitHostPort(entry.addr)
	sshPort, _ := strconv.Atoi(portStr)

	// «Сайт»: считает, сколько соединений до него дошло.
	var reached atomic.Int32
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
			reached.Add(1)
			c.Close()
		}
	}()

	tun := tunnel.New(tunnel.Config{
		Host: host, SSHPort: sshPort, User: "test", KeyPath: keyPath,
		KnownHostsPath: filepath.Join(dir, "known_hosts"),
		PoolSize:       1,
		LocalViaTunnel: true, // цель на 127.0.0.1 — пусть идёт «через сервер», как сайт
		FakeIP:         true, // как на телефоне
		// Сервер цепочки: порт, на котором никто не слушает.
		Exit: []tunnel.Hop{{Name: "B", Host: "127.0.0.1", SSHPort: freePort(t), User: "test", KeyPath: keyPath}},
	}, events.NewBus())
	if err := tun.Start(); err != nil {
		t.Fatalf("входной сервер не поднялся: %v", err)
	}
	t.Cleanup(tun.Stop)

	// Каждая попытка ждёт цепочку несколько секунд — одной по адресу и одной
	// по имени достаточно.
	if c, err := tun.Dial("tcp", ln.Addr().String()); err == nil {
		c.Close()
		t.Fatal("соединение ушло, хотя сервер цепочки лежит")
	}
	// По имени (подставные адреса: DNS решает последний сервер) — тоже нет.
	if c, err := tun.Dial("tcp", "localhost:"+strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)); err == nil {
		c.Close()
		t.Fatal("соединение по имени ушло мимо цепочки")
	}
	time.Sleep(200 * time.Millisecond)
	if n := reached.Load(); n != 0 {
		t.Fatalf("до сайта дошло %d соединений мимо цепочки", n)
	}
	if st := tun.ExitStatus(); st.Up || st.Err == nil {
		t.Fatalf("цепочка: %+v — ждали «лежит» с ошибкой", st)
	}
}
