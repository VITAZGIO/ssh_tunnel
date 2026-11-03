package addons

// Свои приложения (ТЗ 8, п. 19): человек ставит приложение из файла — архив
// с manifest.json, HTML/JS/CSS и значком. Своего кода в систему такое
// приложение не ставит: в архиве разрешены только файлы страниц, а
// работает страница в песочнице (CSP sandbox — свой непрозрачный origin, к
// окну программы и её ключу доступа нет) и ходит к ядру только через API
// со своим ключом. Ключ пускает лишь туда, что разрешил человек при
// установке (permissions). Формат и образец — docs/CUSTOM_APPS.md.
//
// Хранится в <папка данных>/custom/<id>/: app.json (описание и ключ),
// files/ (файлы архива), data.json (данные приложения, /api/app/data).
// «Убрать» удаляет папку целиком.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Permission — что можно своему приложению: текст для экрана разрешений и
// адреса API, которые оно открывает.
type Permission struct {
	ID   string            `json:"id"`
	Text map[string]string `json:"text"`
	// routes — «МЕТОДЫ путь»: путь и всё под ним («путь$» — только он сам);
	// методы через запятую.
	routes []string
}

// permissions — все разрешения, которые может попросить своё приложение.
// GET /api и свои данные (/api/app/data) доступны всегда.
var permissions = []Permission{
	{ID: "state", Text: map[string]string{"ru": "Видеть, включён ли туннель и сеть устройств", "en": "See whether the tunnel and the device network are on"},
		routes: []string{"GET /api/state"}},
	{ID: "control", Text: map[string]string{"ru": "Включать и выключать туннель и сеть устройств", "en": "Turn the tunnel and the device network on and off"},
		routes: []string{"POST /api/state"}},
	{ID: "devices", Text: map[string]string{"ru": "Видеть устройства сети", "en": "See the devices of your network"},
		routes: []string{"GET /api/mesh", "GET /api/netmap", "GET /api/files/devices", "GET /api/ssh/devices"}},
	{ID: "rules", Text: map[string]string{"ru": "Менять правила: что идёт через сервер, что напрямую", "en": "Change the rules: what goes via the server, what goes direct"},
		routes: []string{"GET,POST /api/rules"}},
	{ID: "files", Text: map[string]string{"ru": "Файлы других устройств и свои открытые папки", "en": "Files of other devices and your shared folders"},
		routes: []string{"GET,POST /api/files"}},
	{ID: "traffic", Text: map[string]string{"ru": "Видеть расход трафика", "en": "See the traffic usage"},
		routes: []string{"GET /api/traffic"}},
	{ID: "clipboard", Text: map[string]string{"ru": "Общий буфер обмена", "en": "The shared clipboard"},
		routes: []string{"GET,POST /api/clipboard"}},
	{ID: "calendar", Text: map[string]string{"ru": "Календарь: смотреть и менять события", "en": "Calendar: view and change events"},
		routes: []string{"GET,POST /api/calendar"}},
	{ID: "sitecheck", Text: map[string]string{"ru": "Проверять, открываются ли сайты", "en": "Check whether sites open"},
		routes: []string{"GET,POST /api/diag/site"}},
}

// alwaysRoutes — доступно любому своему приложению.
var alwaysRoutes = []string{"GET /api$", "GET,POST /api/app/data"}

// Permissions — все разрешения (для экрана и документации).
func Permissions() []Permission { return append([]Permission(nil), permissions...) }

func findPermission(id string) (Permission, bool) {
	for _, p := range permissions {
		if p.ID == id {
			return p, true
		}
	}
	return Permission{}, false
}

// routeAllowed — подходит ли запрос под правило «МЕТОДЫ путь».
func routeAllowed(rule, method, p string) bool {
	ms, prefix, _ := strings.Cut(rule, " ")
	okMethod := false
	for _, m := range strings.Split(ms, ",") {
		if m == method {
			okMethod = true
		}
	}
	if exact, ok := strings.CutSuffix(prefix, "$"); ok {
		return okMethod && p == exact
	}
	return okMethod && (p == prefix || strings.HasPrefix(p, prefix+"/"))
}

// customApp — установленное своё приложение.
type customApp struct {
	Manifest
	Key     string `json:"key"`
	Version string `json:"version,omitempty"`
}

var (
	customMu sync.Mutex
	customs  = map[string]*customApp{}
)

const appKeyPrefix = "app_"

func customRoot(dir string) string { return filepath.Join(dir, "custom") }

// loadCustom читает установленные свои приложения из папки данных.
func loadCustom(dir string) {
	customMu.Lock()
	defer customMu.Unlock()
	customs = map[string]*customApp{}
	ents, _ := os.ReadDir(customRoot(dir))
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(customRoot(dir), e.Name(), "app.json"))
		if err != nil {
			continue
		}
		var c customApp
		if json.Unmarshal(b, &c) != nil || c.ID != e.Name() || c.Key == "" {
			continue
		}
		c.Custom = true
		customs[c.ID] = &c
	}
}

// customCatalog — свои приложения в виде строк каталога: страница и значок
// — адреса /custom/<id>/<ключ>/…, ключ в адресе и есть пропуск к файлам.
func customCatalog() []Manifest {
	customMu.Lock()
	defer customMu.Unlock()
	out := make([]Manifest, 0, len(customs))
	for _, c := range customs {
		m := c.Manifest
		base := "custom/" + c.ID + "/" + c.Key + "/"
		m.Page = base + c.Page
		if c.Icon != "" {
			m.IconURL = "/" + base + c.Icon
		}
		m.Permissions = append([]string{}, c.Permissions...)
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func isCustom(id string) bool {
	customMu.Lock()
	defer customMu.Unlock()
	return customs[id] != nil
}

// appByKey — своё приложение по ключу.
func appByKey(key string) *customApp {
	customMu.Lock()
	defer customMu.Unlock()
	for _, c := range customs {
		if subtle.ConstantTimeCompare([]byte(c.Key), []byte(key)) == 1 {
			return c
		}
	}
	return nil
}

// RequestToken — ключ из запроса: Authorization: Bearer, X-Token или ?t=.
func RequestToken(r *http.Request) string {
	if t := r.Header.Get("X-Token"); t != "" {
		return t
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return r.URL.Query().Get("t")
}

// IsAppKey — ключ своего приложения (а не ключ программы из api.json).
func IsAppKey(tok string) bool { return strings.HasPrefix(tok, appKeyPrefix) }

type ctxKey struct{}

// AppOf — какое своё приложение прислало запрос ("" — не своё приложение,
// а окно программы, команда или MCP с ключом программы).
func AppOf(r *http.Request) string {
	id, _ := r.Context().Value(ctxKey{}).(string)
	return id
}

// errPermission — у приложения нет разрешения на адрес.
type errPermission struct{ need string }

func (e errPermission) Error() string {
	if e.need == "" {
		return "своему приложению этот адрес недоступен"
	}
	return "у приложения нет разрешения «" + e.need + "»"
}

// Authorize проверяет запрос с ключом своего приложения: ключ верный,
// приложение работает, разрешение на этот адрес у него есть. Возвращает
// запрос, в контексте которого — id приложения (AppOf).
func (s *Service) Authorize(r *http.Request) (*http.Request, error) {
	c := appByKey(RequestToken(r))
	if c == nil || !s.Active(c.ID) {
		return nil, errors.New("неверный ключ приложения")
	}
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	p := path.Clean(r.URL.Path)
	ok := false
	for _, rule := range alwaysRoutes {
		ok = ok || routeAllowed(rule, method, p)
	}
	for _, id := range c.Permissions {
		if pm, found := findPermission(id); found {
			for _, rule := range pm.routes {
				ok = ok || routeAllowed(rule, method, p)
			}
		}
	}
	if !ok {
		need := ""
		for _, pm := range permissions {
			for _, rule := range pm.routes {
				if routeAllowed(rule, method, p) {
					need = pm.Text["ru"]
				}
			}
		}
		return nil, errPermission{need}
	}
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, c.ID)), nil
}

// DenyJSON — ответ «нельзя» для своего приложения (403 с текстом).
func DenyJSON(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// CORS — свои приложения живут в песочнице с непрозрачным origin, и браузер
// шлёт их запросы к API как чужие (Origin: null). Им отвечаем с
// разрешением CORS; пускает ли запрос — решает ключ (Authorize). Остальным
// ничего не меняется.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") == "null" {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", "*")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Token")
			h.Set("Access-Control-Allow-Methods", "GET, POST")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Files отдаёт файлы своего приложения: /custom/<id>/<ключ>/<файл>. Ключ
// программы тут не нужен и не передаётся — пропуск даёт ключ приложения в
// адресе. Страница получает CSP sandbox: свой origin, без доступа к окну
// программы, запросы — только к этому же адресу (API ядра).
func (s *Service) Files(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/custom/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 3 || parts[2] == "" {
		http.NotFound(w, r)
		return
	}
	c := appByKey(parts[1])
	if c == nil || c.ID != parts[0] || !s.Active(c.ID) {
		http.NotFound(w, r)
		return
	}
	name := path.Clean("/" + parts[2])[1:]
	b, err := os.ReadFile(filepath.Join(customRoot(DataDir()), c.ID, "files", filepath.FromSlash(name)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	self := "http://" + r.Host
	h := w.Header()
	h.Set("Content-Type", contentType(name))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-modals allow-downloads; "+
		"default-src 'self' "+self+" data: blob:; script-src 'self' "+self+" 'unsafe-inline'; "+
		"style-src 'self' "+self+" 'unsafe-inline'; connect-src 'self' "+self+"; "+
		"frame-ancestors 'self' "+self)
	w.Write(b)
}

// allowedExt — что может лежать в архиве. Ни exe, ни библиотек, ни скриптов
// оболочки: только то, что умеет страница браузера.
var allowedExt = map[string]string{
	".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
	".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8", ".json": "application/json; charset=utf-8",
	".txt": "text/plain; charset=utf-8", ".md": "text/plain; charset=utf-8",
	".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".ico": "image/x-icon",
	".woff": "font/woff", ".woff2": "font/woff2",
}

func contentType(name string) string {
	if ct, ok := allowedExt[strings.ToLower(path.Ext(name))]; ok {
		return ct
	}
	return "application/octet-stream"
}

const (
	maxArchive  = 20 << 20 // архив
	maxUnpacked = 50 << 20 // всё распакованное
	maxAppData  = 1 << 20  // data.json приложения
)

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// texts — строка или {"ru","en"}.
type texts map[string]string

func (t *texts) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*t = texts{"ru": s, "en": s}
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*t = m
	return nil
}

// fileManifest — manifest.json в архиве.
type fileManifest struct {
	ID          string   `json:"id"`
	Name        texts    `json:"name"`
	Hint        texts    `json:"hint"`
	Icon        string   `json:"icon"`
	Page        string   `json:"page"`
	Permissions []string `json:"permissions"`
	Version     string   `json:"version"`
	Window      bool     `json:"window"`
}

// unpacked — разобранный архив.
type unpacked struct {
	man   fileManifest
	files map[string][]byte
}

// readArchive разбирает и проверяет архив своего приложения.
func readArchive(b []byte) (*unpacked, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, errors.New("это не zip-архив")
	}
	files := map[string][]byte{}
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := strings.ReplaceAll(f.Name, "\\", "/")
		clean := path.Clean("/" + name)[1:]
		if clean != name || strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			return nil, fmt.Errorf("странный путь в архиве: %s", f.Name)
		}
		if _, ok := allowedExt[strings.ToLower(path.Ext(name))]; !ok {
			return nil, fmt.Errorf("в архиве файл %s: можно только страницы, скрипты, стили, картинки и шрифты — никакого исполняемого кода", name)
		}
		total += int64(f.UncompressedSize64)
		if total > maxUnpacked {
			return nil, errors.New("распакованный архив больше 50 МБ")
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxUnpacked+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		files[name] = data
	}
	// Архив с одной папкой сверху (zip папки целиком) — снимаем её.
	if _, ok := files["manifest.json"]; !ok {
		var top string
		for n := range files {
			if strings.HasSuffix(n, "/manifest.json") && strings.Count(n, "/") == 1 {
				top = strings.TrimSuffix(n, "manifest.json")
			}
		}
		if top == "" {
			return nil, errors.New("в архиве нет manifest.json")
		}
		inner := map[string][]byte{}
		for n, d := range files {
			if strings.HasPrefix(n, top) {
				inner[strings.TrimPrefix(n, top)] = d
			}
		}
		files = inner
	}
	var m fileManifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		return nil, fmt.Errorf("manifest.json: %v", err)
	}
	if !idRe.MatchString(m.ID) {
		return nil, errors.New("manifest.json: id — 2–32 знака: строчные латинские буквы, цифры, дефис")
	}
	for _, c := range catalog {
		if c.ID == m.ID {
			return nil, fmt.Errorf("id «%s» занят приложением из каталога", m.ID)
		}
	}
	if m.Name["ru"] == "" && m.Name["en"] == "" {
		return nil, errors.New("manifest.json: нет name")
	}
	if m.Page == "" {
		m.Page = "index.html"
	}
	if ext := strings.ToLower(path.Ext(m.Page)); ext != ".html" && ext != ".htm" {
		return nil, errors.New("manifest.json: page — HTML-файл")
	}
	if _, ok := files[m.Page]; !ok {
		return nil, fmt.Errorf("в архиве нет страницы %s", m.Page)
	}
	if m.Icon != "" {
		if _, ok := files[m.Icon]; !ok {
			return nil, fmt.Errorf("в архиве нет значка %s", m.Icon)
		}
		if ext := strings.ToLower(path.Ext(m.Icon)); ext != ".svg" && ext != ".png" {
			return nil, errors.New("manifest.json: icon — SVG или PNG")
		}
	}
	seen := map[string]bool{}
	perms := []string{}
	for _, p := range m.Permissions {
		if _, ok := findPermission(p); !ok {
			return nil, fmt.Errorf("manifest.json: неизвестное разрешение «%s»", p)
		}
		if !seen[p] {
			seen[p] = true
			perms = append(perms, p)
		}
	}
	m.Permissions = perms
	return &unpacked{man: m, files: files}, nil
}

// permissionList — разрешения для экрана: id и текст на языке.
func permissionList(ids []string, lang string) []map[string]string {
	if lang != "en" {
		lang = "ru"
	}
	out := []map[string]string{}
	for _, id := range ids {
		if p, ok := findPermission(id); ok {
			out = append(out, map[string]string{"id": id, "text": p.Text[lang]})
		}
	}
	return out
}

// install кладёт приложение в папку данных. Обновление (тот же id) меняет
// файлы и разрешения, ключ и данные остаются.
func (s *Service) install(u *unpacked) error {
	root := customRoot(DataDir())
	dir := filepath.Join(root, u.man.ID)
	key := ""
	customMu.Lock()
	if old := customs[u.man.ID]; old != nil {
		key = old.Key
	}
	customMu.Unlock()
	if key == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		key = appKeyPrefix + hex.EncodeToString(b)
	}
	tmp := filepath.Join(root, "."+u.man.ID+".new")
	os.RemoveAll(tmp)
	for name, data := range u.files {
		p := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	os.RemoveAll(filepath.Join(dir, "files"))
	if err := os.Rename(tmp, filepath.Join(dir, "files")); err != nil {
		return err
	}
	c := customApp{
		Manifest: Manifest{ID: u.man.ID, Name: u.man.Name, Hint: u.man.Hint, Icon: u.man.Icon,
			Page: u.man.Page, Permissions: u.man.Permissions, Window: u.man.Window, Custom: true},
		Key: key, Version: u.man.Version,
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "app.json"), b, 0o600); err != nil {
		return err
	}
	customMu.Lock()
	customs[c.ID] = &c
	customMu.Unlock()
	return s.store.Add(c.ID)
}

// uninstall удаляет своё приложение целиком: файлы, ключ, данные.
func uninstall(id string) error {
	customMu.Lock()
	delete(customs, id)
	customMu.Unlock()
	return os.RemoveAll(filepath.Join(customRoot(DataDir()), id))
}

// handleFile — POST /api/addons/file: тело — zip своего приложения. С ?dry=1
// только проверяет и отдаёт, что приложение просит (экран разрешений);
// без него — ставит с этими разрешениями.
func (s *Service) handleFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "нужен POST", http.StatusMethodNotAllowed)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxArchive+1))
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if len(b) > maxArchive {
		writeJSON(w, map[string]string{"error": "архив больше 20 МБ"})
		return
	}
	u, err := readArchive(b)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	lang := r.URL.Query().Get("lang")
	if r.URL.Query().Get("dry") == "1" {
		writeJSON(w, map[string]any{
			"id": u.man.ID, "name": u.man.Name, "hint": u.man.Hint, "version": u.man.Version,
			"permissions": permissionList(u.man.Permissions, lang),
			"files":       len(u.files), "update": isCustom(u.man.ID),
		})
		return
	}
	if err := s.install(u); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	st := s.state(lang)
	st["installed"] = u.man.ID
	writeJSON(w, st)
}

// handleAppData — свои данные приложения: GET — всё, POST {"key","value"}
// — записать (value: null — удалить). Своё приложение видит только свои;
// с ключом программы — ?app=<id>.
func (s *Service) handleAppData(w http.ResponseWriter, r *http.Request) {
	id := AppOf(r)
	if id == "" {
		id = r.URL.Query().Get("app")
	}
	if !isCustom(id) {
		writeJSON(w, map[string]string{"error": "нет такого своего приложения"})
		return
	}
	p := filepath.Join(customRoot(DataDir()), id, "data.json")
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	data := map[string]json.RawMessage{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &data)
	}
	if r.Method == http.MethodPost {
		var req struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, maxAppData)).Decode(&req); err != nil || req.Key == "" {
			writeJSON(w, map[string]string{"error": "нужен JSON {\"key\",\"value\"}"})
			return
		}
		if len(req.Value) == 0 || string(req.Value) == "null" {
			delete(data, req.Key)
		} else {
			data[req.Key] = req.Value
		}
		b, _ := json.Marshal(data)
		if len(b) > maxAppData {
			writeJSON(w, map[string]string{"error": "данных приложения больше 1 МБ"})
			return
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, map[string]any{"data": data})
}
