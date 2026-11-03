package addons

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleZip — образец из docs/custom-app-sample одним архивом.
func sampleZip(t *testing.T) []byte {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "docs", "custom-app-sample")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		w, _ := zw.Create(e.Name())
		w.Write(b)
	}
	zw.Close()
	return buf.Bytes()
}

func zipOf(files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, d := range files {
		w, _ := zw.Create(n)
		w.Write([]byte(d))
	}
	zw.Close()
	return buf.Bytes()
}

func upload(t *testing.T, s *Service, body []byte, dry bool) map[string]any {
	t.Helper()
	u := "/api/addons/file"
	if dry {
		u += "?dry=1"
	}
	w := httptest.NewRecorder()
	s.handleFile(w, httptest.NewRequest("POST", u, bytes.NewReader(body)))
	var r map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err, w.Body.String())
	}
	return r
}

// guarded — как охрана webui/uihost пускает запрос с ключом приложения.
func guarded(s *Service, method, path, key string, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	for _, rt := range s.Routes() {
		mux.HandleFunc(rt.Path, rt.H)
	}
	ok := func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]bool{"ok": true}) }
	mux.HandleFunc("/api/state", ok)
	mux.HandleFunc("/api/mesh", ok)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Origin", "null")
	r2, err := s.Authorize(r)
	if err != nil {
		DenyJSON(w, err)
		return w
	}
	CORS(mux).ServeHTTP(w, r2)
	return w
}

// TestCustomSample — образец ставится, видит состояние, но без разрешения
// «устройства» списка устройств не получает; «убрать» удаляет всё.
func TestCustomSample(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	zb := sampleZip(t)

	dry := upload(t, s, zb, true)
	if dry["error"] != nil || dry["id"] != "hello" {
		t.Fatalf("проверка архива: %v", dry)
	}
	if ps := dry["permissions"].([]any); len(ps) != 1 {
		t.Fatalf("разрешения: %v", ps)
	}
	if _, ok := s.store.Get("hello"); ok {
		t.Fatal("?dry=1 поставил приложение")
	}

	st := upload(t, s, zb, false)
	if st["error"] != nil || st["installed"] != "hello" {
		t.Fatalf("установка: %v", st)
	}
	var m Manifest
	for _, c := range Catalog() {
		if c.ID == "hello" {
			m = c
		}
	}
	if !m.Custom || !strings.HasPrefix(m.Page, "custom/hello/app_") || m.IconURL == "" {
		t.Fatalf("в каталоге: %+v", m)
	}
	key := strings.Split(m.Page, "/")[2]

	if w := guarded(s, "GET", "/api/state", key, ""); w.Code != 200 {
		t.Fatalf("состояние: %d %s", w.Code, w.Body)
	}
	if w := guarded(s, "GET", "/api/mesh", key, ""); w.Code != 403 || !strings.Contains(w.Body.String(), "устройства") {
		t.Fatalf("устройства без разрешения: %d %s", w.Code, w.Body)
	}
	if w := guarded(s, "POST", "/api/state", key, `{"tunnel":false}`); w.Code != 403 {
		t.Fatalf("кнопки без разрешения: %d", w.Code)
	}
	if w := guarded(s, "GET", "/api/addons", key, ""); w.Code != 403 {
		t.Fatalf("список приложений (с ключами) своему приложению: %d", w.Code)
	}
	if w := guarded(s, "GET", "/api/state", "app_0000", ""); w.Code != 403 {
		t.Fatalf("чужой ключ: %d", w.Code)
	}
	if w := guarded(s, "GET", "/api/state", key, ""); w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("нет CORS для песочницы")
	}

	// Свои данные.
	if w := guarded(s, "POST", "/api/app/data", key, `{"key":"count","value":3}`); w.Code != 200 {
		t.Fatalf("запись данных: %d %s", w.Code, w.Body)
	}
	if w := guarded(s, "GET", "/api/app/data", key, ""); !strings.Contains(w.Body.String(), `"count":3`) {
		t.Fatalf("данные: %s", w.Body)
	}

	// Файлы — только с ключом в адресе, страница в песочнице.
	w := httptest.NewRecorder()
	s.Files(w, httptest.NewRequest("GET", "/"+m.Page, nil))
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox allow-scripts") {
		t.Fatalf("страница: %d %v", w.Code, w.Header())
	}
	w = httptest.NewRecorder()
	s.Files(w, httptest.NewRequest("GET", "/custom/hello/app_bad/index.html", nil))
	if w.Code != 404 {
		t.Fatalf("страница с чужим ключом: %d", w.Code)
	}

	// После перезапуска всё на месте, ключ тот же.
	s2 := New(dir)
	if a := appByKey(key); a == nil || !s2.Active("hello") {
		t.Fatal("после перезапуска приложения нет")
	}

	post(t, s2, `{"op":"remove","id":"hello"}`)
	if _, err := os.Stat(filepath.Join(dir, "custom", "hello")); !os.IsNotExist(err) {
		t.Fatal("убранное приложение оставило файлы или данные")
	}
	if appByKey(key) != nil {
		t.Fatal("ключ убранного приложения работает")
	}
}

// TestCustomRejects — без исполняемого кода, без чужих id и путей.
func TestCustomRejects(t *testing.T) {
	s := New(t.TempDir())
	man := `{"id":"x1","name":"X","permissions":[]}`
	cases := map[string]map[string]string{
		"exe":        {"manifest.json": man, "index.html": "<p>", "tool.exe": "MZ"},
		"dll":        {"manifest.json": man, "index.html": "<p>", "lib/a.dll": "MZ"},
		"no page":    {"manifest.json": man},
		"bad id":     {"manifest.json": `{"id":"X Y","name":"X"}`, "index.html": "<p>"},
		"catalog id": {"manifest.json": `{"id":"calendar","name":"X"}`, "index.html": "<p>"},
		"bad perm":   {"manifest.json": `{"id":"x1","name":"X","permissions":["root"]}`, "index.html": "<p>"},
		"traversal":  {"manifest.json": man, "index.html": "<p>", "../evil.js": "1"},
	}
	for name, files := range cases {
		if r := upload(t, s, zipOf(files), true); r["error"] == nil {
			t.Errorf("%s: принято %v", name, r)
		}
	}
	// Архив папкой целиком — можно.
	if r := upload(t, s, zipOf(map[string]string{"my/manifest.json": man, "my/index.html": "<p>"}), true); r["error"] != nil {
		t.Errorf("архив с папкой: %v", r)
	}
}
