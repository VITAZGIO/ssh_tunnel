package adblock

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"sshtunnel/internal/config"
	"sshtunnel/internal/mesh"
)

// setup — сервер сети с плагином (httptest) и файлы во временной папке.
func setup(t *testing.T, h http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	oldList, oldState, oldServers, oldDial := listPath, statePath, servers, dial
	listPath = func() string { return filepath.Join(dir, "adblock.list") }
	statePath = func() string { return filepath.Join(dir, "adblock-server.json") }
	servers = func() []mesh.Peer {
		return []mesh.Peer{{Name: "VPS", Host: "vps", Platform: "server", Online: true, IP: netip.MustParseAddr("198.19.255.254")}}
	}
	dial = func(string) (net.Conn, error) { return net.Dial("tcp", srv.Listener.Addr().String()) }
	mu.Lock()
	st, loaded, reload = State{}, false, nil
	mu.Unlock()
	t.Cleanup(func() {
		listPath, statePath, servers, dial = oldList, oldState, oldServers, oldDial
		mu.Lock()
		st, loaded, reload = State{}, false, nil
		mu.Unlock()
	})
	return srv, dir
}

func TestСписокССервераИПоследнийБезСвязи(t *testing.T) {
	var down atomic.Bool
	var asked atomic.Int32
	_, dir := setup(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if down.Load() {
			http.Error(w, `{"error":"сломалось"}`, 500)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("X-Adblock-Updated", "1700000000")
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte("# заголовок\n@@good.ads.net\nads.net\ntrack.example.com\n"))
	})
	var reloads atomic.Int32
	SetReload(func() { reloads.Add(1) })

	s := Sync(context.Background())
	if !s.Using || s.Server != "VPS" || s.Names != 2 || s.ETag != `"v1"` || s.Updated != 1700000000 || s.Error != "" {
		t.Fatalf("первый раз: %+v", s)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "adblock.list"))
	if string(b) != "ads.net\ntrack.example.com" {
		t.Fatalf("файл для слоя VPN: %q", b)
	}
	if reloads.Load() != 1 {
		t.Fatal("блокировку не перечитали")
	}
	cfg := WithServer(config.Config{AdBlockAllowlist: "my.site"})
	if !strings.Contains(cfg.AdBlockAllowlist, "my.site") || !strings.Contains(cfg.AdBlockAllowlist, "good.ads.net") {
		t.Fatalf("исключения сервера: %q", cfg.AdBlockAllowlist)
	}
	if on, srv := UsingServer(); !on || srv != "VPS" {
		t.Fatal("список ведёт сервер")
	}

	// Тот же список — 304, ничего не переписывается.
	s = Sync(context.Background())
	if reloads.Load() != 1 || !s.Using || s.Names != 2 {
		t.Fatalf("304: %+v (перечитываний %d)", s, reloads.Load())
	}

	// Сервер сломался — последний список остаётся.
	down.Store(true)
	s = Sync(context.Background())
	if !s.Using || s.Names != 2 || !strings.Contains(s.Error, "последний список") {
		t.Fatalf("без сервера: %+v", s)
	}
	if b2, _ := os.ReadFile(filepath.Join(dir, "adblock.list")); string(b2) != string(b) {
		t.Fatal("файл стёрт без связи с сервером")
	}
	// Сети нет вовсе — тоже.
	servers = func() []mesh.Peer { return nil }
	if s = Sync(context.Background()); !s.Using || !strings.Contains(s.Error, "последний список") {
		t.Fatalf("без сети: %+v", s)
	}

	// Состояние переживает перезапуск программы.
	mu.Lock()
	st, loaded = State{}, false
	mu.Unlock()
	if c := Current(); !c.Using || c.Names != 2 || len(c.Allow) != 1 {
		t.Fatalf("после перезапуска: %+v", c)
	}

	// Сняли «Брать список с сервера» — не спрашиваем и не подмешиваем.
	n := asked.Load()
	SetUse(false)
	Sync(context.Background())
	if asked.Load() != n {
		t.Fatal("выключенное спрашивает сервер")
	}
	if cfg := WithServer(config.Config{AdBlockAllowlist: "my.site"}); cfg.AdBlockAllowlist != "my.site" {
		t.Fatalf("выключенное подмешивает исключения: %q", cfg.AdBlockAllowlist)
	}
	if on, _ := UsingServer(); on {
		t.Fatal("выключенное считается ведущим")
	}
}

func TestПлагинНаСервереВыключен(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) {})
	dial = func(string) (net.Conn, error) { return nil, errors.New("на сервере на порту 47845 ничего не слушает") }
	s := Sync(context.Background())
	if s.Using || !strings.Contains(s.Error, "не включён плагин") {
		t.Fatalf("выключенный плагин: %+v", s)
	}
}
