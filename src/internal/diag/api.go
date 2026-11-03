package diag

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/addons"
)

// Service — диагностика одного устройства: хранилище, API, модуль
// молчаливой проверки утечек.
type Service struct {
	h      Host
	st     *Store
	med    *Meddlers
	dir    string
	active func(id string) bool
	busy   sync.Mutex // одна проба «Кто трогает» за раз
}

// New готовит диагностику. dir — папка настроек; active — работает ли
// приложение (addons.Service.Active): выключенное не отвечает. Модуль
// молчаливой проверки утечек регистрируется в каталоге приложений — вызывать
// до addons.New.
func New(h Host, dir string, active func(string) bool) *Service {
	st := OpenStore(dir)
	s := &Service{h: h, st: st, med: &Meddlers{h: h, st: st}, dir: dir, active: active}
	addons.Register(AppLeaks, func() addons.Module { return &Watch{h: h, st: st} })
	return s
}

// SetActive — проверка «приложение работает» (когда каталог создан после).
func (s *Service) SetActive(f func(string) bool) { s.active = f }

// Route — адрес API (как у addons).
type Route = addons.Route

// Routes — адреса API диагностики.
func (s *Service) Routes() []Route {
	return []Route{
		{Path: "/api/diag/site", Methods: "GET, POST", Desc: "Проверить сайт: GET — история (10) и «популярные»; POST {\"site\"} — проверка напрямую и через VPN по шагам DNS → TCP → TLS → HTTP, итог одной фразой.", H: s.app(AppSite, s.handleSite)},
		{Path: "/api/diag/site/popular", Methods: "POST", Desc: "«Проверить популярные»: {\"list\":[…]} — сохранить список; {\"check\":true} — проверить все.", H: s.app(AppSite, s.handlePopular)},
		{Path: "/api/diag/meddlers", Methods: "GET, POST", Desc: "Кто трогает трафик: GET — последний поиск и «Что я отключал»; POST {} — искать заново.", H: s.app(AppMeddlers, s.handleMeddlers)},
		{Path: "/api/diag/meddlers/", Methods: "POST", Desc: "Действие: /try {\"id\",\"site\"} (это и есть [Разрешаю]: остановить, перепроверить, не помогло — вернуть) | /keep {\"id\"} (отключить насовсем — только автозапуск) | /restore {\"id\"} | /reconcile {\"id\",\"site\"}.", H: s.app(AppMeddlers, s.handleMeddlerAction)},
		{Path: "/api/diag/leaks", Methods: "GET, POST", Desc: "Проверка утечек: GET — последний результат; POST — проверить сейчас (адрес, DNS, IPv6, что исправить).", H: s.app(AppLeaks, s.handleLeaks)},
		{Path: "/api/diag/report", Methods: "GET, POST", Desc: "Отчёт для поддержки: GET — что будет внутри (секреты уже ***); POST {\"op\":\"save\"|\"share\"} — zip в «Загрузки» / поделиться.", H: s.app(AppReport, s.handleReport)},
	}
}

// app — адрес работает, только пока приложение добавлено и включено.
func (s *Service) app(id string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.active != nil && !s.active(id) {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]string{"error": "приложение не добавлено или выключено"})
			return
		}
		h(w, r)
	}
}

func lang(r *http.Request) string {
	if r.URL.Query().Get("lang") == "en" {
		return "en"
	}
	return "ru"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	err := json.NewDecoder(r.Body).Decode(v)
	if err != nil && err.Error() == "EOF" {
		return nil
	}
	return err
}

func (s *Service) handleSite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		d := s.st.read()
		writeJSON(w, map[string]any{"history": d.History, "popular": d.Popular})
		return
	}
	var req struct {
		Site string `json:"site"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	res, err := CheckSite(ctx, s.h, req.Site, lang(r))
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	s.st.addHistory(res)
	writeJSON(w, res)
}

func (s *Service) handlePopular(w http.ResponseWriter, r *http.Request) {
	var req struct {
		List  []string `json:"list"`
		Check bool     `json:"check"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if req.List != nil {
		var clean []string
		for _, x := range req.List {
			if x = strings.TrimSpace(x); x != "" {
				clean = append(clean, x)
			}
		}
		s.st.update(func(d *storeData) { d.Popular = clean })
	}
	if !req.Check {
		writeJSON(w, map[string]any{"popular": s.st.read().Popular})
		return
	}
	list := s.st.read().Popular
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	out := make([]SiteResult, len(list))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, site := range list {
		wg.Add(1)
		go func(i int, site string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if res, err := CheckSite(ctx, s.h, site, lang(r)); err == nil {
				out[i] = res
			} else {
				out[i] = SiteResult{Site: site, Text: err.Error()}
			}
		}(i, site)
	}
	wg.Wait()
	writeJSON(w, map[string]any{"results": out})
}

func (s *Service) handleMeddlers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		sc := s.med.Scan(ctx, lang(r))
		writeJSON(w, map[string]any{"scan": sc, "disabled": s.st.read().Disabled})
		return
	}
	d := s.st.read()
	writeJSON(w, map[string]any{"scan": d.Meddlers, "disabled": d.Disabled})
}

func (s *Service) handleMeddlerAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "нужен POST", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID   string `json:"id"`
		Site string `json:"site"`
	}
	if err := decode(r, &req); err != nil || req.ID == "" {
		writeJSON(w, map[string]string{"error": "нужен JSON {\"id\"}"})
		return
	}
	s.busy.Lock()
	defer s.busy.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	l := lang(r)
	var (
		res any
		err error
	)
	switch strings.TrimPrefix(r.URL.Path, "/api/diag/meddlers/") {
	case "try":
		res, err = s.med.Try(ctx, req.ID, req.Site, l)
	case "keep":
		res, err = s.med.Keep(req.ID)
	case "restore":
		err = s.med.Restore(req.ID)
		res = map[string]bool{"ok": err == nil}
	case "reconcile":
		res, err = s.med.Reconcile(ctx, req.ID, req.Site, l)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error(), "disabled": s.st.read().Disabled})
		return
	}
	writeJSON(w, map[string]any{"result": res, "disabled": s.st.read().Disabled})
}

func (s *Service) handleLeaks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, map[string]any{"report": s.st.read().Leaks})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	rep := CheckLeaks(ctx, s.h, lang(r))
	s.st.update(func(d *storeData) { d.Leaks = &rep })
	writeJSON(w, map[string]any{"report": rep})
}

func (s *Service) handleReport(w http.ResponseWriter, r *http.Request) {
	files := BuildReport(s.h, s.st, s.dir)
	if r.Method != http.MethodPost {
		writeJSON(w, map[string]any{"files": files})
		return
	}
	var req struct {
		Op string `json:"op"`
	}
	_ = decode(r, &req)
	p, err := SaveReport(files)
	if err == nil && req.Op == "share" {
		err = ShareFile(p)
	}
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error(), "path": p})
		return
	}
	writeJSON(w, map[string]string{"path": p})
}
