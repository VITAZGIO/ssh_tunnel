// Package adblock — устройство берёт список «Блокировки рекламы» с сервера
// сети (ТЗ 15). Сам список ведёт панель сервера (internal/node, порт
// ServerPort): источники, свои «блокировать» и «не блокировать». Устройство
// раз в 15 минут спрашивает его (с меткой — пока список тот же, сервер
// отвечает «не изменился») и кладёт в тот же файл adblock.list, который
// читает слой VPN, — блокирует по-прежнему устройство, своим DNS.
//
// Нет связи с сервером или плагин там выключен — действует последний
// полученный список, ничего не стирается. «Брать список с сервера» снят —
// устройство снова качает свои источники кнопкой «Обновить списки».
//
// Исключения сервера («@@имя») добавляются к своим исключениям устройства
// (WithServer) — так «не блокировать» панели снимает и поддомены, которые
// закрыты по родителю.
package adblock

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/config"
	"sshtunnel/internal/devnet"
	"sshtunnel/internal/mesh"
)

// ID — плагин в каталоге.
const ID = "adblock"

// ServerPort — порт плагина на сервере (nodeapi.PortAdblock).
const ServerPort = 47845

// every — как часто спрашивать сервер.
const every = 15 * time.Minute

// State — что известно о списке с сервера (adblock-server.json).
type State struct {
	// Off — не брать список с сервера (своя галка устройства).
	Off bool `json:"off,omitempty"`
	// Using — сейчас в adblock.list лежит список сервера.
	Using   bool     `json:"using"`
	Server  string   `json:"server,omitempty"` // имя сервера
	ETag    string   `json:"etag,omitempty"`
	Updated int64    `json:"updated,omitempty"` // когда сервер обновил список (unix)
	Fetched int64    `json:"fetched,omitempty"` // когда устройство его получило
	Checked int64    `json:"checked,omitempty"` // последняя попытка
	Names   int      `json:"names"`
	Allow   []string `json:"allow,omitempty"`
	Error   string   `json:"error,omitempty"`
}

var (
	mu     sync.Mutex
	st     State
	loaded bool
	reload func()
	// listPath, statePath — подменяются в тестах.
	listPath  = func() string { return filepath.Join(config.Dir(), "adblock.list") }
	statePath = func() string { return filepath.Join(config.Dir(), "adblock-server.json") }
	// servers — серверы сети на связи (тесты подменяют).
	servers = meshServers
	// dial — соединение с сервером в сети устройств.
	dial = func(addr string) (net.Conn, error) { return devnet.DialPeer(addr) }
)

func init() {
	addons.Register(ID, func() addons.Module { return &module{} })
	addons.RegisterRoutes(ID, addons.Route{
		Path: "/api/adblock/server", Methods: "GET, POST",
		Desc: "Блокировка рекламы: список с сервера сети. GET — откуда список, когда обновлён, сколько имён; POST {\"op\":\"sync\"} — спросить сервер сейчас, {\"op\":\"use\",\"on\":bool} — брать ли список с сервера.",
		H:    handleAPI,
	})
}

// SetReload — как перечитать список (зовёт программа, у которой есть слой VPN).
func SetReload(f func()) {
	mu.Lock()
	reload = f
	mu.Unlock()
}

func loadLocked() {
	if loaded {
		return
	}
	loaded = true
	if b, err := os.ReadFile(statePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
}

func saveLocked() {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := statePath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, statePath())
	}
}

// Current — состояние списка с сервера.
func Current() State {
	mu.Lock()
	defer mu.Unlock()
	loadLocked()
	s := st
	s.Allow = append([]string(nil), st.Allow...)
	return s
}

// UsingServer — список ведёт сервер (своими источниками качать нельзя).
func UsingServer() (bool, string) {
	s := Current()
	return s.Using && !s.Off, s.Server
}

// WithServer — настройки для слоя VPN: к своим исключениям устройства —
// исключения сервера, если действует его список.
func WithServer(cfg config.Config) config.Config {
	s := Current()
	if !s.Using || s.Off || len(s.Allow) == 0 {
		return cfg
	}
	cfg.AdBlockAllowlist = strings.TrimSpace(cfg.AdBlockAllowlist + "\n" + strings.Join(s.Allow, "\n"))
	return cfg
}

func meshServers() []mesh.Peer {
	var out []mesh.Peer
	for _, p := range devnet.Status().Peers {
		if !p.Self && p.Online && p.Platform == "server" && p.IP.IsValid() {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

func srvName(p mesh.Peer) string {
	if p.Name != "" {
		return p.Name
	}
	return p.Host
}

var syncMu sync.Mutex

// Sync — спросить серверы сети и, если список новый, применить его.
func Sync(ctx context.Context) State {
	syncMu.Lock()
	defer syncMu.Unlock()
	cur := Current()
	if cur.Off {
		return cur
	}
	list := servers()
	now := time.Now().Unix()
	if len(list) == 0 {
		return finish(func(s *State) {
			s.Checked = now
			s.Error = "серверов сети нет на связи"
			if s.Using {
				s.Error += " — действует последний список"
			}
		})
	}
	var lastErr string
	for _, p := range list {
		etag := ""
		if cur.Server == srvName(p) && cur.Using {
			etag = cur.ETag
		}
		res, err := fetch(ctx, p.IP.String(), etag)
		if err != nil {
			lastErr = "«" + srvName(p) + "»: " + err.Error()
			continue
		}
		if res.notModified {
			return finish(func(s *State) { s.Checked, s.Error = now, "" })
		}
		if err := writeList(res.names); err != nil {
			lastErr = err.Error()
			continue
		}
		s := finish(func(s *State) {
			*s = State{Using: true, Server: srvName(p), ETag: res.etag, Updated: res.updated,
				Fetched: now, Checked: now, Names: len(res.names), Allow: res.allow}
		})
		mu.Lock()
		f := reload
		mu.Unlock()
		if f != nil {
			f()
		}
		return s
	}
	return finish(func(s *State) {
		s.Checked, s.Error = now, lastErr
		if s.Using {
			s.Error += " — действует последний список"
		}
	})
}

func finish(f func(*State)) State {
	mu.Lock()
	defer mu.Unlock()
	loadLocked()
	f(&st)
	saveLocked()
	s := st
	s.Allow = append([]string(nil), st.Allow...)
	return s
}

type fetched struct {
	notModified bool
	etag        string
	updated     int64
	names       []string
	allow       []string
}

func fetch(ctx context.Context, ip, etag string) (fetched, error) {
	hc := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext:       func(_ context.Context, _, addr string) (net.Conn, error) { return dial(addr) },
	}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(ip, strconv.Itoa(ServerPort))+"/list", nil)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := hc.Do(req)
	if err != nil {
		if s := err.Error(); strings.Contains(s, "не слушает") || strings.Contains(s, "refused") {
			return fetched{}, errors.New("на сервере не включён плагин «Блокировка рекламы»")
		}
		return fetched{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return fetched{notModified: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fetched{}, errors.New(e.Error)
	}
	out := fetched{etag: resp.Header.Get("ETag")}
	out.updated, _ = strconv.ParseInt(resp.Header.Get("X-Adblock-Updated"), 10, 64)
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 128<<20))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case l == "" || l[0] == '#':
		case strings.HasPrefix(l, "@@"):
			out.allow = append(out.allow, strings.TrimPrefix(l, "@@"))
		default:
			out.names = append(out.names, l)
		}
	}
	if err := sc.Err(); err != nil {
		return fetched{}, err
	}
	return out, nil
}

// writeList — тот же формат, что у android/core.UpdateBlockListFile: имя в
// строке.
func writeList(names []string) error {
	p := listPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(names, "\n")), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// SetUse — брать ли список с сервера. Сняли — список остаётся, пока не
// скачаешь свои источники.
func SetUse(on bool) State {
	s := finish(func(s *State) {
		s.Off = !on
		if !on {
			s.Using = false
		}
	})
	mu.Lock()
	f := reload
	mu.Unlock()
	if f != nil {
		f()
	}
	return s
}

type module struct {
	stop chan struct{}
	done chan struct{}
}

func (m *module) Start() error {
	m.stop, m.done = make(chan struct{}), make(chan struct{})
	go func(stop, done chan struct{}) {
		defer close(done)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-stop; cancel() }()
		wait := time.Minute // первый раз — пока поднимется сеть устройств
		for {
			select {
			case <-stop:
				return
			case <-time.After(wait):
			}
			s := Sync(ctx)
			wait = every
			if s.Error != "" && !s.Using {
				wait = 2 * time.Minute
			}
		}
	}(m.stop, m.done)
	return nil
}

func (m *module) Stop() {
	if m.stop != nil {
		close(m.stop)
		<-m.done
		m.stop = nil
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func handleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusOK, Current())
		return
	}
	var req struct {
		Op string `json:"op"`
		On bool   `json:"on"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "нужен JSON {\"op\"}"})
		return
	}
	switch req.Op {
	case "sync":
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		writeJSON(w, http.StatusOK, Sync(ctx))
	case "use":
		s := SetUse(req.On)
		if req.On {
			ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
			defer cancel()
			s = Sync(ctx)
		}
		writeJSON(w, http.StatusOK, s)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "op: sync, use"})
	}
}
