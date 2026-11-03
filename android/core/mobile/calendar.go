package mobile

// Календарь на телефоне (ТЗ 7): сверка с Nextcloud — в ядре
// (sshtunnel/internal/calendar), Kotlin только переносит готовые события в
// системный календарь (CalendarContract), контакты — в ContactsContract,
// и рисует виджет. Сверку зовёт и модуль ядра (раз в 15 минут, пока жив
// процесс), и SyncAdapter Android (переживает перезагрузку).

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"sshtunnel/internal/calendar"
	core2 "sshtunnel/internal/core"
)

// CalendarListener — Kotlin: данные календаря изменились (переписать
// системный календарь и виджет).
type CalendarListener interface {
	CalendarChanged()
}

// SetCalendarListener подписывает Kotlin на изменения.
func SetCalendarListener(l CalendarListener) {
	calendar.Default().OnChange(func() { l.CalendarChanged() })
}

// dialSelf — соединение календаря к Nextcloud через ядро телефона: имя
// .mesh — в сеть устройств, домашний адрес — напрямую (решает core.Decide).
// Без работающего туннеля — напрямую (приложение исключено из своего VPN).
func (t *Tunnel) dialSelf(ctx context.Context, addr string) (net.Conn, error) {
	t.mu.Lock()
	tun := t.core
	t.mu.Unlock()
	if tun != nil {
		return tun.DialSelf(addr)
	}
	if core2.IsMeshTarget(addr) {
		return nil, errors.New("сеть устройств выключена — включи её на главном экране")
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// calendarActive — приложение «Календарь» добавлено и включено (его API
// отвечает только тогда).
func calendarActive(dir string) bool {
	if _, err := StartUI(dir); err != nil {
		return false
	}
	uiMu.Lock()
	h := uiHost
	uiMu.Unlock()
	req, _ := http.NewRequest("GET", h.URL()+"/api/calendar", nil)
	req.Header.Set("X-Token", h.Token())
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// CalendarSyncNow — сверка для SyncAdapter: {"active":false} — приложение
// не добавлено или выключено; иначе состояние и ошибка, если была. dir —
// папка данных приложения (filesDir).
func CalendarSyncNow(dir string) string {
	if !calendarActive(dir) {
		return `{"active":false}`
	}
	err := calendar.Default().SyncNow()
	out := map[string]any{"active": true, "status": calendar.Default().Status()}
	if err != nil {
		out["error"] = err.Error()
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// CalendarActive — для Kotlin: держать ли системный аккаунт и виджет.
func CalendarActive(dir string) bool { return calendarActive(dir) }

// CalendarRev — номер версии данных: изменился — пора переписать.
func CalendarRev() int64 { return calendar.Default().Rev() }

// CalendarExportJSON — все календари с главными событиями (повтор —
// правилом RRULE, его разворачивает системный календарь). Время — в мс.
func CalendarExportJSON() string {
	type ev struct {
		Href, ETag, UID, Title, Location, Description, TZ, RRule string
		Start, End, RecurrenceID                                 int64
		AllDay, Cancelled                                        bool
		ExDates                                                  []int64
	}
	type cal struct {
		Href, Name, Color string
		ReadOnly          bool
		Events            []ev
	}
	ms := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return t.UnixMilli()
	}
	var out []cal
	for _, c := range calendar.Default().Export() {
		oc := cal{Href: c.Href, Name: c.Name, Color: c.Color, ReadOnly: c.ReadOnly, Events: []ev{}}
		for _, e := range c.Events {
			x := ev{Href: e.Href, ETag: e.ETag, UID: e.UID, Title: e.Title, Location: e.Location,
				Description: e.Description, TZ: e.TZ, RRule: e.RRule, Start: ms(e.Start), End: ms(e.End),
				RecurrenceID: ms(e.RecurrenceID), AllDay: e.AllDay, Cancelled: e.Cancelled}
			for _, d := range e.ExDates {
				x.ExDates = append(x.ExDates, ms(d))
			}
			oc.Events = append(oc.Events, x)
		}
		out = append(out, oc)
	}
	if out == nil {
		out = []cal{}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// ContactsExportJSON — контакты (если включена галочка «Ещё и контакты»).
func ContactsExportJSON() string {
	b, _ := json.Marshal(calendar.Default().Contacts())
	return string(b)
}

// CalendarUpcomingJSON — события видимых календарей на days дней вперёд
// для виджета: [{"title","start","end","allDay","color"}], время в мс.
func CalendarUpcomingJSON(days int) string {
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	type it struct {
		Title  string `json:"title"`
		Start  int64  `json:"start"`
		End    int64  `json:"end"`
		AllDay bool   `json:"allDay"`
		Color  string `json:"color,omitempty"`
	}
	out := []it{}
	for _, e := range calendar.Default().Events(from, from.AddDate(0, 0, days), false) {
		if !e.AllDay && e.End.Before(now) {
			continue // уже прошло
		}
		out = append(out, it{e.Title, e.Start.UnixMilli(), e.End.UnixMilli(), e.AllDay, e.Color})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// SetTimeZone — часовой пояс телефона (TimeZone.getDefault().id). Go на
// Android сам его не знает и считает местным временем UTC: «сегодня» в
// виджете и события без пояса съехали бы.
func SetTimeZone(name string) {
	if loc, err := time.LoadLocation(name); err == nil {
		time.Local = loc
	}
}
