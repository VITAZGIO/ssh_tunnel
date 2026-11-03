package addons

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"sync"

	"sshtunnel/internal/uiassets"
)

// Module — часть ядра, которая работает за приложением (например, учёт
// трафика). Запускается, только пока приложение добавлено, работает на этом
// устройстве и (для фоновых) включено.
type Module interface {
	Start() error
	Stop()
}

var (
	regMu     sync.Mutex
	modules   = map[string]func() Module{}
	appRoutes = map[string][]Route{}
	dataDir   string
)

// Register задаёт модуль ядра для приложения id. Зовётся из init пакета
// приложения.
func Register(id string, f func() Module) {
	regMu.Lock()
	modules[id] = f
	regMu.Unlock()
}

// RegisterRoutes — адреса API приложения id. Отвечают, только пока
// приложение работает (Active); охрану ключом ставит тот, кто подключает
// Routes. Зовётся из init пакета приложения.
func RegisterRoutes(id string, rs ...Route) {
	regMu.Lock()
	appRoutes[id] = append(appRoutes[id], rs...)
	regMu.Unlock()
}

// AppRoutes — адреса API приложения id без проверки Active: для окна
// VPN-версии Windows, где их обслуживает процесс окна (рабочий стол,
// ключи пользователя), а список приложений хранит служба.
func AppRoutes(id string) []Route {
	regMu.Lock()
	defer regMu.Unlock()
	return append([]Route(nil), appRoutes[id]...)
}

// DataDir — папка данных программы на этом устройстве (там addons.json и
// данные приложений).
func DataDir() string {
	regMu.Lock()
	defer regMu.Unlock()
	return dataDir
}

// Opener открывает отдельное окно (ПК с нативным окном). key — чьё окно (id
// приложения или «book»): по нему запоминается место; url — путь страницы
// («/book.html?…»).
type Opener func(key, title, url string) error

// Service — дополнительные приложения этого устройства: хранилище, модули,
// API.
type Service struct {
	store *Store

	mu      sync.Mutex
	running map[string]Module
	opener  Opener
	log     func(format string, args ...any)

	dataMu sync.Mutex // data.json своих приложений
}

// New открывает хранилище в dir и запускает модули добавленных приложений.
func New(dir string) *Service {
	regMu.Lock()
	dataDir = dir
	regMu.Unlock()
	loadCustom(dir)
	s := &Service{store: Open(dir), running: map[string]Module{}}
	s.sync()
	return s
}

// Store — что добавлено на этом устройстве.
func (s *Service) Store() *Store { return s.store }

// SetOpener — как открывать отдельные окна; nil — не умеем (браузер, телефон).
func (s *Service) SetOpener(o Opener) {
	s.mu.Lock()
	s.opener = o
	s.mu.Unlock()
}

// SetLog — куда писать ошибки модулей.
func (s *Service) SetLog(f func(format string, args ...any)) {
	s.mu.Lock()
	s.log = f
	s.mu.Unlock()
}

// Active — работает ли приложение сейчас: добавлено, доступно на этом
// устройстве и не выключено. Только такое запускается и открывается.
func (s *Service) Active(id string) bool {
	m, ok := Find(id)
	if !ok || Unavailable(m, "") != "" {
		return false
	}
	e, ok := s.store.Get(id)
	if !ok {
		return false
	}
	return !(m.Background && e.Off)
}

// sync приводит запущенные модули в соответствие со списком: лишние
// останавливает, недостающие запускает.
func (s *Service) sync() {
	regMu.Lock()
	reg := make(map[string]func() Module, len(modules))
	for k, v := range modules {
		reg[k] = v
	}
	regMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, m := range s.running {
		if !s.Active(id) {
			m.Stop()
			delete(s.running, id)
		}
	}
	for id, f := range reg {
		if _, ok := s.running[id]; ok || !s.Active(id) {
			continue
		}
		m := f()
		if err := m.Start(); err != nil {
			if s.log != nil {
				s.log("Приложение %s не запустилось: %v", id, err)
			}
			continue
		}
		s.running[id] = m
	}
}

// Close останавливает все модули.
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, m := range s.running {
		m.Stop()
		delete(s.running, id)
	}
}

// Route — адрес API приложений (вливается в общую таблицу internal/webui и
// internal/uihost).
type Route struct {
	Path    string
	Methods string
	Desc    string
	H       http.HandlerFunc
}

// Routes — адреса API. Охрану (ключ) ставит тот, кто их подключает.
func (s *Service) Routes() []Route {
	rs := []Route{
		{"/api/addons", "GET, POST", "Дополнительные приложения: каталог и добавленные на этом устройстве. POST {\"op\":\"add|remove|star|on|seen\",\"id\",\"value\":bool}.", s.handleAddons},
		{"/api/addons/file", "POST", "Своё приложение из файла: тело — zip (manifest.json, HTML/JS/CSS, значок). ?dry=1 — только проверить и показать разрешения; без него — поставить.", s.handleFile},
		{"/api/app/data", "GET, POST", "Данные своего приложения (его ключом): GET — всё, POST {\"key\",\"value\"} (null — удалить). С ключом программы — ?app=<id>.", s.handleAppData},
		{"/api/window", "POST", "Открыть отдельное окно {\"key\",\"title\",\"url\"} (ПК). {\"ok\":false} — не умею, открывай сам.", s.handleWindow},
	}
	regMu.Lock()
	defer regMu.Unlock()
	for _, m := range catalog {
		for _, rt := range appRoutes[m.ID] {
			rt.H = s.onlyActive(m.ID, rt.H)
			rs = append(rs, rt)
		}
	}
	return rs
}

// onlyActive — адрес приложения отвечает, только пока оно работает.
func (s *Service) onlyActive(id string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Active(id) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "приложение не добавлено или выключено"})
			return
		}
		h(w, r)
	}
}

type catalogItem struct {
	Manifest
	Unavailable string `json:"unavailable,omitempty"`
}

type addedItem struct {
	Entry
	On     bool `json:"on"`
	Active bool `json:"active"`
}

func (s *Service) state(lang string) map[string]any {
	cat := []catalogItem{}
	for _, m := range Catalog() {
		cat = append(cat, catalogItem{m, Unavailable(m, lang)})
	}
	added := []addedItem{}
	for _, e := range s.store.Added() {
		if _, ok := Find(e.ID); !ok {
			continue // приложение исчезло из каталога этой сборки
		}
		added = append(added, addedItem{e, !e.Off, s.Active(e.ID)})
	}
	s.mu.Lock()
	windows := s.opener != nil
	s.mu.Unlock()
	return map[string]any{"catalog": cat, "added": added, "windows": windows, "platform": goos}
}

func (s *Service) handleAddons(w http.ResponseWriter, r *http.Request) {
	lang := r.URL.Query().Get("lang")
	if r.Method == http.MethodPost {
		var req struct {
			Op    string `json:"op"`
			ID    string `json:"id"`
			Value bool   `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]string{"error": "нужен JSON {\"op\",\"id\"}"})
			return
		}
		var err error
		switch req.Op {
		case "add":
			err = s.store.Add(req.ID)
		case "remove":
			err = s.store.Remove(req.ID)
			if err == nil && isCustom(req.ID) {
				err = uninstall(req.ID)
			}
		case "star":
			err = s.store.SetStar(req.ID, req.Value)
		case "on":
			err = s.store.SetOn(req.ID, req.Value)
		case "seen":
			err = s.store.SetSeen(req.ID)
		default:
			writeJSON(w, map[string]string{"error": "op: add, remove, star, on или seen"})
			return
		}
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		s.sync()
	}
	writeJSON(w, s.state(lang))
}

func (s *Service) handleWindow(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	open := s.opener
	s.mu.Unlock()
	WindowHandler(open, s.Active)(w, r)
}

// WindowHandler — POST /api/window: открыть отдельное окно. Нужен и окну
// VPN-версии Windows, где ядро в службе, а окна открывает процесс окна.
// active — можно ли открывать приложение (nil — не проверять: проверит
// служба, отдавая страницу). open == nil — {"ok":false}: страница откроет окно
// сама (браузер).
func WindowHandler(open Opener, active func(id string) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "нужен POST", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Key   string `json:"key"`
			Title string `json:"title"`
			URL   string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" ||
			!strings.HasPrefix(req.URL, "/") || strings.HasPrefix(req.URL, "//") {
			writeJSON(w, map[string]any{"ok": false, "error": "нужен JSON {\"key\",\"title\",\"url\":\"/…\"}"})
			return
		}
		if req.Key != "book" && active != nil && !active(req.Key) {
			writeJSON(w, map[string]any{"ok": false, "error": "приложение не добавлено или выключено"})
			return
		}
		if open == nil {
			writeJSON(w, map[string]any{"ok": false})
			return
		}
		if err := open(req.Key, req.Title, req.URL); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}
}

// Page отдаёт страницу приложения (/addons/<id>.html) — только работающего:
// не добавленное и выключенное не открывается.
func (s *Service) Page(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	id := strings.TrimSuffix(strings.TrimPrefix(name, "addons/"), ".html")
	m, ok := Find(id)
	if !ok || m.Page != name || !s.Active(id) {
		http.NotFound(w, r)
		return
	}
	uiassets.Serve(w, r, name)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
