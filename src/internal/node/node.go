// Package node — хост плагинов на сервере (ssh_tunnel_node, ТЗ 9): сервер
// как полноправный участник сети устройств для трёх плагинов —
// «Зеркальные папки» (сервер — всегда включённый «сервер папки»), «Файлы
// устройства» (общая папка сети на сервере) и «Общий буфер» (сервер хранит
// историю и отдаёт пропущенное).
//
// Плагины не переписаны: здесь работают те же модули, что в программе
// (mirror.Hub, devfiles.NewHandler, clipshare.ServerBoard); отличается
// только, откуда известно, кто пришёл, и где лежат данные.
//
// Как устроено:
//   - процесс работает под своим пользователем системы (nodeapi.User), без
//     root; данные — только в своей папке (nodeapi.DataDir), по сети на
//     сеть: <data>/nets/<id сети>/...;
//   - порты плагинов (nodeapi.Ports) — только на 127.0.0.1. Устройство
//     звонит на 198.19.255.254:ПОРТ, meshd открывает соединение с
//     127.0.0.1:ПОРТ. На каждое соединение хост спрашивает meshd, кто это
//     (op who по порту, с которого пришло соединение). meshd отвечает только
//     про соединения, которые открыл сам, и только владельцу порта: чужой
//     локальный пользователь или соединение мимо meshd не опознаются и сразу
//     закрываются;
//   - сети разделены: данные сети ищутся только по id сети, который назвал
//     meshd; права доступа панели (policy) meshd проверяет до соединения, как
//     для любого порта сервера;
//   - панель управляет хостом через unix-сокет (nodeapi.ControlSocket);
//   - «Резервные копии» (ТЗ 13, internal/backup) — четвёртый плагин, только
//     серверный: копии устройств по сетям, читать может лишь владелец
//     (устройство или его SSH-пользователь).
package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/backup"
	"sshtunnel/internal/clipshare"
	"sshtunnel/internal/devfiles"
	"sshtunnel/internal/mesh"
	"sshtunnel/internal/mirror"
	"sshtunnel/internal/nodeapi"
)

// Options — как запустить хост.
type Options struct {
	Dir     string // папка данных
	Meshd   string // адрес meshd (127.0.0.1:47831)
	Listen  string // адрес портов плагинов (127.0.0.1)
	Version string
	// Ports — порт каждого плагина; нет — стандартный (тесты: 0 — любой).
	Ports map[string]int
	// Who — кто пришёл; nil — спросить meshd. Подменяется в тестах.
	Who func(conn net.Conn) (nodeapi.Caller, error)
	// UsageEvery — как часто пересчитывать занятое место с диска.
	UsageEvery time.Duration
	// Now — часы (тесты).
	Now func() time.Time
}

// folderConf — зеркальная папка сети в config.json.
type folderConf struct {
	ID      string `json:"id"` // id папки у Hub
	Name    string `json:"name"`
	Path    string `json:"path"`
	QuotaMB int64  `json:"quotaMB"`
}

type fileConf struct {
	Config  nodeapi.Config          `json:"config"`
	Folders map[string][]folderConf `json:"folders"` // по id сети
}

// Host — работающий хост плагинов.
type Host struct {
	opt Options

	mu      sync.Mutex
	conf    fileConf
	nets    map[string]*netData
	lns     map[string]*pluginListener
	events  []nodeapi.Event
	meshd   string
	used    int64 // всё занятое место (кэш)
	usedAt  time.Time
	stopped bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type netData struct {
	id  string
	dir string

	mu       sync.Mutex
	hub      *mirror.Hub
	acl      *mirror.Access // кому видна каждая папка (ТЗ 18)
	mirrorH  http.Handler
	board    *clipshare.ServerBoard
	filesH   http.Handler
	backup   *backup.Store
	last     map[string]int64
	used     int64 // всё место сети (кэш)
	folderBy map[string]int64
}

const maxEvents = 100

// New открывает хост: настройки, папки сетей. Порты — в Start.
func New(opt Options) (*Host, error) {
	if opt.Meshd == "" {
		opt.Meshd = "127.0.0.1:47831"
	}
	if opt.Listen == "" {
		opt.Listen = "127.0.0.1"
	}
	if opt.UsageEvery <= 0 {
		opt.UsageEvery = 30 * time.Second
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Who == nil {
		addr := opt.Meshd
		opt.Who = func(c net.Conn) (nodeapi.Caller, error) { return AskMeshd(addr, c) }
	}
	if err := os.MkdirAll(filepath.Join(opt.Dir, "nets"), 0o700); err != nil {
		return nil, err
	}
	h := &Host{opt: opt, nets: map[string]*netData{}, lns: map[string]*pluginListener{}}
	h.conf.Config.QuotaMB = nodeapi.DefaultQuotaMB
	if data, err := os.ReadFile(h.confPath()); err == nil {
		if err := json.Unmarshal(data, &h.conf); err != nil {
			return nil, fmt.Errorf("испорчен %s: %w", h.confPath(), err)
		}
	}
	if h.conf.Folders == nil {
		h.conf.Folders = map[string][]folderConf{}
	}
	if h.conf.Config.QuotaMB <= 0 {
		h.conf.Config.QuotaMB = nodeapi.DefaultQuotaMB
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	return h, nil
}

func (h *Host) confPath() string { return filepath.Join(h.opt.Dir, "config.json") }

func (h *Host) saveLocked() error {
	data, _ := json.MarshalIndent(h.conf, "", "  ")
	tmp := h.confPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.confPath())
}

// Start поднимает включённые плагины и фоновые круги (сверка папок, чистка
// буфера, пересчёт места).
func (h *Host) Start() error {
	h.mu.Lock()
	err := h.applyLocked()
	h.mu.Unlock()
	h.wg.Add(1)
	go h.loop()
	return err
}

// Close останавливает всё.
func (h *Host) Close() {
	h.mu.Lock()
	h.stopped = true
	for p, l := range h.lns {
		l.close()
		delete(h.lns, p)
	}
	nets := h.nets
	h.mu.Unlock()
	h.cancel()
	h.wg.Wait()
	for _, nd := range nets {
		nd.mu.Lock()
		if nd.hub != nil {
			nd.hub.Close()
		}
		nd.mu.Unlock()
	}
}

// Addr — где слушает плагин (тесты); "" — выключен.
func (h *Host) Addr(plugin string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l := h.lns[plugin]; l != nil {
		return l.ln.Addr().String()
	}
	return ""
}

func (h *Host) port(plugin string) int {
	if p, ok := h.opt.Ports[plugin]; ok {
		return p
	}
	switch plugin {
	case nodeapi.PluginFiles:
		return nodeapi.PortFiles
	case nodeapi.PluginMirror:
		return nodeapi.PortMirror
	case nodeapi.PluginBackup:
		return nodeapi.PortBackup
	default:
		return nodeapi.PortClipboard
	}
}

// applyLocked открывает порты включённых плагинов и закрывает выключенные:
// выключенный плагин не слушает вовсе — устройство получает «на сервере
// на порту ничего не слушает».
func (h *Host) applyLocked() error {
	want := map[string]bool{
		nodeapi.PluginMirror:    h.conf.Config.Mirror,
		nodeapi.PluginFiles:     h.conf.Config.Files,
		nodeapi.PluginClipboard: h.conf.Config.Clipboard,
		nodeapi.PluginBackup:    h.conf.Config.Backup,
	}
	var errs []string
	for _, p := range []string{nodeapi.PluginMirror, nodeapi.PluginFiles, nodeapi.PluginClipboard, nodeapi.PluginBackup} {
		l := h.lns[p]
		switch {
		case want[p] && l == nil && !h.stopped:
			nl, err := h.listen(p)
			if err != nil {
				errs = append(errs, err.Error())
				h.eventLocked(nodeapi.Event{Plugin: p, Error: true, Text: err.Error()})
				continue
			}
			h.lns[p] = nl
		case !want[p] && l != nil:
			l.close()
			delete(h.lns, p)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (h *Host) eventLocked(e nodeapi.Event) {
	if e.Time == 0 {
		e.Time = h.opt.Now().Unix()
	}
	h.events = append(h.events, e)
	if len(h.events) > maxEvents {
		h.events = h.events[len(h.events)-maxEvents:]
	}
}

func (h *Host) event(e nodeapi.Event) {
	h.mu.Lock()
	h.eventLocked(e)
	h.mu.Unlock()
}

// ---------- соединения ----------

type callerKey struct{}

func callerOf(r *http.Request) nodeapi.Caller {
	c, _ := r.Context().Value(callerKey{}).(nodeapi.Caller)
	return c
}

// pluginListener — порт плагина: принимает соединения, узнаёт у meshd, кто
// пришёл, и только опознанные отдаёт HTTP-серверу.
type pluginListener struct {
	ln   net.Listener
	srv  *http.Server
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

type callerConn struct {
	net.Conn
	c nodeapi.Caller
}

func (l *pluginListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pluginListener) Close() error   { return nil }
func (l *pluginListener) Addr() net.Addr { return l.ln.Addr() }

func (l *pluginListener) close() {
	l.once.Do(func() {
		close(l.done)
		l.ln.Close()
		l.srv.Close()
	})
}

func (h *Host) listen(plugin string) (*pluginListener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(h.opt.Listen, strconv.Itoa(h.port(plugin))))
	if err != nil {
		return nil, fmt.Errorf("порт плагина занят: %v", err)
	}
	l := &pluginListener{ln: ln, ch: make(chan net.Conn), done: make(chan struct{})}
	l.srv = &http.Server{Handler: h.pluginHandler(plugin), ReadHeaderTimeout: 20 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if cc, ok := c.(callerConn); ok {
				return context.WithValue(ctx, callerKey{}, cc.c)
			}
			return ctx
		}}
	go l.srv.Serve(l)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go h.admit(l, plugin, c)
		}
	}()
	return l, nil
}

// admit — опознать соединение; чужое — закрыть.
func (h *Host) admit(l *pluginListener, plugin string, c net.Conn) {
	if ra, ok := c.RemoteAddr().(*net.TCPAddr); !ok || !ra.IP.IsLoopback() {
		c.Close()
		return
	}
	who, err := h.opt.Who(c)
	h.mu.Lock()
	switch {
	case err == nil:
		h.meshd = "ok"
	case errors.Is(err, errMeshdOld):
		if h.meshd != "old" {
			h.eventLocked(nodeapi.Event{Plugin: plugin, Error: true,
				Text: "meshd старой версии не говорит, кто пришёл — плагины никого не пускают. Обнови сеть устройств в панели («Переустановить»)."})
		}
		h.meshd = "old"
	case errors.Is(err, errMeshdDown):
		h.meshd = "down"
	}
	h.mu.Unlock()
	if err != nil || !nodeapi.ValidNet(who.Net) || who.Device == "" {
		c.Close()
		return
	}
	select {
	case l.ch <- callerConn{c, who}:
	case <-l.done:
		c.Close()
	}
}

var (
	errMeshdOld  = errors.New("meshd не умеет who")
	errMeshdDown = errors.New("meshd не отвечает")
)

// AskMeshd — кто пришёл на соединение c (оно открыто meshd на наш порт).
func AskMeshd(addr string, c net.Conn) (nodeapi.Caller, error) {
	la, ok1 := c.LocalAddr().(*net.TCPAddr)
	ra, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return nodeapi.Caller{}, errors.New("не TCP")
	}
	m, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nodeapi.Caller{}, fmt.Errorf("%w: %v", errMeshdDown, err)
	}
	defer m.Close()
	m.SetDeadline(time.Now().Add(5 * time.Second))
	req, _ := json.Marshal(map[string]any{"op": "who", "port": la.Port, "src": ra.Port})
	if _, err := m.Write(append(req, '\n')); err != nil {
		return nodeapi.Caller{}, fmt.Errorf("%w: %v", errMeshdDown, err)
	}
	line, err := bufio.NewReader(io.LimitReader(m, 8192)).ReadBytes('\n')
	if err != nil {
		return nodeapi.Caller{}, fmt.Errorf("%w: %v", errMeshdDown, err)
	}
	var out struct {
		Op     string `json:"op"`
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Net    string `json:"net"`
		Device string `json:"device"`
		IP     string `json:"ip"`
		Host   string `json:"host"`
		Name   string `json:"name"`
		User   string `json:"user"`
	}
	if err := json.Unmarshal(line, &out); err != nil {
		return nodeapi.Caller{}, err
	}
	if out.Op != "whois" || !out.OK {
		// Старый meshd спотыкается о «who» раньше разбора операции.
		if strings.Contains(out.Error, "нет ключа сети") || strings.Contains(out.Error, "неизвестная операция") {
			return nodeapi.Caller{}, errMeshdOld
		}
		return nodeapi.Caller{}, errors.New(out.Error)
	}
	return nodeapi.Caller{Net: out.Net, Device: out.Device, IP: out.IP, Host: out.Host, Name: out.Name, User: out.User}, nil
}

// ---------- сети ----------

func (h *Host) netDir(id string) string { return filepath.Join(h.opt.Dir, "nets", id) }

// netFor — данные сети id (в памяти; на диске создаётся по нужде).
func (h *Host) netFor(id string) (*netData, error) {
	if !nodeapi.ValidNet(id) {
		return nil, errors.New("неверный id сети")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	nd := h.nets[id]
	if nd == nil {
		nd = &netData{id: id, dir: h.netDir(id), last: map[string]int64{}, folderBy: map[string]int64{}}
		h.nets[id] = nd
	}
	return nd, nil
}

func (h *Host) folders(id string) []folderConf {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]folderConf(nil), h.conf.Folders[id]...)
}

func (h *Host) folder(id, fid string) (folderConf, bool) {
	for _, f := range h.folders(id) {
		if f.ID == fid {
			return f, true
		}
	}
	return folderConf{}, false
}

// hubOf — сервер папок сети; открывается при первой папке.
func (h *Host) hubOf(nd *netData) (*mirror.Hub, http.Handler, error) {
	nd.mu.Lock()
	defer nd.mu.Unlock()
	if nd.hub == nil {
		acl, err := h.aclLocked(nd)
		if err != nil {
			return nil, nil, err
		}
		hub, err := mirror.OpenHub(filepath.Join(nd.dir, "mirror", "hub"), "сервер")
		if err != nil {
			return nil, nil, err
		}
		// Доступ по устройствам: кто пришёл — из meshd (op who), id
		// устройства в meshd → PeerID, как его видят устройства.
		hub.Visible = func(r *http.Request, folder string) bool {
			return acl.Allowed(folder, nodeapi.PeerID(callerOf(r).Device))
		}
		// Квота — в самом сервере папок: проверка до записи и точный учёт
		// сразу после неё (и при копии уже лежащего содержимого).
		hub.Room = func(folder string, used int64) (int64, string) { return h.room(nd, folder, used) }
		hub.OnUsage = func(folder string, d int64) { h.grow(nd, folder, d) }
		nd.hub = hub
		nd.mirrorH = hub.Handler(func(r *http.Request) string {
			c := callerOf(r)
			if c.Host != "" {
				return c.Host
			}
			return c.IP
		})
	}
	return nd.hub, nd.mirrorH, nil
}

// aclLocked — список доступа к папкам сети (под nd.mu). Папки, которые
// уже были до первого открытия (обновление на ТЗ 18), — старые: видны
// всем до MigrateAccess.
func (h *Host) aclLocked(nd *netData) (*mirror.Access, error) {
	if nd.acl != nil {
		return nd.acl, nil
	}
	var ids []string
	for _, f := range h.folders(nd.id) {
		ids = append(ids, f.ID)
	}
	acl, err := mirror.OpenAccess(filepath.Join(nd.dir, "mirror", "access.json"), ids)
	if err != nil {
		return nil, err
	}
	nd.acl = acl
	return acl, nil
}

func (h *Host) aclOf(nd *netData) (*mirror.Access, error) {
	nd.mu.Lock()
	defer nd.mu.Unlock()
	return h.aclLocked(nd)
}

// SetFolderAccess — галочка устройства у папки сети (Folder "*" — у всех).
func (h *Host) SetFolderAccess(req nodeapi.AccessReq) error {
	nd, err := h.netFor(req.Net)
	if err != nil {
		return err
	}
	var ids []string
	if req.Folder == "*" {
		for _, f := range h.folders(nd.id) {
			ids = append(ids, f.ID)
		}
	} else if f, ok := h.folder(nd.id, req.Folder); ok {
		ids = []string{f.ID}
	} else {
		return errors.New("нет такой папки")
	}
	acl, err := h.aclOf(nd)
	if err != nil {
		return err
	}
	for _, f := range ids {
		if err := acl.Set(f, req.Device, req.Name, req.On); err != nil {
			return err
		}
	}
	h.wakeMirror(nd)
	return nil
}

// MigrateAccess — правило обновления: старые папки сети получают все
// устройства, которые сейчас знает meshd (их передаёт панель).
func (h *Host) MigrateAccess(req nodeapi.MigrateReq) error {
	nd, err := h.netFor(req.Net)
	if err != nil {
		return err
	}
	acl, err := h.aclOf(nd)
	if err != nil {
		return err
	}
	var known []mirror.Known
	for _, d := range req.Devices {
		known = append(known, mirror.Known{ID: d.ID, Name: d.Name})
	}
	did, err := acl.Migrate(known)
	if did {
		h.wakeMirror(nd)
		h.event(nodeapi.Event{Plugin: nodeapi.PluginMirror, Net: nd.id,
			Text: fmt.Sprintf("доступ к прежним папкам выдан %d устройствам сети", len(known))})
	}
	return err
}

// wakeMirror — доступ изменился: ждущие запросы устройств перепроверят его.
func (h *Host) wakeMirror(nd *netData) {
	nd.mu.Lock()
	hub := nd.hub
	nd.mu.Unlock()
	if hub != nil {
		hub.Changed()
	}
}

// backupOf — резервные копии сети; открываются при первом обращении.
func (h *Host) backupOf(nd *netData) *backup.Store {
	nd.mu.Lock()
	defer nd.mu.Unlock()
	if nd.backup == nil {
		st := backup.OpenStore(filepath.Join(nd.dir, "backup"))
		st.Room = func() (int64, string) { return h.room(nd, "", -1) }
		st.OnUsage = func(d int64) { h.grow(nd, "", d) }
		st.OnEvent = func(text string, isErr bool) {
			h.event(nodeapi.Event{Plugin: nodeapi.PluginBackup, Net: nd.id, Error: isErr, Text: text})
		}
		nd.backup = st
	}
	return nd.backup
}

func (nd *netData) boardOf() *clipshare.ServerBoard {
	nd.mu.Lock()
	defer nd.mu.Unlock()
	if nd.board == nil {
		nd.board = clipshare.OpenServerBoard(nd.dir, 0)
	}
	return nd.board
}

func (nd *netData) touch(plugin string, now time.Time) {
	nd.mu.Lock()
	nd.last[plugin] = now.Unix()
	nd.mu.Unlock()
}

// ---------- место ----------

// du — сколько байт в папке (файлы, без ссылок).
func du(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// recount — пересчитать место с диска.
func (h *Host) recount() {
	des, _ := os.ReadDir(filepath.Join(h.opt.Dir, "nets"))
	var total int64
	for _, d := range des {
		if !d.IsDir() || !nodeapi.ValidNet(d.Name()) {
			continue
		}
		nd, err := h.netFor(d.Name())
		if err != nil {
			continue
		}
		used := du(nd.dir)
		by := map[string]int64{}
		nd.mu.Lock()
		hub := nd.hub
		nd.mu.Unlock()
		for _, f := range h.folders(nd.id) {
			if hub != nil {
				if n, err := hub.Usage(f.ID); err == nil {
					by[f.ID] = n
					continue
				}
			}
			by[f.ID] = du(f.Path) + du(filepath.Join(nd.dir, "mirror", "hub", f.ID))
		}
		nd.mu.Lock()
		nd.used, nd.folderBy = used, by
		nd.mu.Unlock()
		total += used
	}
	h.mu.Lock()
	h.used, h.usedAt = total, h.opt.Now()
	h.mu.Unlock()
}

// grow — место изменилось на n байт (между пересчётами; n < 0 — убрано).
func (h *Host) grow(nd *netData, folder string, n int64) {
	if n == 0 {
		return
	}
	h.mu.Lock()
	h.used += n
	h.mu.Unlock()
	nd.mu.Lock()
	nd.used += n
	if folder != "" {
		nd.folderBy[folder] += n
	}
	nd.mu.Unlock()
}

// room — сколько ещё можно записать (folder — зеркальная папка или "";
// used — сколько папка занимает сейчас, -1 — взять из учёта).
func (h *Host) room(nd *netData, folder string, used int64) (int64, string) {
	h.mu.Lock()
	total := h.conf.Config.QuotaMB<<20 - h.used
	quota := h.conf.Config.QuotaMB
	h.mu.Unlock()
	left, why := total, fmt.Sprintf("на сервере кончилось место для плагинов (всего %d МБ) — освободи место или увеличь квоту в панели сервера", quota)
	if folder != "" {
		if f, ok := h.folder(nd.id, folder); ok && f.QuotaMB > 0 {
			if used < 0 {
				nd.mu.Lock()
				used = nd.folderBy[folder]
				nd.mu.Unlock()
			}
			fl := f.QuotaMB<<20 - used
			if fl < left {
				left, why = fl, fmt.Sprintf("папка «%s» на сервере заполнена (квота %d МБ) — освободи место или увеличь квоту в панели сервера", f.Name, f.QuotaMB)
			}
		}
	}
	return left, why
}

// errFull — место кончилось посреди загрузки.
type errFull struct{ why string }

func (e errFull) Error() string { return e.why }

// quotaReader пропускает не больше left байт, дальше — ошибка с объяснением.
type quotaReader struct {
	r    io.Reader
	left int64
	why  string
	n    int64
}

func (q *quotaReader) Read(p []byte) (int, error) {
	if q.left <= 0 {
		// Ровно в край: проверим, что дальше ничего нет.
		var one [1]byte
		if n, _ := q.r.Read(one[:]); n == 0 {
			return 0, io.EOF
		}
		return 0, errFull{q.why}
	}
	if int64(len(p)) > q.left {
		p = p[:q.left]
	}
	n, err := q.r.Read(p)
	q.left -= int64(n)
	q.n += int64(n)
	return n, err
}

// limitPut — проверка места перед загрузкой: не влезает — отказ сразу,
// длина неизвестна — тело обрезается на краю с понятной ошибкой. Ничего не
// портится: недогруженное лежит отдельным куском и в папку не попадает.
func (h *Host) limitPut(w http.ResponseWriter, r *http.Request, nd *netData, folder string) (*quotaReader, bool) {
	left, why := h.room(nd, folder, -1)
	if left <= 0 || (r.ContentLength > 0 && r.ContentLength > left) {
		h.event(nodeapi.Event{Net: nd.id, Error: true, Text: why})
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInsufficientStorage)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": why})
		return nil, false
	}
	q := &quotaReader{r: r.Body, left: left, why: why}
	r.Body = io.NopCloser(q)
	return q, true
}

// ---------- обработчики плагинов ----------

func (h *Host) pluginHandler(plugin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := callerOf(r)
		nd, err := h.netFor(c.Net)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		nd.touch(plugin, h.opt.Now())
		switch plugin {
		case nodeapi.PluginMirror:
			h.serveMirror(w, r, nd)
		case nodeapi.PluginFiles:
			h.serveFiles(w, r, nd)
		case nodeapi.PluginClipboard:
			if r.URL.Path == "/clip" {
				if _, ok := h.limitPut(w, r, nd, ""); !ok {
					return
				}
			}
			r = r.WithContext(clipshare.WithServerCaller(r.Context(), clipshare.ServerCaller{Device: c.Device, Name: c.Name}))
			nd.boardOf().Handler().ServeHTTP(w, r)
		case nodeapi.PluginBackup:
			r = r.WithContext(backup.WithCaller(r.Context(), backup.Caller{Device: c.Device, Name: c.Name, User: c.User}))
			h.backupOf(nd).Handler().ServeHTTP(w, r)
		}
	})
}

func (h *Host) serveMirror(w http.ResponseWriter, r *http.Request, nd *netData) {
	fs := h.folders(nd.id)
	if len(fs) == 0 {
		// Папок у этой сети нет — Hub не открываем (и ничего не создаём на
		// диске): устройства каждую минуту спрашивают /folders у всех.
		if r.URL.Path == "/folders" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			io.WriteString(w, "[]\n")
			return
		}
		http.Error(w, `{"error":"на сервере нет зеркальных папок этой сети"}`, http.StatusNotFound)
		return
	}
	_, handler, err := h.hubOf(nd)
	if err != nil {
		h.event(nodeapi.Event{Plugin: nodeapi.PluginMirror, Net: nd.id, Error: true, Text: err.Error()})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Квоту проверяет и учитывает сам сервер папок (hub.Room/OnUsage).
	handler.ServeHTTP(w, r)
}

func (h *Host) serveFiles(w http.ResponseWriter, r *http.Request, nd *netData) {
	dir := filepath.Join(nd.dir, "files")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.mu.Lock()
	write := h.conf.Config.FilesWrite
	h.mu.Unlock()
	src := func() []devfiles.Share {
		return []devfiles.Share{{ID: "server", Name: "Сервер", Path: dir, Write: write}}
	}
	c := callerOf(r)
	r = r.WithContext(devfiles.WithCaller(r.Context(), mesh.Caller{IP: c.IP, Host: c.Host}))
	var q *quotaReader
	if r.URL.Path == "/put" && r.Method == http.MethodPut && write {
		var ok bool
		if q, ok = h.limitPut(w, r, nd, ""); !ok {
			return
		}
	}
	devfiles.NewHandler(src, nil).ServeHTTP(w, r)
	if q != nil {
		h.grow(nd, "", q.n)
	}
}

// ---------- фоновые круги ----------

func (h *Host) loop() {
	defer h.wg.Done()
	h.recount()
	scan := time.NewTicker(30 * time.Second)
	defer scan.Stop()
	usage := time.NewTicker(h.opt.UsageEvery)
	defer usage.Stop()
	var lastPrune time.Time
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-scan.C:
			h.scanAll(time.Since(lastPrune) > time.Hour)
			if time.Since(lastPrune) > time.Hour {
				lastPrune = time.Now()
			}
		case <-usage.C:
			h.recount()
		}
	}
}

// scanAll — правки прямо на диске сервера (их нет, но так Hub ведёт себя
// одинаково) и чистка версий старше 30 дней, буфера — старше суток.
func (h *Host) scanAll(prune bool) {
	h.mu.Lock()
	nets := make([]*netData, 0, len(h.nets))
	for _, nd := range h.nets {
		nets = append(nets, nd)
	}
	h.mu.Unlock()
	for _, nd := range nets {
		nd.mu.Lock()
		hub, board := nd.hub, nd.board
		nd.mu.Unlock()
		if hub != nil {
			for _, f := range hub.Folders() {
				_ = hub.Scan(f.ID)
			}
			if prune {
				hub.Prune()
			}
		}
		if board != nil {
			board.Prune()
		}
	}
}

// ---------- управление (панель) ----------

// State — для панели.
func (h *Host) State() nodeapi.State {
	h.mu.Lock()
	st := nodeapi.State{Version: h.opt.Version, Config: h.conf.Config, Meshd: h.meshd, UsedBytes: h.used,
		Events: append([]nodeapi.Event{}, h.events...), Listening: map[string]bool{}}
	for p := range h.lns {
		st.Listening[p] = true
	}
	ids := map[string]bool{}
	for id := range h.conf.Folders {
		ids[id] = true
	}
	for id := range h.nets {
		ids[id] = true
	}
	h.mu.Unlock()
	des, _ := os.ReadDir(filepath.Join(h.opt.Dir, "nets"))
	for _, d := range des {
		if d.IsDir() && nodeapi.ValidNet(d.Name()) {
			ids[d.Name()] = true
		}
	}
	for id := range ids {
		nd, err := h.netFor(id)
		if err != nil {
			continue
		}
		n := nodeapi.Network{ID: id, Folders: []nodeapi.Folder{}}
		nd.mu.Lock()
		n.UsedBytes = nd.used
		if len(nd.last) > 0 {
			n.LastActive = map[string]int64{}
			for k, v := range nd.last {
				n.LastActive[k] = v
			}
		}
		hub := nd.hub
		by := map[string]int64{}
		for k, v := range nd.folderBy {
			by[k] = v
		}
		nd.mu.Unlock()
		var acl *mirror.Access
		if len(h.folders(id)) > 0 {
			acl, _ = h.aclOf(nd)
		}
		for _, f := range h.folders(id) {
			out := nodeapi.Folder{ID: f.ID, Name: f.Name, QuotaMB: f.QuotaMB, UsedBytes: by[f.ID], Devices: []string{}}
			if acl != nil {
				out.Devices, out.Old = acl.Devices(f.ID), acl.IsOld(f.ID)
			}
			if hub != nil {
				if rep, err := hub.Index(f.ID, 0, 0, nil); err == nil {
					for _, fi := range rep.Files {
						if !fi.Deleted && fi.Hash != mirror.DirHash {
							out.Files++
						}
					}
					out.Conflicts = len(rep.Conflicts)
				}
			}
			n.Folders = append(n.Folders, out)
		}
		if _, err := os.Stat(filepath.Join(nd.dir, "files")); err == nil {
			n.FilesBytes = du(filepath.Join(nd.dir, "files"))
		}
		if _, err := os.Stat(filepath.Join(nd.dir, "clipboard")); err == nil {
			bs := nd.boardOf().Stats()
			n.Clipboard = nodeapi.Clipboard{Items: bs.Items, Bytes: bs.Bytes, Devices: bs.Devices}
		}
		if _, err := os.Stat(filepath.Join(nd.dir, "backup")); err == nil {
			st := h.backupOf(nd)
			for _, b := range st.AllSets() {
				n.Backups = append(n.Backups, nodeapi.BackupSet{Scope: b.Scope, Device: b.Device, DeviceName: b.DeviceName,
					Set: b.Set, Name: b.Name, Keep: b.Keep, Versions: b.Versions, Bytes: b.Bytes, Last: b.Last})
			}
			n.BackupBytes = st.Stored()
		}
		st.Networks = append(st.Networks, n)
	}
	sort.Slice(st.Networks, func(i, j int) bool { return st.Networks[i].ID < st.Networks[j].ID })
	if st.Networks == nil {
		st.Networks = []nodeapi.Network{}
	}
	return st
}

// SetConfig — включить и выключить плагины, общая квота.
func (h *Host) SetConfig(p nodeapi.ConfigPatch) (nodeapi.State, error) {
	h.mu.Lock()
	c := &h.conf.Config
	if p.Mirror != nil {
		c.Mirror = *p.Mirror
	}
	if p.Files != nil {
		c.Files = *p.Files
	}
	if p.FilesWrite != nil {
		c.FilesWrite = *p.FilesWrite
	}
	if p.Clipboard != nil {
		c.Clipboard = *p.Clipboard
	}
	if p.Backup != nil {
		c.Backup = *p.Backup
	}
	if p.QuotaMB != nil {
		if *p.QuotaMB < 1 || *p.QuotaMB > 1<<24 {
			h.mu.Unlock()
			return nodeapi.State{}, errors.New("квота — от 1 МБ до 16 ТБ")
		}
		c.QuotaMB = *p.QuotaMB
	}
	err := h.saveLocked()
	if err == nil {
		err = h.applyLocked()
	}
	h.mu.Unlock()
	return h.State(), err
}

func validFolderName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" || len([]rune(n)) > 64 || strings.ContainsAny(n, "\x00\r\n") {
		return "", errors.New("имя папки — от 1 до 64 символов")
	}
	return n, nil
}

// AddFolder — зеркальная папка сети на сервере.
func (h *Host) AddFolder(req nodeapi.FolderReq) (nodeapi.Folder, error) {
	name, err := validFolderName(req.Name)
	if err != nil {
		return nodeapi.Folder{}, err
	}
	if req.QuotaMB < 0 {
		return nodeapi.Folder{}, errors.New("квота не может быть меньше нуля")
	}
	if req.QuotaMB == 0 {
		req.QuotaMB = nodeapi.DefaultFolderQuotaMB
	}
	nd, err := h.netFor(req.Net)
	if err != nil {
		return nodeapi.Folder{}, err
	}
	for _, f := range h.folders(nd.id) {
		if strings.EqualFold(f.Name, name) {
			return nodeapi.Folder{}, fmt.Errorf("папка «%s» в этой сети уже есть", name)
		}
	}
	if len(h.folders(nd.id)) >= 32 {
		return nodeapi.Folder{}, errors.New("в сети не больше 32 зеркальных папок")
	}
	hub, _, err := h.hubOf(nd)
	if err != nil {
		return nodeapi.Folder{}, err
	}
	path := filepath.Join(nd.dir, "mirror", "folders", randName())
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nodeapi.Folder{}, err
	}
	hf, err := hub.AddFolder(path, name)
	if err != nil {
		os.RemoveAll(path)
		return nodeapi.Folder{}, err
	}
	fc := folderConf{ID: hf.ID, Name: name, Path: path, QuotaMB: req.QuotaMB}
	h.mu.Lock()
	h.conf.Folders[nd.id] = append(h.conf.Folders[nd.id], fc)
	err = h.saveLocked()
	h.eventLocked(nodeapi.Event{Plugin: nodeapi.PluginMirror, Net: nd.id, Text: "добавлена папка «" + name + "»"})
	h.mu.Unlock()
	return nodeapi.Folder{ID: fc.ID, Name: fc.Name, QuotaMB: fc.QuotaMB}, err
}

// RemoveFolder — перестать раздавать; deleteData — стереть и содержимое,
// и версии.
func (h *Host) RemoveFolder(req nodeapi.FolderReq) error {
	nd, err := h.netFor(req.Net)
	if err != nil {
		return err
	}
	f, ok := h.folder(nd.id, req.ID)
	if !ok {
		return errors.New("нет такой папки")
	}
	hub, _, err := h.hubOf(nd)
	if err != nil {
		return err
	}
	if err := hub.RemoveFolder(f.ID); err != nil {
		return err
	}
	if acl, err := h.aclOf(nd); err == nil {
		_ = acl.Remove(f.ID)
	}
	h.mu.Lock()
	list := h.conf.Folders[nd.id]
	out := list[:0]
	for _, x := range list {
		if x.ID != f.ID {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		delete(h.conf.Folders, nd.id)
	} else {
		h.conf.Folders[nd.id] = out
	}
	err = h.saveLocked()
	text := "убрана папка «" + f.Name + "» (данные оставлены)"
	if req.DeleteData {
		text = "убрана папка «" + f.Name + "» вместе с данными"
	}
	h.eventLocked(nodeapi.Event{Plugin: nodeapi.PluginMirror, Net: nd.id, Text: text})
	h.mu.Unlock()
	if req.DeleteData {
		os.RemoveAll(f.Path)
		os.RemoveAll(filepath.Join(nd.dir, "mirror", "hub", "removed-"+f.ID))
	}
	h.recount()
	return err
}

// SetFolderQuota — квота папки в МБ.
func (h *Host) SetFolderQuota(req nodeapi.FolderReq) error {
	if req.QuotaMB < 1 || req.QuotaMB > 1<<24 {
		return errors.New("квота — от 1 МБ до 16 ТБ")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.conf.Folders[req.Net]
	for i := range list {
		if list[i].ID == req.ID {
			list[i].QuotaMB = req.QuotaMB
			return h.saveLocked()
		}
	}
	return errors.New("нет такой папки")
}

// ClearClipboard — стереть буфер сети на сервере.
func (h *Host) ClearClipboard(netID string) error {
	nd, err := h.netFor(netID)
	if err != nil {
		return err
	}
	nd.boardOf().Clear()
	h.event(nodeapi.Event{Plugin: nodeapi.PluginClipboard, Net: nd.id, Text: "буфер очищен из панели"})
	return nil
}

// DeleteBackup — убрать резервную копию устройства (панель).
func (h *Host) DeleteBackup(req nodeapi.BackupReq) error {
	nd, err := h.netFor(req.Net)
	if err != nil {
		return err
	}
	if err := h.backupOf(nd).DeleteAny(req.Scope, req.Device, req.Set); err != nil {
		return err
	}
	h.event(nodeapi.Event{Plugin: nodeapi.PluginBackup, Net: nd.id, Text: "резервная копия убрана из панели"})
	return nil
}

// DeleteNet — стереть все данные плагинов сети (её удалили в meshd). Сам
// хост не знает, что сеть удалена, — решает и спрашивает человека панель.
func (h *Host) DeleteNet(netID string) error {
	if !nodeapi.ValidNet(netID) {
		return errors.New("неверный id сети")
	}
	h.mu.Lock()
	nd := h.nets[netID]
	delete(h.nets, netID)
	delete(h.conf.Folders, netID)
	err := h.saveLocked()
	h.mu.Unlock()
	if nd != nil {
		nd.mu.Lock()
		if nd.hub != nil {
			nd.hub.Close()
		}
		nd.mu.Unlock()
	}
	if e := os.RemoveAll(h.netDir(netID)); e != nil && err == nil {
		err = e
	}
	h.event(nodeapi.Event{Net: netID, Text: "данные удалённой сети стёрты из панели"})
	h.recount()
	return err
}

func randName() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// ControlHandler — управляющий вход для панели (nodeapi).
func (h *Host) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any, err error) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	post := func(path string, fn func(body []byte) (any, error)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST", http.StatusMethodNotAllowed)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
			if err != nil {
				reply(w, nil, err)
				return
			}
			v, err := fn(body)
			reply(w, v, err)
		})
	}
	ok := map[string]bool{"ok": true}
	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) { reply(w, h.State(), nil) })
	post("/config", func(b []byte) (any, error) {
		var p nodeapi.ConfigPatch
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		return h.SetConfig(p)
	})
	folderReq := func(b []byte) (nodeapi.FolderReq, error) {
		var req nodeapi.FolderReq
		err := json.Unmarshal(b, &req)
		return req, err
	}
	post("/folder/add", func(b []byte) (any, error) {
		req, err := folderReq(b)
		if err != nil {
			return nil, err
		}
		return h.AddFolder(req)
	})
	post("/folder/remove", func(b []byte) (any, error) {
		req, err := folderReq(b)
		if err != nil {
			return nil, err
		}
		return ok, h.RemoveFolder(req)
	})
	post("/folder/quota", func(b []byte) (any, error) {
		req, err := folderReq(b)
		if err != nil {
			return nil, err
		}
		return ok, h.SetFolderQuota(req)
	})
	post("/backup/delete", func(b []byte) (any, error) {
		var req nodeapi.BackupReq
		if err := json.Unmarshal(b, &req); err != nil {
			return nil, err
		}
		return ok, h.DeleteBackup(req)
	})
	post("/net/delete", func(b []byte) (any, error) {
		var req struct {
			Net string `json:"net"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			return nil, err
		}
		return ok, h.DeleteNet(req.Net)
	})
	post("/folder/access", func(b []byte) (any, error) {
		var req nodeapi.AccessReq
		if err := json.Unmarshal(b, &req); err != nil {
			return nil, err
		}
		return ok, h.SetFolderAccess(req)
	})
	post("/folder/migrate", func(b []byte) (any, error) {
		var req nodeapi.MigrateReq
		if err := json.Unmarshal(b, &req); err != nil {
			return nil, err
		}
		return ok, h.MigrateAccess(req)
	})
	post("/clipboard/clear", func(b []byte) (any, error) {
		var req struct {
			Net string `json:"net"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			return nil, err
		}
		return ok, h.ClearClipboard(req.Net)
	})
	return mux
}
