// Package calendar — плагин «Календарь» (ТЗ 7): календари
// Nextcloud по CalDAV вместо DAVx5. Логика синхронизации — здесь, одна на
// все платформы; Android только переносит события в системный календарь и
// рисует виджет (android/app/.../calendar).
package calendar

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"sshtunnel/internal/addons"
)

// ID — приложение в каталоге (internal/addons/manifest.go).
const ID = "calendar"

func init() {
	addons.Register(ID, func() addons.Module { return eng })
	addons.RegisterRoutes(ID,
		addons.Route{Path: "/api/calendar", Methods: "GET",
			Desc: "Календарь: аккаунт (без пароля), календари, последняя сверка, ошибка, конфликт.", H: handleState},
		addons.Route{Path: "/api/calendar/account", Methods: "POST",
			Desc: "Подключить Nextcloud {\"url\",\"user\",\"password\",\"contacts\":bool} (password \"\" — прежний) или {\"forget\":true}.", H: handleAccount},
		addons.Route{Path: "/api/calendar/sync", Methods: "POST",
			Desc: "Сверить с сервером сейчас (ждёт конца сверки).", H: handleSync},
		addons.Route{Path: "/api/calendar/events", Methods: "GET",
			Desc: "События видимых календарей ?from=2026-11-01&to=2026-12-01 (повторы развёрнуты; &all=1 — и скрытых).", H: handleEvents},
		addons.Route{Path: "/api/calendar/event", Methods: "POST",
			Desc: "Создать (без href) или изменить событие {\"cal\",\"href\",\"title\",\"start\",\"end\",\"allDay\",\"location\",\"description\"}. {\"conflict\":true} — изменили в другом месте.", H: handleEvent},
		addons.Route{Path: "/api/calendar/delete", Methods: "POST",
			Desc: "Удалить событие {\"href\"} (у повторяющегося — всю серию).", H: handleDelete},
		addons.Route{Path: "/api/calendar/settings", Methods: "POST",
			Desc: "{\"cal\",\"shown\":bool} — показывать календарь; {\"widget\":bool} — виджет на рабочем столе ПК.", H: handleSettings},
	)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	writeJSON(w, map[string]any{"error": err.Error(), "conflict": errors.Is(err, ErrConflict)})
}

func post(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "нужен POST", http.StatusMethodNotAllowed)
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, map[string]string{"error": "нужен JSON"})
		return false
	}
	return true
}

func handleState(w http.ResponseWriter, r *http.Request) { writeJSON(w, eng.Status()) }

func handleAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		User     string `json:"user"`
		Password string `json:"password"`
		Contacts bool   `json:"contacts"`
		Forget   bool   `json:"forget"`
	}
	if !post(w, r, &req) {
		return
	}
	var err error
	if req.Forget {
		err = eng.Forget()
	} else {
		err = eng.SetAccount(Account{URL: req.URL, User: req.User, Contacts: req.Contacts}, req.Password)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, eng.Status())
}

func handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "нужен POST", http.StatusMethodNotAllowed)
		return
	}
	if err := eng.SyncNow(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, eng.Status())
}

// parseDay — «2026-11-02» в местном времени или RFC 3339.
func parseDay(s string, def time.Time) time.Time {
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return def
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now()
	from := parseDay(q.Get("from"), time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local))
	to := parseDay(q.Get("to"), from.AddDate(0, 1, 0))
	if to.Sub(from) > 400*24*time.Hour {
		to = from.AddDate(1, 1, 0)
	}
	ev := eng.Events(from, to, q.Get("all") == "1")
	if ev == nil {
		ev = []Instance{}
	}
	writeJSON(w, map[string]any{"events": ev, "rev": eng.Rev()})
}

func handleEvent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cal  string `json:"cal"`
		Href string `json:"href"`
		Edit
	}
	if !post(w, r, &req) {
		return
	}
	href, err := eng.SaveEvent(req.Cal, req.Href, req.Edit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "href": href})
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Href string `json:"href"`
	}
	if !post(w, r, &req) {
		return
	}
	if err := eng.DeleteEvent(req.Href); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func handleSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cal    string `json:"cal"`
		Shown  *bool  `json:"shown"`
		Widget *bool  `json:"widget"`
	}
	if !post(w, r, &req) {
		return
	}
	var err error
	if req.Cal != "" && req.Shown != nil {
		err = eng.SetShown(req.Cal, *req.Shown)
	}
	if err == nil && req.Widget != nil {
		err = eng.SetWidget(*req.Widget)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, eng.Status())
}
