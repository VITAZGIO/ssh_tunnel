package addons

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeModule struct{ started, stopped *int }

func (m fakeModule) Start() error { *m.started++; return nil }
func (m fakeModule) Stop()        { *m.stopped++ }

func post(t *testing.T, s *Service, body string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleAddons(w, httptest.NewRequest("POST", "/api/addons", bytes.NewBufferString(body)))
	var r map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r["error"] != nil {
		t.Fatalf("%s: %v", body, r["error"])
	}
	return r
}

// TestAddStarSurvivesRestart — добавил, звёздочка, выключил: после
// перезапуска (новое хранилище из того же файла) всё на месте.
func TestAddStarSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	post(t, s, `{"op":"add","id":"sample"}`)
	post(t, s, `{"op":"star","id":"sample","value":true}`)
	post(t, s, `{"op":"on","id":"sample","value":false}`)
	post(t, s, `{"op":"seen","id":"sample"}`)
	post(t, s, `{"op":"win","id":"sample","value":false}`)

	e, ok := New(dir).store.Get("sample")
	if !ok || !e.Star || !e.Off || !e.Seen || e.Win == nil || *e.Win {
		t.Fatalf("после перезапуска: %+v, %v", e, ok)
	}
	post(t, s, `{"op":"remove","id":"sample"}`)
	if _, ok := New(dir).store.Get("sample"); ok {
		t.Fatal("убранное вернулось после перезапуска")
	}
}

// TestPerDevice — у другого устройства (другая папка) список свой.
func TestPerDevice(t *testing.T) {
	a, b := New(t.TempDir()), New(t.TempDir())
	post(t, a, `{"op":"add","id":"sample"}`)
	if len(b.store.Added()) != 0 {
		t.Fatal("добавленное на одном устройстве появилось на другом")
	}
}

// TestModuleRunsOnlyWhenActive — модуль ядра работает только у добавленного и
// включённого приложения.
func TestModuleRunsOnlyWhenActive(t *testing.T) {
	var started, stopped int
	Register("sample", func() Module { return fakeModule{&started, &stopped} })
	defer func() { regMu.Lock(); delete(modules, "sample"); regMu.Unlock() }()

	s := New(t.TempDir())
	if started != 0 {
		t.Fatal("модуль не добавленного приложения запустился")
	}
	post(t, s, `{"op":"add","id":"sample"}`)
	if started != 1 {
		t.Fatalf("после добавления запусков %d, нужно 1", started)
	}
	post(t, s, `{"op":"on","id":"sample","value":false}`)
	if stopped != 1 {
		t.Fatalf("после выключения остановок %d, нужна 1", stopped)
	}
	post(t, s, `{"op":"on","id":"sample","value":true}`)
	post(t, s, `{"op":"remove","id":"sample"}`)
	if started != 2 || stopped != 2 {
		t.Fatalf("запусков %d, остановок %d, нужно 2 и 2", started, stopped)
	}
}

// TestPageOnlyWhenActive — страница не добавленного или выключенного
// приложения не отдаётся.
func TestPageOnlyWhenActive(t *testing.T) {
	s := New(t.TempDir())
	get := func() int {
		w := httptest.NewRecorder()
		s.Page(w, httptest.NewRequest("GET", "/addons/sample.html", nil))
		return w.Code
	}
	if c := get(); c != http.StatusNotFound {
		t.Fatalf("не добавленное: %d", c)
	}
	post(t, s, `{"op":"add","id":"sample"}`)
	if c := get(); c != http.StatusOK {
		t.Fatalf("добавленное: %d", c)
	}
	post(t, s, `{"op":"on","id":"sample","value":false}`)
	if c := get(); c != http.StatusNotFound {
		t.Fatalf("выключенное: %d", c)
	}
}

// TestSampleHiddenInRelease — образец в каталоге только у сборки «dev».
func TestSampleHiddenInRelease(t *testing.T) {
	defer func(f func() bool) { showDev = f }(showDev)
	showDev = func() bool { return false }
	if _, ok := Find("sample"); ok {
		t.Fatal("образец виден в релизе")
	}
	if err := Open(t.TempDir()).Add("sample"); err == nil {
		t.Fatal("в релизе образец добавился")
	}
}

// TestUnavailable — приложение не для этой платформы: серое с причиной и не
// запускается.
func TestUnavailable(t *testing.T) {
	m := Manifest{ID: "x", Platforms: []string{"nope"}}
	if Unavailable(m, "ru") == "" {
		t.Fatal("нет причины")
	}
	if Unavailable(Manifest{ID: "y"}, "ru") != "" {
		t.Fatal("без списка платформ — работает везде")
	}
}

// TestWindowHandler — без умения открывать окна ответ ok:false (страница
// откроет окно браузера), выключенное приложение не открывается.
func TestWindowHandler(t *testing.T) {
	call := func(open Opener, body string) map[string]any {
		w := httptest.NewRecorder()
		WindowHandler(open, func(string) bool { return false })(w,
			httptest.NewRequest("POST", "/api/window", bytes.NewBufferString(body)))
		var r map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &r)
		return r
	}
	var opened string
	open := func(key, title, url string) error { opened = key + " " + url; return nil }
	if r := call(nil, `{"key":"book","url":"/book.html"}`); r["ok"] != false {
		t.Fatalf("без окон: %v", r)
	}
	if r := call(open, `{"key":"book","url":"/book.html"}`); r["ok"] != true || opened != "book /book.html" {
		t.Fatalf("Книга: %v, %q", r, opened)
	}
	if r := call(open, `{"key":"sample","url":"/apps.html"}`); r["ok"] != false {
		t.Fatalf("выключенное открылось: %v", r)
	}
	if r := call(open, `{"key":"book","url":"//evil.example/"}`); r["ok"] != false {
		t.Fatalf("чужой адрес принят: %v", r)
	}
}
