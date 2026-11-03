// Package uiassets — страницы интерфейса, одни на все платформы: окно ПК и
// Linux (internal/webui) и WebView на Android (internal/uihost через
// android/core/mobile). Новая кнопка на странице появляется везде сразу, без
// кода под Android.
package uiassets

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed *.html *.css *.js *.svg icons/*.svg addons/*.html vendor/*
var files embed.FS

// FS — все файлы интерфейса.
func FS() fs.FS { return files }

// ReadFile читает файл интерфейса по имени («index.html», «icons/add.svg»).
func ReadFile(name string) ([]byte, error) { return files.ReadFile(name) }

// Static отдаёт файлы интерфейса. Страницы приложений (addons/…) сюда не
// входят: их отдаёт addons — только добавленным и включённым.
func Static() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if strings.HasPrefix(name, "addons/") {
			http.NotFound(w, r)
			return
		}
		Serve(w, r, name)
	}
}

// Serve отдаёт один файл интерфейса. Кэш выключен: после обновления
// программы окно должно показать новую страницу, а не старую из кэша.
func Serve(w http.ResponseWriter, r *http.Request, name string) {
	data, err := files.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := "application/octet-stream"
	switch path.Ext(name) {
	case ".html":
		ct = "text/html; charset=utf-8"
	case ".css":
		ct = "text/css; charset=utf-8"
	case ".js":
		ct = "text/javascript; charset=utf-8"
	case ".svg":
		ct = "image/svg+xml"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}
