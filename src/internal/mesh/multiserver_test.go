package mesh

// Несколько серверов без главного (ТЗ 12): настоящие meshd, по одному на
// «сервер», устройство держит связь со всеми сразу.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type testMeshd struct {
	addr, sock string
	cmd        *exec.Cmd
}

// startMeshds поднимает n независимых meshd со входом для панели.
func startMeshds(t *testing.T, n int) []*testMeshd {
	t.Helper()
	bin, err := buildMeshdOnce()
	if err != nil {
		t.Skipf("не удалось собрать meshd: %v", err)
	}
	var out []*testMeshd
	for i := 0; i < n; i++ {
		dir, err := os.MkdirTemp("", "md")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		sock := filepath.Join(dir, "a.sock")
		addr, cmd := runMeshdCmd(t, bin, filepath.Join(dir, "state.json"), "-admin", sock, "-stun", "")
		out = append(out, &testMeshd{addr: addr, sock: sock, cmd: cmd})
	}
	return out
}

func dialTo(addr string) Dialer {
	return func(network, _ string) (net.Conn, error) { return net.Dial(network, addr) }
}

// startMulti — клиент, который знает серверы srv (первый — основной).
func startMulti(t *testing.T, srv []*testMeshd, cfg Config) *Client {
	t.Helper()
	if cfg.DeviceID == "" {
		cfg.DeviceID = NewDeviceID()
	}
	cfg.Via = "s0"
	cfg.NoDirect = true // проверяем путь через серверы
	c := New(cfg, dialTo(srv[0].addr), nil)
	for i, s := range srv[1:] {
		c.AddServer("s"+strconv.Itoa(i+1), "s"+strconv.Itoa(i+1), dialTo(s.addr))
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx)
	waitFor(t, func() bool {
		for _, s := range c.Status().Servers {
			if s.State != "online" {
				return false
			}
		}
		return true
	}, "клиент "+cfg.Name+" не подключился ко всем серверам")
	return c
}

func peerByHost(c *Client, host string) (Peer, int) {
	var found Peer
	n := 0
	for _, p := range c.Status().Peers {
		if p.Host == host {
			found = p
			n++
		}
	}
	return found, n
}

func TestДваСервераОдноУстройство(t *testing.T) {
	srv := startMeshds(t, 2)
	key := NewKey()
	a := startMulti(t, srv, Config{Key: key, Name: "a"})
	b := startMulti(t, srv, Config{Key: key, Name: "b", AllowIncoming: true})

	// В списке у a устройство b одно, на связи через оба сервера.
	waitFor(t, func() bool { p, n := peerByHost(a, "b"); return n == 1 && len(p.Servers) == 2 }, "b не одно / не через оба сервера")
	// У обоих серверов — тот же адрес и то же имя.
	bIP := b.Status().Self.IP.String()
	for i, s := range srv {
		_, d, ok := findDev(getState(t, adminClient(s.sock)), "b")
		if !ok || d.IP != bIP || d.Host != "b" {
			t.Fatalf("сервер %d: %+v, ждали адрес %s", i, d, bIP)
		}
	}
	// Отчёт о других серверах: панель первого видит второй.
	var peers struct {
		Peers []struct {
			ID   string `json:"id"`
			Same bool   `json:"same"`
		} `json:"peers"`
		Self struct {
			ID string `json:"id"`
		} `json:"self"`
	}
	waitFor(t, func() bool {
		getJSON(t, adminClient(srv[0].sock), "/v1/state", &peers)
		return len(peers.Peers) == 1 && peers.Peers[0].Same
	}, "панель не видит второй сервер сети")
}

func TestПадениеОдногоСервераИзТрёх(t *testing.T) {
	srv := startMeshds(t, 3)
	key := NewKey()
	a := startMulti(t, srv, Config{Key: key, Name: "a"})
	b := startMulti(t, srv, Config{Key: key, Name: "b", AllowIncoming: true})
	port := echoService(t, "b")
	waitFor(t, func() bool { p, _ := peerByHost(a, "b"); return len(p.Servers) == 3 }, "b не на связи через три сервера")
	target := "b.mesh:" + strconv.Itoa(port)
	conn, err := a.DialPeer(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := ask(t, conn, "раз"); got != "b: раз" {
		t.Fatalf("ответ %q", got)
	}

	// Сервер, через который шло соединение (первый), падает.
	srv[0].cmd.Process.Kill()
	srv[0].cmd.Wait()
	start := time.Now()
	for {
		conn, err := a.DialPeer(target)
		if err == nil {
			if got := ask(t, conn, "два"); got != "b: два" {
				t.Fatalf("ответ %q", got)
			}
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("через оставшиеся серверы не соединилось за 5 с: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, c := range []*Client{a, b} {
		if st := c.Status(); st.State != "online" {
			t.Fatalf("сеть пропала: %+v", st.State)
		}
	}
	waitFor(t, func() bool { p, _ := peerByHost(a, "b"); return len(p.Servers) == 2 }, "упавший сервер не убран из путей")
}

func TestНетОбщегоСервера(t *testing.T) {
	srv := startMeshds(t, 2)
	key := NewKey()
	a := startMulti(t, srv[:1], Config{Key: key, Name: "a"})
	// У b основной — второй сервер, первый — дополнительный.
	b := startMulti(t, []*testMeshd{srv[1], srv[0]}, Config{Key: key, Name: "b", AllowIncoming: true})
	waitFor(t, func() bool { p, _ := peerByHost(a, "b"); return p.Online }, "a не видит b")
	// Первый сервер должен узнать, что b на связи и со вторым.
	waitFor(t, func() bool {
		var st struct {
			Networks []struct {
				Devices []struct {
					Name string `json:"name"`
				} `json:"devices"`
			} `json:"networks"`
		}
		getJSON(t, adminClient(srv[1].sock), "/v1/state", &st)
		return len(st.Networks) == 1
	}, "второй сервер не видит сеть")
	time.Sleep(300 * time.Millisecond) // отчёт op on доходит до первого
	b.RemoveServer("s1")               // b уходит с первого сервера
	waitFor(t, func() bool { p, _ := peerByHost(a, "b"); return !p.Online && p.NoCommon }, "a не понял, что общего сервера нет")
	_, err := a.DialPeer("b.mesh:80")
	if err == nil || !strings.Contains(err.Error(), "нет общего сервера") {
		t.Fatalf("ошибка %v, ждали «нет общего сервера»", err)
	}
}

func getJSON(t *testing.T, c *http.Client, path string, v any) {
	t.Helper()
	resp, err := c.Get("http://meshd" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// Два пути к одному meshd (старый побочный сервер — проброс к главному, или
// один сервер в двух профилях): второе соединение не выбивает первое.
func TestДваПутиКОдномуMeshd(t *testing.T) {
	srv := startMeshds(t, 1)
	key := NewKey()
	a := New(Config{Key: key, DeviceID: NewDeviceID(), Name: "a", Via: "s0", NoDirect: true}, dialTo(srv[0].addr), nil)
	a.AddServer("s1", "s1", dialTo(srv[0].addr))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	waitFor(t, func() bool {
		st := a.Status().Servers
		return len(st) == 2 && oneDuplicate(st)
	}, "второй путь к тому же meshd не узнан")
	time.Sleep(time.Second)
	_, d, _ := findDev(getState(t, adminClient(srv[0].sock)), "a")
	if !d.Online {
		t.Fatal("устройство выпало из сети")
	}
}

// Старый meshd (версия 3, из истории репозитория) с новыми клиентами: оба
// пути к нему — одно соединение, устройства видят друг друга, адрес даёт он.
func TestСтарыйMeshdСНовымиКлиентами(t *testing.T) {
	dir := t.TempDir()
	src, err := exec.Command("git", "show", oldMeshdCommit+":src/cmd/meshd/main.go").Output()
	if err != nil {
		t.Skip("нет истории git (неглубокая копия) — старый meshd не собрать")
	}
	os.WriteFile(filepath.Join(dir, "main.go"), src, 0o600)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module oldmeshd\n\ngo 1.22\n"), 0o600)
	bin := filepath.Join(dir, "meshd"+exeSuffix())
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка старого meshd: %v: %s", err, out)
	}
	addr := runMeshd(t, bin, filepath.Join(dir, "state.json"), "-stun", "")
	old := []*testMeshd{{addr: addr}, {addr: addr}} // главный и «побочный» — один meshd
	key := NewKey()
	a := New(Config{Key: key, DeviceID: NewDeviceID(), Name: "a", Via: "s0", NoDirect: true}, dialTo(old[0].addr), nil)
	a.AddServer("s1", "s1", dialTo(old[1].addr))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	startClient(t, addr, Config{Key: key, Name: "b", AllowIncoming: true, NoDirect: true})
	port := echoService(t, "b")
	waitFor(t, func() bool { p, n := peerByHost(a, "b"); return n == 1 && p.Online }, "a не видит b")
	waitFor(t, func() bool { return oneDuplicate(a.Status().Servers) }, "проброс не узнан как тот же meshd")
	conn, err := a.DialPeer("b.mesh:" + strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	if got := ask(t, conn, "старый"); got != "b: старый" {
		t.Fatalf("ответ %q", got)
	}
}

// oldMeshdCommit — main до ТЗ 12 (meshd версии 3).
const oldMeshdCommit = "32c1331"

// oneDuplicate — из двух путей к одному meshd один на связи, другой узнан
// как тот же сервер.
func oneDuplicate(st []ServerStatus) bool {
	return len(st) == 2 && ((st[0].State == "online" && st[1].State == "duplicate") ||
		(st[1].State == "online" && st[0].State == "duplicate"))
}
