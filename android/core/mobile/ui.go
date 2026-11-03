package mobile

// Общие страницы интерфейса на телефоне: тот же HTML, что в окне ПК
// (sshtunnel/internal/uiassets), и то же API дополнительных приложений и
// Книги (sshtunnel/internal/uihost). Kotlin показывает их в WebView
// (WebPageActivity) — новая кнопка на странице появляется на телефоне без
// кода под Android. Сервер живёт, пока живёт процесс приложения, и от VPN
// не зависит.

import (
	"encoding/base64"
	"encoding/json"
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

// StarredJSON — приложения со звёздочкой для быстрого доступа на главном
// экране телефона: [{"id","name"}] на языке lang. Ходит в тот же API, что и
// страницы, — ответ одинаковый с ПК.
func StarredJSON(lang string) string {
	uiMu.Lock()
	h := uiHost
	uiMu.Unlock()
	if h == nil {
		return "[]"
	}
	req, _ := http.NewRequest("GET", h.URL()+"/api/addons?lang="+lang, nil)
	req.Header.Set("X-Token", h.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "[]"
	}
	defer resp.Body.Close()
	var st struct {
		Catalog []struct {
			ID   string            `json:"id"`
			Name map[string]string `json:"name"`
		} `json:"catalog"`
		Added []struct {
			ID     string `json:"id"`
			Star   bool   `json:"star"`
			Active bool   `json:"active"`
		} `json:"added"`
	}
	if json.NewDecoder(resp.Body).Decode(&st) != nil {
		return "[]"
	}
	type item struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	out := []item{}
	for _, e := range st.Added {
		if !e.Star || !e.Active {
			continue
		}
		for _, m := range st.Catalog {
			if m.ID == e.ID {
				n := m.Name[lang]
				if n == "" {
					n = m.Name["ru"]
				}
				out = append(out, item{e.ID, n})
			}
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}
