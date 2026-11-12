package tunnel

// ТЗ 6: клиент сети устройств живёт по кнопке, а не по пулу SSH. Тесты —
// на подделках: тестовый SSH-сервер, настоящий meshd из cmd/meshd и «ворота»
// перед SSH-сервером, которыми сервер включается и выключается.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sshtunnel/internal/events"
	"sshtunnel/internal/mesh"
)

// Тот же клиент на те же настройки, новый — на другие; Stop гасит; путь к
// серверу снимает только тот, кто его дал; без пути — быстрый отказ.
func TestMeshHostОдинКлиент(t *testing.T) {
	before := runtime.NumGoroutine()
	h := NewMeshHost(nil)
	cfg := mesh.Config{Key: "k", DeviceID: "d", Name: "n", Addr: "127.0.0.1:1", NoNATProbe: true, NoDirect: true}
	c1 := h.Start(cfg)
	if c2 := h.Start(cfg); c2 != c1 {
		t.Fatal("на те же настройки — второй клиент")
	}
	cfg.Name = "другое"
	c3 := h.Start(cfg)
	if c3 == c1 || h.Client() != c3 {
		t.Fatal("новые настройки не дали нового клиента")
	}

	owner1, owner2 := new(int), new(int)
	called := 0
	h.Attach(owner1, func(string, string) (net.Conn, error) { called++; return nil, errors.New("x") })
	h.Attach(owner2, func(string, string) (net.Conn, error) { called += 10; return nil, errors.New("y") })
	h.Detach(owner1) // прежний туннель уходит — путь нового остаётся
	if _, err := h.dial("tcp", "a:1"); err == nil || called != 10 {
		t.Fatalf("Detach чужого снял путь: err=%v called=%d", err, called)
	}
	h.Detach(owner2)
	start := time.Now()
	if _, err := h.dial("tcp", "a:1"); !errors.Is(err, ErrNoServer) || time.Since(start) > time.Second {
		t.Fatalf("без пути: %v за %v", err, time.Since(start))
	}

	h.Stop()
	h.Stop()
	if h.Client() != nil || h.Running() {
		t.Fatal("Stop не погасил клиента")
	}
	waitGoroutines(t, before)
}

// waitGoroutines — горутин не больше, чем было (с небольшим запасом на
// рантайм), иначе утечка.
func waitGoroutines(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+2 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("утечка горутин: было %d, стало %d\n%s", before, runtime.NumGoroutine(), buf[:n])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// gate — «интернет до сервера»: открыты — соединения идут к SSH-серверу,
// закрыты — обрываются сразу, а уже идущие рвутся.
type gate struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	open   bool
	conns  []net.Conn
}

func newGate(t *testing.T, target string, open bool) *gate {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &gate{ln: ln, target: target, open: open}
	t.Cleanup(func() { ln.Close(); g.set(false) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			g.mu.Lock()
			ok := g.open
			if ok {
				g.conns = append(g.conns, c)
			}
			g.mu.Unlock()
			if !ok {
				c.Close()
				continue
			}
			go func() {
				up, err := net.Dial("tcp", g.target)
				if err != nil {
					c.Close()
					return
				}
				g.mu.Lock()
				g.conns = append(g.conns, up)
				g.mu.Unlock()
				go func() { io.Copy(up, c); up.Close() }()
				io.Copy(c, up)
				c.Close()
			}()
		}
	}()
	return g
}

func (g *gate) set(open bool) {
	g.mu.Lock()
	g.open = open
	var kill []net.Conn
	if !open {
		kill, g.conns = g.conns, nil
	}
	g.mu.Unlock()
	for _, c := range kill {
		c.Close()
	}
}

func (g *gate) port() int { return g.ln.Addr().(*net.TCPAddr).Port }

// startTestMeshdSTUN — meshd с проверкой NAT: без неё прямых соединений нет.
func startTestMeshdSTUN(t *testing.T) string {
	t.Helper()
	bin, err := buildMeshdOnce()
	if err != nil {
		t.Skipf("не удалось собрать meshd: %v", err)
	}
	cmd := exec.Command(bin, "-listen", "127.0.0.1:0", "-state", filepath.Join(t.TempDir(), "state.json"), "-stun", "0,0")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if i := strings.Index(sc.Text(), "слушает "); i >= 0 {
				found <- strings.TrimSpace(sc.Text()[i+len("слушает "):])
			}
		}
	}()
	select {
	case addr := <-found:
		return addr
	case <-time.After(10 * time.Second):
		t.Fatal("meshd не поднялся")
		return ""
	}
}

// device — одно «устройство»: свой хозяин сети и туннель к sshPort.
type device struct {
	t       *testing.T
	host    *MeshHost
	cfg     mesh.Config
	keyPath string
	known   string
	tun     *Tunnel
	socks   string
}

func newDevice(t *testing.T, keyPath string, cfg mesh.Config) *device {
	return &device{t: t, host: NewMeshHost(nil), cfg: cfg, keyPath: keyPath,
		known: filepath.Join(t.TempDir(), "known_hosts")}
}

// up — «кнопка сети включена» и туннель к 127.0.0.1:sshPort.
func (d *device) up(sshPort int, optional bool) error {
	d.host.Start(d.cfg)
	d.socks = fmt.Sprintf("127.0.0.1:%d", freePort(d.t))
	cfg := d.cfg
	d.tun = New(Config{
		Host: "127.0.0.1", SSHPort: sshPort, User: "test", KeyPath: d.keyPath,
		SocksAddr: d.socks, PoolSize: 1, KnownHostsPath: d.known,
		DialTimeout: 2 * time.Second, Mesh: &cfg, MeshHost: d.host, ServerOptional: optional,
	}, events.NewBus())
	if err := d.tun.Start(); err != nil {
		return err
	}
	tun := d.tun
	d.t.Cleanup(tun.Stop)
	return nil
}

func (d *device) peer(ip string) mesh.Peer {
	c := d.host.Client()
	if c == nil {
		return mesh.Peer{}
	}
	for _, p := range c.Status().Peers {
		if p.IP.String() == ip {
			return p
		}
	}
	return mesh.Peer{}
}

func eventually(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Перезагрузка без интернета: тёплый кэш, сервера нет — сеть устройств
// работает, сосед достаётся напрямую; сервер появился — клиент сам вошёл к
// нему без перезапуска; новый пул (переход на запасной) — тот же клиент;
// кнопка выключена — клиента нет.
func TestСетьУстройствБезСервераПриСтарте(t *testing.T) {
	if testing.Short() {
		t.Skip("долгий: прямые соединения и паузы клиента")
	}
	meshd := startTestMeshdSTUN(t)
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	srv := newTestSSHServer(t, pub)
	_, srvPortStr, _ := net.SplitHostPort(srv.addr)
	srvPort, _ := strconv.Atoi(srvPortStr)
	g := newGate(t, srv.addr, true)

	key := mesh.NewKey()
	base := mesh.Config{Key: key, AllowIncoming: true, Addr: meshd, Via: "127.0.0.1", NoNATProbe: true}
	ac, bc := base, base
	ac.DeviceID, ac.Name, ac.StateDir = mesh.NewDeviceID(), "a", t.TempDir()
	bc.DeviceID, bc.Name, bc.StateDir = mesh.NewDeviceID(), "b", t.TempDir()
	a, b := newDevice(t, keyPath, ac), newDevice(t, keyPath, bc)
	t.Cleanup(a.host.Stop)
	t.Cleanup(b.host.Stop)
	if err := a.up(srvPort, false); err != nil {
		t.Fatal(err)
	}
	if err := b.up(g.port(), false); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() bool {
		return a.host.Client().Status().State == "online" && b.host.Client().Status().State == "online"
	}, "не вошли в сеть")
	aIP := a.host.Client().Status().Self.IP.String()
	bIP := b.host.Client().Status().Self.IP.String()
	eventually(t, 20*time.Second, func() bool { return a.peer(bIP).Direct && b.peer(aIP).Direct },
		"прямое соединение не установилось")
	time.Sleep(time.Second) // список с адресами — на диск

	svc := echoServer(t) // служба «на устройстве a»

	// Интернет пропал, b перезагрузился: сервер недоступен с самого старта.
	g.set(false)
	b.tun.Stop()
	b.host.Stop()
	if b.host.Client() != nil {
		t.Fatal("Stop не погасил клиента")
	}
	b.host = NewMeshHost(nil) // новый процесс — новый хозяин, кэш тот же
	t.Cleanup(b.host.Stop)
	if err := b.up(g.port(), true); err != nil {
		t.Fatalf("без сервера туннель не поднялся: %v", err)
	}
	if b.tun.ServerReached() {
		t.Fatal("сервера не было, а ServerReached")
	}
	if st := b.tun.State(); st != events.StateReconnecting {
		t.Fatalf("состояние туннеля без сервера %q", st)
	}
	client := b.host.Client()
	eventually(t, 20*time.Second, func() bool { return b.peer(aIP).Direct && b.peer(aIP).Online },
		"без сервера сосед не нашёлся напрямую")
	if st := client.Status().State; st != "local" {
		t.Fatalf("без сервера состояние %q, ждали local", st)
	}
	c := socks5Connect(t, b.socks, "a.mesh", svc.Port, true)
	assertHTTPBody(t, c, "a.mesh", "/hello", "привет от a.mesh")
	c.Close()

	// Сервер появился — тот же клиент входит к нему сам.
	g.set(true)
	eventually(t, 45*time.Second, func() bool { return b.tun.ServerUp() && client.Status().State == "online" },
		"сервер вернулся, а клиент к нему не вошёл")
	if b.host.Client() != client {
		t.Fatal("клиент пересоздан")
	}

	// Новый пул (как после перехода на запасной): старый туннель гаснет,
	// новый подключается — клиент тот же, второго нет.
	old := b.tun
	if err := b.up(g.port(), false); err != nil {
		t.Fatal(err)
	}
	old.Stop()
	if b.host.Client() != client {
		t.Fatal("после смены пула другой клиент")
	}
	eventually(t, 45*time.Second, func() bool { return client.Status().State == "online" },
		"после смены пула клиент не вернулся к серверу")

	// Кнопка выключена.
	b.host.Stop()
	if b.tun.Mesh() != nil {
		t.Fatal("кнопка выключена, а клиент есть")
	}
	if _, err := b.tun.DialApp(net.JoinHostPort("a.mesh", strconv.Itoa(svc.Port))); err == nil {
		t.Fatal("кнопка выключена, а соседа видно")
	}
}

// Без сервера и без сети устройств туннель с ServerOptional всё равно
// поднимается, а подключается к серверу, когда тот появится.
func TestServerOptionalПодключаетсяПозже(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	srv := newTestSSHServer(t, pub)
	g := newGate(t, srv.addr, false)
	tun := New(Config{
		Host: "127.0.0.1", SSHPort: g.port(), User: "test", KeyPath: keyPath,
		SocksAddr: fmt.Sprintf("127.0.0.1:%d", freePort(t)), PoolSize: 2,
		KnownHostsPath: filepath.Join(dir, "known_hosts"), DialTimeout: time.Second,
		ServerOptional: true,
	}, events.NewBus())
	start := time.Now()
	if err := tun.Start(); err != nil {
		t.Fatal(err)
	}
	defer tun.Stop()
	if time.Since(start) > time.Second {
		t.Fatalf("старт без сервера ждал %v", time.Since(start))
	}
	eventually(t, 5*time.Second, func() bool { return tun.ServerErr() != nil }, "нет причины, почему сервера нет")
	g.set(true)
	tun.Kick()
	eventually(t, 10*time.Second, func() bool { return tun.ServerUp() && tun.State() == events.StateConnected },
		"сервер появился, а пул не подключился")
	if !tun.ServerReached() || tun.ServerErr() != nil {
		t.Fatal("после подключения остались признаки «нет сервера»")
	}
}
