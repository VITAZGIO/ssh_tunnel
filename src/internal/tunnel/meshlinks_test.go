package tunnel

// ТЗ 12: дополнительные серверы сети устройств — лёгкие SSH-соединения
// (MeshLinks) рядом с пулом туннеля.

import (
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"sshtunnel/internal/mesh"
)

// Путь к дополнительному серверу: даётся до и после старта клиента,
// снимается DetachExtra; без пути — быстрый отказ.
func TestMeshHostДополнительныеСерверы(t *testing.T) {
	h := NewMeshHost(nil)
	var calls atomic.Int32
	h.AttachExtra("london", "london.example", func(string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("нет")
	})
	c := h.Start(mesh.Config{Key: "k", DeviceID: "d", Name: "n", Addr: "127.0.0.1:1", Via: "fra", NoNATProbe: true, NoDirect: true})
	defer h.Stop()
	eventually(t, 5*time.Second, func() bool { return len(c.Status().Servers) == 2 && calls.Load() > 0 },
		"клиент не узнал о дополнительном сервере")
	h.AttachExtra("ams", "ams.example", func(string, string) (net.Conn, error) { return nil, errors.New("нет") })
	eventually(t, 5*time.Second, func() bool { return len(c.Status().Servers) == 3 }, "сервер, добавленный на ходу, не виден")
	h.DetachExtra("london")
	eventually(t, 5*time.Second, func() bool { return len(c.Status().Servers) == 2 }, "DetachExtra не убрал сервер")
	if got := h.Extras(); len(got) != 1 || got[0] != "ams" {
		t.Fatalf("Extras: %v", got)
	}
}

// Лёгкое соединение входит по SSH со своим ключом и проверкой ключа
// сервера и доходит до meshd. Здесь оба пути ведут в один meshd — клиент
// должен узнать в нём тот же сервер и не выбить своё соединение.
func TestMeshLinksЛёгкоеСоединение(t *testing.T) {
	meshd := startTestMeshdSTUN(t)
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	srv := newTestSSHServer(t, pub)
	_, portStr, _ := net.SplitHostPort(srv.addr)
	port, _ := strconv.Atoi(portStr)

	cfg := mesh.Config{Key: mesh.NewKey(), DeviceID: mesh.NewDeviceID(), Name: "a", Addr: meshd,
		Via: "127.0.0.1", NoNATProbe: true, NoDirect: true}
	d := newDevice(t, keyPath, cfg)
	t.Cleanup(d.host.Stop)
	if err := d.up(port, false); err != nil {
		t.Fatal(err)
	}
	links := NewMeshLinks(d.host, nil)
	t.Cleanup(links.Stop)
	links.Set([]MeshServer{{Key: "second", Name: "второй", Host: "127.0.0.1", SSHPort: port, User: "test",
		KeyPath: keyPath, KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts")}})
	c := d.host.Client()
	eventually(t, 15*time.Second, func() bool {
		st := c.Status().Servers
		if len(st) != 2 {
			return false
		}
		return (st[0].State == "online" && st[1].State == "duplicate") || (st[1].State == "online" && st[0].State == "duplicate")
	}, "второй путь к тому же meshd не узнан")
	if c.Status().State != "online" {
		t.Fatal("сеть устройств выпала")
	}
	links.Set(nil)
	eventually(t, 5*time.Second, func() bool { return len(c.Status().Servers) == 1 }, "Set(nil) не убрал сервер")
}
