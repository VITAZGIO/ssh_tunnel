package mobile

// Общие страницы интерфейса на телефоне: тот же HTML, что в окне ПК
// (sshtunnel/internal/uiassets), и то же API дополнительных приложений и
// Книги (sshtunnel/internal/uihost). Kotlin показывает их в WebView
// (WebPageActivity) — новая кнопка на странице появляется на телефоне без
// кода под Android. Сервер живёт, пока живёт процесс приложения, и от VPN
// не зависит.

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"

	"sshtunnel/internal/uihost"
)

var (
	uiMu   sync.Mutex
	uiHost *uihost.Host
)

// StartUI поднимает сервер страниц (один раз за процесс) и отдаёт его адрес
// «http://127.0.0.1:порт». dir — папка данных приложения (filesDir): там
// список добавленных приложений, свой на этом телефоне.
func StartUI(dir string) (string, error) {
	uiMu.Lock()
	defer uiMu.Unlock()
	if uiHost == nil {
		h, err := uihost.Start(dir)
		if err != nil {
			return "", err
		}
		uiHost = h
	}
	return uiHost.URL(), nil
}

// API — остальное API главного экрана (связь, настройки, серверы, выбор
// приложений), которое на телефоне держит приложение: те же адреса и тот же
// JSON, что у webui на ПК (docs/API.md). Handle получает метод, путь,
// строку запроса без «?» и тело и отдаёт ответ: JSON или "png:<base64>"
// для картинки (значок приложения).
type API interface {
	Handle(method, path, query, body string) string
}

// SetAPI подключает обработчик приложения к серверу страниц.
func SetAPI(a API) {
	uiMu.Lock()
	h := uiHost
	uiMu.Unlock()
	if h == nil {
		return
	}
	h.SetAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		out := a.Handle(r.Method, r.URL.Path, r.URL.RawQuery, string(body))
		if png, ok := strings.CutPrefix(out, "png:"); ok {
			b, err := base64.StdEncoding.DecodeString(png)
			if err != nil || len(b) == 0 {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "max-age=86400")
			w.Write(b)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, out)
	}))
}

// Emit отправляет событие экрану (поток /events): готовый JSON вида
// {"kind":"core",…} — те же виды, что у webui на ПК.
func Emit(event string) {
	uiMu.Lock()
	h := uiHost
	uiMu.Unlock()
	if h != nil {
		h.Emit([]byte(event))
	}
}

// UIToken — ключ к API страниц (передаётся страницам параметром t).
func UIToken() string {
	uiMu.Lock()
	defer uiMu.Unlock()
	if uiHost == nil {
		return ""
	}
	return uiHost.Token()
}
