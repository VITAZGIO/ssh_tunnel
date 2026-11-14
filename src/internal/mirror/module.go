// Package mirror — «Зеркальные папки» (ТЗ 6): устройство-сервер раздаёт
// папки, другие устройства держат их копии; правки идут устройство →
// сервер → все по сети устройств, без туннеля в интернет.
//
// Сервер — Hub (hub.go): единственный источник правды, версии и корзина 30
// дней, конфликты. Клиент — Replica (replica.go). Протокол — proto.go.
// Здесь — модуль ядра (addons): служба на порту Port сети устройств, поиск
// серверов, циклы сверки, API для страницы.
package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/devnet"
	"sshtunnel/internal/mesh"
)

// Port — служба зеркальных папок в сети устройств (рядом со службой файлов
// 47835).
const Port = 47836

func init() {
	addons.Register("mirror", func() addons.Module { return &module{} })
	addons.RegisterRoutes("mirror",
		addons.Route{Path: "/api/mirror", Methods: "GET, POST",
			Desc: "Зеркальные папки: раздаваемые (serve, у каждой devices — кому видна), подключённые (subs) со статусом, найденные серверы (found), доступ (access: устройства сети, сколько папок у каждого). POST {\"op\":\"serve\",\"path\",\"name\"} | {\"op\":\"unserve\",\"id\"} | {\"op\":\"grant\",\"device\":\"<id устройства>\",\"id\":\"hub:<id>|*\",\"on\":true|false} | {\"op\":\"ignore\",\"id\",\"list\":[…]} | {\"op\":\"join\",\"peer\",\"host\",\"folder\",\"name\",\"path\",\"mode\":\"both|recv|send\"} | {\"op\":\"leave\",\"id\"} | {\"op\":\"mode\",\"id\",\"mode\"} | {\"op\":\"sync\",\"id\"} | {\"op\":\"resolve\",\"id\",\"path\",\"keep\":\"orig|copy|both\"} | {\"op\":\"restore\",\"id\",\"blob\"}. id раздаваемой — «hub:<id>».",
			H:    handleAPI},
		addons.Route{Path: "/api/mirror/versions", Methods: "GET",
			Desc: "Версии и корзина папки ?id=<hub:id|id подключённой>[&p=путь] (30 дней).",
			H:    handleVersions},
		addons.Route{Path: "/api/mirror/file", Methods: "GET",
			Desc: "Содержимое файла папки для просмотра ?id=&p= или старой версии ?id=&blob= (до 2 МБ).",
			H:    handleFile},
	)
}

var (
	notifyMu sync.Mutex
	notifyFn = func(string) {}
)

// SetNotify — куда сообщать человеку о конфликтах (журнал окна,
// уведомление телефона).
func SetNotify(f func(text string)) {
	notifyMu.Lock()
	if f == nil {
		f = func(string) {}
	}
	notifyFn = f
	notifyMu.Unlock()
}

func notify(kind, text string) {
	notifyMu.Lock()
	f := notifyFn
	notifyMu.Unlock()
	f(text)
}

// Sub — подключённая папка этого устройства.
type Sub struct {
	ID     string `json:"id"`
	Peer   string `json:"peer"` // адрес сервера в сети устройств
	Host   string `json:"host"` // имя сервера (адрес мог смениться)
	Folder string `json:"folder"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Mode   string `json:"mode"`
}

type found struct {
	Peer    string      `json:"peer"`
	Host    string      `json:"host"`
	Folders []FolderRef `json:"folders"`
}

type module struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	hub    *Hub
	acl    *Access
	srv    *http.Server
	ln     *meshListener

	mu    sync.Mutex
	subs  map[string]*runner
	found []found
	seen  time.Time // когда искали серверы

	finding atomic.Bool // поиск серверов уже идёт — второй не запускать
}

type runner struct {
	Sub
	rep    *Replica
	kick   chan struct{}
	cancel context.CancelFunc
}

var (
	curMu sync.Mutex
	cur   *module
)

func current() *module {
	curMu.Lock()
	defer curMu.Unlock()
	return cur
}

func dataDir() string { return filepath.Join(addons.DataDir(), "mirror") }

// selfName — имя этого устройства в сети (для конфликт-копий).
func selfName() string {
	for _, p := range devnet.Status().Peers {
		if p.Self && p.Host != "" {
			return p.Host
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" && h != "localhost" {
		return h
	}
	return runtime.GOOS
}

func (m *module) Start() error {
	h, err := OpenHub(filepath.Join(dataDir(), "hub"), selfName())
	if err != nil {
		return err
	}
	h.OnEvent = notify
	m.hub = h
	var ids []string
	for _, f := range h.Folders() {
		ids = append(ids, f.ID)
	}
	if m.acl, err = OpenAccess(filepath.Join(dataDir(), "access.json"), ids); err != nil {
		h.Close()
		return err
	}
	h.Visible = func(r *http.Request, folder string) bool {
		id := callerOf(r).ID
		if id != "" && id == selfID() {
			return true // сам к себе
		}
		return m.acl.Allowed(folder, id)
	}
	m.migrate()
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.subs = map[string]*runner{}

	m.ln = &meshListener{ch: make(chan net.Conn), done: make(chan struct{})}
	m.srv = &http.Server{Handler: h.Handler(who), ReadHeaderTimeout: 20 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if cc, ok := c.(callerConn); ok {
				return context.WithValue(ctx, callerKey{}, cc.from)
			}
			return ctx
		}}
	go m.srv.Serve(m.ln)
	mesh.Serve(Port, func(c net.Conn, from mesh.Caller) {
		select {
		case m.ln.ch <- callerConn{c, from}:
		case <-m.ln.done:
			c.Close()
		}
	})

	var subs []Sub
	_ = readJSON(filepath.Join(dataDir(), "subs.json"), &subs)
	for _, s := range subs {
		m.startSub(s)
	}
	m.wg.Add(2)
	go m.hubLoop()
	go m.findLoop()

	curMu.Lock()
	cur = m
	curMu.Unlock()
	return nil
}

func (m *module) Stop() {
	curMu.Lock()
	if cur == m {
		cur = nil
	}
	curMu.Unlock()
	mesh.Serve(Port, nil)
	m.cancel()
	m.mu.Lock()
	for _, r := range m.subs {
		r.cancel()
	}
	m.mu.Unlock()
	m.srv.Close()
	m.ln.Close()
	m.wg.Wait()
	m.hub.Close()
}

// hubLoop — сверка раздаваемых папок с диском (правки прямо на сервере)
// раз в 30 с и чистка версий старше 30 дней раз в час.
func (m *module) hubLoop() {
	defer m.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		for _, f := range m.hub.Folders() {
			_ = m.hub.Scan(f.ID)
		}
		if time.Since(lastPrune) > time.Hour {
			m.hub.Prune()
			lastPrune = time.Now()
		}
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// findLoop — какие устройства сети раздают папки («Найден сервер,
// подключиться?»).
func (m *module) findLoop() {
	defer m.wg.Done()
	for {
		m.find()
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// selfID — постоянный id этого устройства в сети (mesh.PeerID).
func selfID() string { return devnet.Status().Self.ID }

// netDevice — устройство сети в списке «Доступ к папкам».
type netDevice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host,omitempty"`
	IP       string `json:"ip,omitempty"`
	Platform string `json:"platform,omitempty"`
	Server   bool   `json:"server,omitempty"`
	Online   bool   `json:"online"`
	// Gone — устройства больше нет в списке сети (убрали в панели), а
	// папки у него остались: их можно забрать.
	Gone    bool `json:"gone,omitempty"`
	Folders int  `json:"folders"`
}

// devices — все устройства сети с id (и не в сети), сначала серверы,
// потом остальные, внутри — по имени.
func (m *module) devices() []netDevice {
	return listDevices(devnet.Status().Peers, m.acl)
}

func listDevices(peers []mesh.Peer, acl *Access) []netDevice {
	granted := acl.Granted()
	seen := map[string]bool{}
	out := []netDevice{}
	for _, p := range peers {
		if p.Self || p.ID == "" || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		name := p.Name
		if name == "" {
			name = p.Host
		}
		d := netDevice{ID: p.ID, Name: name, Host: p.Host, Platform: p.Platform, Server: p.Platform == "server",
			Online: p.Online, Folders: granted[p.ID]}
		if p.IP.IsValid() {
			d.IP = p.IP.String()
		}
		out = append(out, d)
	}
	for id, n := range granted {
		if !seen[id] {
			name := acl.Name(id)
			if name == "" {
				name = id
			}
			out = append(out, netDevice{ID: id, Name: name, Gone: true, Folders: n})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Server != b.Server {
			return a.Server
		}
		if x, y := strings.ToLower(a.Name), strings.ToLower(b.Name); x != y {
			return x < y
		}
		return a.ID < b.ID
	})
	return out
}

// known — устройства сети с id для правила обновления.
func known(peers []mesh.Peer) []Known {
	var out []Known
	for _, p := range peers {
		if !p.Self && p.ID != "" {
			out = append(out, Known{ID: p.ID, Name: p.Name})
		}
	}
	return out
}

// migrate — правило обновления (access.go): как только известны id
// устройств сети, старые папки получают все известные в этот момент.
func (m *module) migrate() {
	if !m.acl.Pending() {
		return
	}
	if did, _ := m.acl.Migrate(known(devnet.Status().Peers)); did {
		m.hub.Changed()
	}
}

// grant — дать или забрать папку (folder "*" — все раздаваемые).
func (m *module) grant(device, folder string, on bool) error {
	if device == "" {
		return errors.New("нужен device — id устройства")
	}
	m.migrate() // старые папки — сначала по правилу обновления
	name := ""
	for _, d := range m.devices() {
		if d.ID == device {
			name = d.Name
		}
	}
	var ids []string
	if folder == "*" {
		for _, f := range m.hub.Folders() {
			ids = append(ids, f.ID)
		}
	} else {
		if _, err := m.hub.folder(folder); err != nil {
			return errors.New("нет такой раздаваемой папки")
		}
		ids = []string{folder}
	}
	defer m.hub.Changed() // ждущие опросы перепроверят доступ сразу
	for _, f := range ids {
		if err := m.acl.Set(f, device, name, on); err != nil {
			return err
		}
	}
	return nil
}

func (m *module) find() {
	m.migrate()
	var mu sync.Mutex
	var out []found
	var wg sync.WaitGroup
	for _, p := range devnet.Status().Peers {
		if p.Self || !p.Online || !p.IP.IsValid() {
			continue
		}
		wg.Add(1)
		go func(p mesh.Peer) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(m.ctx, 6*time.Second)
			defer cancel()
			fs, err := remoteFor(p.IP.String()).Folders(ctx)
			if err != nil || len(fs) == 0 {
				return
			}
			mu.Lock()
			out = append(out, found{Peer: p.IP.String(), Host: p.Host, Folders: fs})
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	m.mu.Lock()
	m.found, m.seen = out, time.Now()
	m.mu.Unlock()
}

// remoteFor — клиент сервера по адресу в сети устройств.
func remoteFor(peer string) *Remote {
	return &Remote{Base: "http://" + net.JoinHostPort(peer, strconv.Itoa(Port)),
		HC: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return devnet.DialPeer(addr)
			},
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		}}}
}

// peerAddr — нынешний адрес сервера: по имени, если адрес сменился.
func peerAddr(s Sub) string {
	if s.Host != "" {
		for _, p := range devnet.Status().Peers {
			if p.Host == s.Host && p.IP.IsValid() {
				return p.IP.String()
			}
		}
	}
	return s.Peer
}

func (m *module) startSub(s Sub) {
	ctx, cancel := context.WithCancel(m.ctx)
	r := &runner{Sub: s, kick: make(chan struct{}, 1), cancel: cancel}
	r.rep = &Replica{Folder: s.Folder, Path: s.Path, Mode: s.Mode, Device: selfName(),
		State: filepath.Join(dataDir(), "sub-"+s.ID+".json"), R: remoteFor(peerAddr(s)), OnEvent: notify}
	m.mu.Lock()
	m.subs[s.ID] = r
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		r.loop(ctx)
	}()
}

// loop — сверка: долгий опрос сервера (изменения приходят сразу), свои
// изменения — не позже чем через 10 с, полная перепроверка хешей — раз в 6
// часов. Без сервера — повтор раз в 30 с; очередь — разница диска и
// состояния, она не теряется.
func (r *runner) loop(ctx context.Context) {
	wait := time.Duration(0)
	lastFull := time.Now()
	for ctx.Err() == nil {
		r.rep.R = remoteFor(peerAddr(Sub{Host: r.Host, Peer: r.Peer}))
		full := time.Since(lastFull) > 6*time.Hour
		did, err := r.rep.Sync(ctx, wait, full)
		if full && err == nil {
			lastFull = time.Now()
		}
		pause := time.Duration(0)
		switch {
		case err != nil && ctx.Err() == nil:
			wait, pause = 0, 30*time.Second
			if st := r.rep.Status().State; st != "offline" && st != "closed" {
				pause = 15 * time.Second
			}
		case did:
			wait = 0
		default:
			wait = 10 * time.Second
		}
		if pause > 0 {
			select {
			case <-ctx.Done():
			case <-r.kick:
			case <-time.After(pause):
			}
		}
		select {
		case <-r.kick:
			wait = 0
		default:
		}
	}
}

func (m *module) saveSubs() error {
	m.mu.Lock()
	list := []Sub{}
	for _, r := range m.subs {
		list = append(list, r.Sub)
	}
	m.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return writeJSONAtomic(filepath.Join(dataDir(), "subs.json"), list)
}

func (m *module) overlaps(p string) error {
	for _, f := range m.hub.Folders() {
		if nested(f.Path, p) {
			return errors.New("пересекается с раздаваемой папкой «" + f.Name + "»")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.subs {
		if nested(r.Path, p) {
			return errors.New("пересекается с подключённой папкой «" + r.Name + "»")
		}
	}
	return nil
}

func (m *module) join(s Sub) error {
	switch s.Mode {
	case "":
		s.Mode = ModeBoth
	case ModeBoth, ModeRecv, ModeSend:
	default:
		return errors.New("mode: both, recv или send")
	}
	if s.Peer == "" || s.Folder == "" || strings.TrimSpace(s.Path) == "" {
		return errors.New("нужны peer, folder и path")
	}
	abs, err := filepath.Abs(localPath(s.Path))
	if err != nil {
		return err
	}
	if err := m.overlaps(abs); err != nil {
		return err
	}
	m.mu.Lock()
	for _, r := range m.subs {
		if r.Folder == s.Folder && (r.Host == s.Host || r.Peer == s.Peer) {
			m.mu.Unlock()
			return errors.New("эта папка уже подключена: " + r.Path)
		}
	}
	m.mu.Unlock()
	// Существующую папку можно: совпадающие файлы не передаются, разные
	// становятся конфликт-копиями — ничего не затирается.
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	s.Path = abs
	s.ID = randHex(5)
	m.startSub(s)
	return m.saveSubs()
}

func (m *module) leave(id string) error {
	m.mu.Lock()
	r, ok := m.subs[id]
	delete(m.subs, id)
	m.mu.Unlock()
	if !ok {
		return errors.New("нет такой папки")
	}
	r.cancel()
	r.rep.Forget() // ждёт конца прохода; файлы папки остаются
	return m.saveSubs()
}

func (m *module) setMode(id, mode string) error {
	if mode != ModeBoth && mode != ModeRecv && mode != ModeSend {
		return errors.New("mode: both, recv или send")
	}
	m.mu.Lock()
	r, ok := m.subs[id]
	if ok {
		r.Mode = mode
	}
	m.mu.Unlock()
	if !ok {
		return errors.New("нет такой папки")
	}
	r.rep.mu.Lock() // между проходами
	r.rep.Mode = mode
	r.rep.mu.Unlock()
	select {
	case r.kick <- struct{}{}:
	default:
	}
	return m.saveSubs()
}

func (m *module) sub(id string) (*runner, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.subs[id]
	return r, ok
}

// ---- API для страницы ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// localPath — путь, как его написал человек, — на телефоне во внутренней
// памяти: «/TRASH/Obsidian/Server» или «TRASH/Obsidian» — это
// /storage/emulated/0/TRASH/…, как у Syncthing (корень Android только для
// чтения, и такой путь иначе падал с «read-only file system»).
func localPath(p string) string {
	p = strings.TrimSpace(p)
	if runtime.GOOS != "android" || p == "" {
		return p
	}
	for _, root := range []string{"/storage/", "/sdcard", "/mnt/", "/data/"} {
		if strings.HasPrefix(p, root) {
			return p
		}
	}
	return "/storage/emulated/0/" + strings.TrimLeft(p, "/")
}

func suggestBase() string {
	if runtime.GOOS == "android" {
		return "/storage/emulated/0"
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, "Documents")
	}
	return ""
}

func (m *module) state() map[string]any {
	type serveItem struct {
		HubFolder
		Files     int        `json:"files"`
		Conflicts []Conflict `json:"conflicts"`
		// Devices — кому видна (id устройств, ТЗ 18); Old — папка с прошлой
		// версии, пока видна всем (ждёт id устройств от meshd).
		Devices []string `json:"devices"`
		Old     bool     `json:"old,omitempty"`
	}
	m.migrate()
	serve := []serveItem{}
	for _, f := range m.hub.Folders() {
		it := serveItem{HubFolder: f, Conflicts: []Conflict{}, Devices: m.acl.Devices(f.ID), Old: m.acl.IsOld(f.ID)}
		if r, err := m.hub.Index(f.ID, 0, 0, nil); err == nil {
			for p, fi := range r.Files {
				if !fi.Deleted && !isDir(p) {
					it.Files++
				}
			}
			it.Conflicts = r.Conflicts
		}
		serve = append(serve, it)
	}
	type subItem struct {
		Sub
		Status Status `json:"status"`
	}
	m.mu.Lock()
	subs := []subItem{}
	joined := map[string]bool{}
	for _, r := range m.subs {
		subs = append(subs, subItem{r.Sub, r.rep.Status()})
		joined[r.Host+"/"+r.Folder] = true
		joined[r.Peer+"/"+r.Folder] = true
	}
	type foundItem struct {
		found
		Joined map[string]bool `json:"joined"`
	}
	fs := []foundItem{}
	for _, f := range m.found {
		it := foundItem{f, map[string]bool{}}
		for _, x := range f.Folders {
			if joined[f.Host+"/"+x.ID] || joined[f.Peer+"/"+x.ID] {
				it.Joined[x.ID] = true
			}
		}
		fs = append(fs, it)
	}
	stale := time.Since(m.seen) > 15*time.Second
	m.mu.Unlock()
	if stale && m.finding.CompareAndSwap(false, true) {
		go func() { defer m.finding.Store(false); m.find() }()
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name < subs[j].Name })
	devs := m.devices()
	// ids — сеть даёт id устройств (новый meshd): без них доступ не выдать.
	ids := false
	for _, d := range devs {
		ids = ids || !d.Gone
	}
	return map[string]any{"serve": serve, "subs": subs, "found": fs, "self": selfName(),
		"suggest": suggestBase(), "platform": runtime.GOOS, "devnet": devnet.Status().State,
		"access": map[string]any{"devices": devs, "ids": ids, "pending": m.acl.Pending()}}
}

func handleAPI(w http.ResponseWriter, r *http.Request) {
	m := current()
	if m == nil {
		http.Error(w, "приложение не запущено", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Op     string   `json:"op"`
			ID     string   `json:"id"`
			Path   string   `json:"path"`
			Name   string   `json:"name"`
			List   []string `json:"list"`
			Peer   string   `json:"peer"`
			Host   string   `json:"host"`
			Folder string   `json:"folder"`
			Mode   string   `json:"mode"`
			Keep   string   `json:"keep"`
			Blob   string   `json:"blob"`
			Device string   `json:"device"`
			On     bool     `json:"on"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, map[string]string{"error": "нужен JSON {\"op\"}"})
			return
		}
		var err error
		hubID, isHub := strings.CutPrefix(req.ID, "hub:")
		switch req.Op {
		case "serve":
			if err = m.overlaps(req.Path); err == nil {
				_, err = m.hub.AddFolder(localPath(req.Path), req.Name)
			}
		case "unserve":
			if err = m.hub.RemoveFolder(hubID); err == nil {
				err = m.acl.Remove(hubID)
			}
		case "grant":
			switch {
			case req.ID == "*":
				err = m.grant(req.Device, "*", req.On)
			case isHub:
				err = m.grant(req.Device, hubID, req.On)
			default:
				err = errors.New("id: hub:<id папки> или *")
			}
		case "ignore":
			err = m.hub.SetIgnore(hubID, req.List)
		case "join":
			err = m.join(Sub{Peer: req.Peer, Host: req.Host, Folder: req.Folder, Name: req.Name, Path: req.Path, Mode: req.Mode})
		case "leave":
			err = m.leave(req.ID)
		case "mode":
			err = m.setMode(req.ID, req.Mode)
		case "sync":
			if isHub {
				err = m.hub.Scan(hubID)
			} else if s, ok := m.sub(req.ID); ok {
				select {
				case s.kick <- struct{}{}:
				default:
				}
			}
		case "resolve":
			if isHub {
				err = m.hub.Resolve(hubID, selfName(), req.Path, req.Keep)
			} else if s, ok := m.sub(req.ID); ok {
				ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				err = s.rep.Resolve(ctx, req.Path, req.Keep)
				cancel()
				select {
				case s.kick <- struct{}{}:
				default:
				}
			} else {
				err = errors.New("нет такой папки")
			}
		case "restore":
			if isHub {
				_, err = m.hub.Restore(hubID, selfName(), req.Blob)
			} else if s, ok := m.sub(req.ID); ok {
				var out map[string]string
				err = s.rep.R.post(r.Context(), "/restore", map[string]string{"f": s.Folder, "blob": req.Blob}, &out)
			} else {
				err = errors.New("нет такой папки")
			}
		default:
			err = errors.New("op: serve, unserve, grant, ignore, join, leave, mode, sync, resolve или restore")
		}
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, m.state())
}

func handleVersions(w http.ResponseWriter, r *http.Request) {
	m := current()
	if m == nil {
		http.Error(w, "приложение не запущено", http.StatusServiceUnavailable)
		return
	}
	id, p := r.URL.Query().Get("id"), r.URL.Query().Get("p")
	if hubID, ok := strings.CutPrefix(id, "hub:"); ok {
		vs, err := m.hub.Versions(hubID, p)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, vs)
		return
	}
	s, ok := m.sub(id)
	if !ok {
		writeJSON(w, map[string]string{"error": "нет такой папки"})
		return
	}
	var vs []Version
	if err := s.rep.R.json(r.Context(), "GET", "/versions", url.Values{"f": {s.Folder}, "p": {p}}, nil, &vs); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, vs)
}

// handleFile — файл для просмотра двух версий рядом. С клиента берётся с
// сервера: у «только отправлять» копии конфликта на устройстве нет.
func handleFile(w http.ResponseWriter, r *http.Request) {
	m := current()
	if m == nil {
		http.Error(w, "приложение не запущено", http.StatusServiceUnavailable)
		return
	}
	id, p, blob := r.URL.Query().Get("id"), r.URL.Query().Get("p"), r.URL.Query().Get("blob")
	var rc io.ReadCloser
	if hubID, ok := strings.CutPrefix(id, "hub:"); ok {
		var f *os.File
		var err error
		if blob != "" {
			f, err = m.hub.OpenVersion(hubID, blob)
		} else {
			f, err = m.hub.Open(hubID, p, "")
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		rc = f
	} else {
		s, ok := m.sub(id)
		if !ok {
			http.Error(w, "нет такой папки", http.StatusNotFound)
			return
		}
		path, q := "/get", url.Values{"f": {s.Folder}, "p": {p}}
		if blob != "" {
			path, q = "/blob", url.Values{"f": {s.Folder}, "b": {blob}}
		}
		resp, err := s.rep.R.do(r.Context(), "GET", path, q, nil, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			http.Error(w, resp.Status, resp.StatusCode)
			return
		}
		rc = resp.Body
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, io.LimitReader(rc, 2<<20))
}

// ---- служба в сети устройств (как у internal/devfiles) ----

type callerKey struct{}

type callerConn struct {
	net.Conn
	from mesh.Caller
}

type meshListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *meshListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *meshListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *meshListener) Addr() net.Addr { return &net.TCPAddr{Port: Port} }

// callerOf — кто пришёл (ставит сеть устройств, не подделать).
func callerOf(r *http.Request) mesh.Caller {
	c, _ := r.Context().Value(callerKey{}).(mesh.Caller)
	return c
}

// who — имя пришедшего устройства (ставит сеть устройств, не подделать).
func who(r *http.Request) string {
	c := callerOf(r)
	if c.Host != "" {
		return c.Host
	}
	return c.IP
}
