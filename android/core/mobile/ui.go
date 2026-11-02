package mobile

// Общие страницы интерфейса на телефоне: тот же HTML, что в окне ПК
// (sshtunnel/internal/uiassets), и то же API дополнительных приложений и
// Книги (sshtunnel/internal/uihost). Kotlin показывает их в WebView
// (WebPageActivity) — новая кнопка на странице появляется на телефоне без
// кода под Android. Сервер живёт, пока живёт процесс приложения, и от VPN
// не зависит.

import (
	"encoding/json"
	"net/http"
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
