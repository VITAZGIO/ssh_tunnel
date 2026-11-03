package calendar

import (
	"context"
	"errors"
	"net"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Interval — как часто сверяться с сервером без правок (ТЗ 7: раз в 15 мин).
const Interval = 15 * time.Minute

// Engine — календарь этого устройства: аккаунт, сверка с сервером, правки.
// Один на процесс (модуль ядра приложения «Календарь»).
type Engine struct {
	syncMu sync.Mutex // сверка и правки по сети — по одной

	mu   sync.Mutex
	dir  string
	st   *state
	dial DialFunc
	busy bool

	stop    chan struct{}
	kick    chan struct{}
	changed []func()
}

var eng = &Engine{}

// Default — календарь процесса.
func Default() *Engine { return eng }

// Init задаёт папку данных (там calendar.json и зашифрованный пароль).
// Зовётся там же, где создаются плагины (webui, uihost).
func Init(dir string) {
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if eng.dir == dir && eng.st != nil {
		return
	}
	eng.dir = dir
	eng.st = loadState(dir)
}

// SetDial — как ходить к серверу: платформа отдаёт соединение «через ядро»
// (core.Decide: имя .mesh — в сеть устройств, домашний адрес — напрямую).
// nil — обычное соединение с этого устройства.
func SetDial(d DialFunc) {
	eng.mu.Lock()
	eng.dial = d
	eng.mu.Unlock()
}

// OnChange — позвать f после каждого изменения данных (телефон переписывает
// системный календарь и виджет).
func (e *Engine) OnChange(f func()) {
	e.mu.Lock()
	e.changed = append(e.changed, f)
	e.mu.Unlock()
}

func (e *Engine) notify() {
	e.mu.Lock()
	fs := append([]func(){}, e.changed...)
	e.mu.Unlock()
	for _, f := range fs {
		go f()
	}
}

// Start — модуль ядра: сверка сразу, потом раз в Interval и сразу после
// просьбы (Kick).
func (e *Engine) Start() error {
	e.mu.Lock()
	if e.st == nil {
		e.mu.Unlock()
		return errors.New("календарь не настроен (нет папки данных)")
	}
	if e.stop != nil {
		e.mu.Unlock()
		return nil
	}
	e.stop = make(chan struct{})
	e.kick = make(chan struct{}, 1)
	stop, kick := e.stop, e.kick
	e.mu.Unlock()
	go func() {
		t := time.NewTicker(Interval)
		defer t.Stop()
		for {
			if e.Configured() {
				_ = e.SyncNow()
			}
			select {
			case <-stop:
				return
			case <-t.C:
			case <-kick:
			}
		}
	}()
	return nil
}

// Stop останавливает фоновую сверку.
func (e *Engine) Stop() {
	e.mu.Lock()
	if e.stop != nil {
		close(e.stop)
		e.stop = nil
	}
	e.mu.Unlock()
}

// Kick — сверить как можно скорее (в фоне). Модуль не запущен (приложение
// выключено) — ничего: сверяется только работающее.
func (e *Engine) Kick() {
	e.mu.Lock()
	k := e.kick
	e.mu.Unlock()
	if k == nil {
		return
	}
	select {
	case k <- struct{}{}:
	default:
	}
}

// Configured — аккаунт задан.
func (e *Engine) Configured() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.st != nil && e.st.Account.URL != ""
}

func (e *Engine) client() (backend, error) {
	e.mu.Lock()
	if e.st == nil || e.st.Account.URL == "" {
		e.mu.Unlock()
		return nil, errors.New("календарь не подключён: выбери сервис и укажи адрес и вход")
	}
	acc, dir, dial := e.st.Account, e.dir, e.dial
	e.mu.Unlock()
	return newBackend(acc, loadPassword(dir), dial)
}

// SetAccount проверяет вход и сохраняет аккаунт; пароль "" — оставить
// прежний. Календари перечитываются с нуля.
func (e *Engine) SetAccount(acc Account, pass string) error {
	e.mu.Lock()
	dir, dial := e.dir, e.dial
	if e.st == nil {
		e.mu.Unlock()
		return errors.New("календарь не настроен")
	}
	e.mu.Unlock()
	if pass == "" {
		pass = loadPassword(dir)
	}
	if acc.Kind == KindHA {
		acc.Contacts = false // контактов у HA нет
	}
	c, err := newBackend(acc, pass, dial)
	if err != nil {
		return err
	}
	if _, err := c.Discover(false); err != nil {
		return err
	}
	if err := savePassword(dir, pass); err != nil {
		return err
	}
	e.mu.Lock()
	same := e.st.Account.URL == acc.URL && e.st.Account.User == acc.User && e.st.Account.Kind == acc.Kind
	e.st.Account = acc
	if !same {
		e.st.Collections = nil
	}
	e.st.LastError = ""
	e.st.Rev++
	err = e.st.save(dir)
	e.mu.Unlock()
	e.notify()
	e.Kick()
	return err
}

// Forget — отключить аккаунт: забыть пароль и всё скачанное.
func (e *Engine) Forget() error {
	e.mu.Lock()
	defer e.notify()
	defer e.mu.Unlock()
	if e.st == nil {
		return nil
	}
	rev := e.st.Rev
	*e.st = state{Rev: rev + 1, Widget: e.st.Widget}
	_ = removeFile(secretPath(e.dir))
	return e.st.save(e.dir)
}

// SyncNow — одна сверка со всеми календарями.
func (e *Engine) SyncNow() error {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.setBusy(true)
	defer e.setBusy(false)
	err := e.syncLocked()
	e.mu.Lock()
	if e.st != nil {
		e.st.LastSync = time.Now()
		e.st.LastError = ""
		if err != nil {
			e.st.LastError = err.Error()
		}
		_ = e.st.save(e.dir)
	}
	e.mu.Unlock()
	e.notify()
	return err
}

func (e *Engine) setBusy(b bool) {
	e.mu.Lock()
	e.busy = b
	e.mu.Unlock()
}

func (e *Engine) syncLocked() error {
	c, err := e.client()
	if err != nil {
		return err
	}
	e.mu.Lock()
	contacts := e.st.Account.Contacts
	e.mu.Unlock()
	cols, err := c.Discover(contacts)
	if err != nil {
		return err
	}

	// Список календарей — как на сервере; токены, скачанное и «скрыт»
	// у знакомых сохраняются.
	e.mu.Lock()
	var next []*colState
	for _, col := range cols {
		cs := e.st.col(col.Href)
		if cs == nil {
			cs = &colState{Items: map[string]item{}}
		}
		token, hidden, items := cs.Token, cs.Hidden, cs.Items
		*cs = colState{Collection: col, Token: token, Hidden: hidden, Items: items}
		next = append(next, cs)
	}
	if len(next) != len(e.st.Collections) {
		e.st.Rev++
	}
	e.st.Collections = next
	snap := make([]colState, len(next))
	for i, cs := range next {
		snap[i] = colState{Collection: cs.Collection, Token: cs.Token, Items: map[string]item{}}
		for h, it := range cs.Items {
			snap[i].Items[h] = it
		}
	}
	e.mu.Unlock()

	var firstErr error
	for _, cs := range snap {
		col := cs.Collection
		col.SyncToken = cs.Token
		ch, err := c.Changes(col)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var need []string
		for h, et := range ch.Changed {
			if old, ok := cs.Items[h]; !ok || old.ETag != et {
				need = append(need, h)
			}
		}
		got, err := c.Fetch(col, need)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		e.mu.Lock()
		live := e.st.col(col.Href)
		if live != nil {
			dirty := false
			for _, h := range ch.Removed {
				if _, ok := live.Items[h]; ok {
					delete(live.Items, h)
					dirty = true
				}
			}
			if ch.Full {
				for h := range live.Items {
					if _, ok := ch.Changed[h]; !ok {
						delete(live.Items, h)
						dirty = true
					}
				}
			}
			for h, v := range got {
				et := v[0]
				if et == "" {
					et = ch.Changed[h]
				}
				live.Items[h] = item{ETag: et, Data: v[1]}
				dirty = true
			}
			// Токен — только если всё нужное скачали: иначе пропущенное
			// потерялось бы до полной сверки.
			if len(got) >= len(need) {
				live.Token = ch.Token
			}
			if dirty {
				e.st.Rev++
			}
		}
		e.mu.Unlock()
	}
	return firstErr
}

// ── правки ───────────────────────────────────────────────────────────────

// SaveEvent создаёт (href "") или меняет событие в календаре cal. Сразу
// пишет на сервер; 412 — событие изменили в другом месте: перечитываем,
// возвращаем ErrConflict.
func (e *Engine) SaveEvent(cal, href string, ed Edit) (string, error) {
	if strings.TrimSpace(ed.Title) == "" {
		return "", errors.New("нужно название события")
	}
	if ed.Start.IsZero() {
		return "", errors.New("нужно время начала")
	}
	c, err := e.client()
	if err != nil {
		return "", err
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	e.mu.Lock()
	cs := e.st.col(cal)
	if cs == nil {
		e.mu.Unlock()
		return "", errors.New("нет такого календаря")
	}
	if cs.ReadOnly || cs.Contacts {
		e.mu.Unlock()
		return "", errors.New("этот календарь только для чтения")
	}
	var data, etag string
	now := time.Now()
	if href == "" {
		uid := NewUID()
		href = joinHref(cs.Href, strings.TrimSuffix(uid, "@ssh_tunnel")+".ics")
		data = BuildEvent(uid, ed, now)
	} else {
		old, ok := cs.Items[href]
		if !ok {
			e.mu.Unlock()
			return "", errors.New("событие уже удалено")
		}
		etag = old.ETag
		data = UpdateEvent(old.Data, ed, now)
	}
	e.mu.Unlock()

	newTag, err := c.Put(href, etag, data)
	if err != nil {
		return "", e.afterFail(err)
	}
	e.mu.Lock()
	if cs := e.st.col(cal); cs != nil {
		cs.Items[href] = item{ETag: newTag, Data: data}
	}
	e.st.Conflict = ""
	e.st.Rev++
	_ = e.st.save(e.dir)
	e.mu.Unlock()
	e.notify()
	e.Kick() // свежий ETag и токен — следующей сверкой
	return href, nil
}

// DeleteEvent удаляет событие (вся серия у повторяющегося). Подтверждение
// спрашивает экран.
func (e *Engine) DeleteEvent(href string) error {
	c, err := e.client()
	if err != nil {
		return err
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.mu.Lock()
	var cs *colState
	var it item
	for _, x := range e.st.Collections {
		if v, ok := x.Items[href]; ok {
			cs, it = x, v
		}
	}
	if cs == nil {
		e.mu.Unlock()
		return errors.New("событие уже удалено")
	}
	if cs.ReadOnly {
		e.mu.Unlock()
		return errors.New("этот календарь только для чтения")
	}
	e.mu.Unlock()
	if err := c.Delete(href, it.ETag); err != nil {
		return e.afterFail(err)
	}
	e.mu.Lock()
	delete(cs.Items, href)
	e.st.Conflict = ""
	e.st.Rev++
	_ = e.st.save(e.dir)
	e.mu.Unlock()
	e.notify()
	e.Kick()
	return nil
}

// afterFail — при конфликте сразу перечитываем: экран покажет версию
// сервера.
func (e *Engine) afterFail(err error) error {
	if !errors.Is(err, ErrConflict) {
		return err
	}
	e.mu.Lock()
	e.st.Conflict = err.Error()
	e.mu.Unlock()
	_ = e.syncLocked()
	e.mu.Lock()
	e.st.Rev++
	_ = e.st.save(e.dir)
	e.mu.Unlock()
	e.notify()
	return err
}

func joinHref(col, name string) string {
	u, err := url.Parse(col)
	if err != nil {
		return strings.TrimSuffix(col, "/") + "/" + name
	}
	u.Path = path.Join(u.Path, name)
	return u.String()
}

// SetShown — показывать ли календарь в своём виде и виджете.
func (e *Engine) SetShown(href string, shown bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cs := e.st.col(href)
	if cs == nil {
		return errors.New("нет такого календаря")
	}
	cs.Hidden = !shown
	e.st.Rev++
	return e.st.save(e.dir)
}

// SetWidget — включить/выключить виджет на рабочем столе ПК.
func (e *Engine) SetWidget(on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st.Widget = on
	return e.st.save(e.dir)
}

// WidgetWanted — открывать ли виджет при запуске (ПК).
func WidgetWanted(dir string) bool { return loadState(dir).Widget }

// ── чтение ───────────────────────────────────────────────────────────────

// Instance — событие в окне времени (повторы развёрнуты).
type Instance struct {
	Cal         string    `json:"cal"`
	Href        string    `json:"href"`
	UID         string    `json:"uid"`
	Title       string    `json:"title"`
	Location    string    `json:"location,omitempty"`
	Description string    `json:"description,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"allDay"`
	Recurring   bool      `json:"recurring,omitempty"`
	Color       string    `json:"color,omitempty"`
	ReadOnly    bool      `json:"readOnly,omitempty"`
}

// Events — события видимых календарей, пересекающие [from, to).
func (e *Engine) Events(from, to time.Time, all bool) []Instance {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Instance
	if e.st == nil {
		return out
	}
	for _, cs := range e.st.Collections {
		if cs.Contacts || (cs.Hidden && !all) {
			continue
		}
		for href, it := range cs.Items {
			for _, oc := range Expand(ParseEvents(it.Data), from, to) {
				out = append(out, Instance{
					Cal: cs.Href, Href: href, UID: oc.UID, Title: oc.Title,
					Location: oc.Location, Description: oc.Description,
					Start: oc.Start, End: oc.End, AllDay: oc.AllDay,
					Recurring: oc.Recurring, Color: cs.Color, ReadOnly: cs.ReadOnly,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// Status — что показать на экране.
type Status struct {
	Account     Account      `json:"account"`
	HasPassword bool         `json:"hasPassword"`
	Calendars   []CalSummary `json:"calendars"`
	Busy        bool         `json:"busy"`
	LastSync    time.Time    `json:"lastSync,omitempty"`
	LastError   string       `json:"lastError,omitempty"`
	Conflict    string       `json:"conflict,omitempty"`
	Widget      bool         `json:"widget"`
	Rev         int64        `json:"rev"`
}

// CalSummary — календарь в списке.
type CalSummary struct {
	Collection
	Shown bool `json:"shown"`
	Count int  `json:"count"`
}

// Status — состояние для экрана (без пароля).
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Status{Calendars: []CalSummary{}}
	if e.st == nil {
		return s
	}
	s.Account = e.st.Account
	s.HasPassword = fileExists(secretPath(e.dir))
	for _, cs := range e.st.Collections {
		s.Calendars = append(s.Calendars, CalSummary{cs.Collection, !cs.Hidden, len(cs.Items)})
	}
	s.Busy, s.LastSync, s.LastError, s.Conflict = e.busy, e.st.LastSync, e.st.LastError, e.st.Conflict
	s.Widget, s.Rev = e.st.Widget, e.st.Rev
	return s
}

// Rev — номер версии данных.
func (e *Engine) Rev() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st == nil {
		return 0
	}
	return e.st.Rev
}

// ExportEvent — главное событие для системного календаря Android:
// повторы — правилом, система разворачивает их сама.
type ExportEvent struct {
	Cal  string `json:"cal"`
	Href string `json:"href"`
	ETag string `json:"etag"`
	Event
}

// ExportCal — календарь с событиями для системного календаря телефона.
type ExportCal struct {
	Href     string        `json:"href"`
	Name     string        `json:"name"`
	Color    string        `json:"color,omitempty"`
	ReadOnly bool          `json:"readOnly,omitempty"`
	Events   []ExportEvent `json:"events"`
}

// Export — все календари с событиями (изменённые повторы — отдельными
// событиями с RecurrenceID).
func (e *Engine) Export() []ExportCal {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []ExportCal{}
	if e.st == nil {
		return out
	}
	for _, cs := range e.st.Collections {
		if cs.Contacts {
			continue
		}
		ec := ExportCal{Href: cs.Href, Name: cs.Name, Color: cs.Color, ReadOnly: cs.ReadOnly, Events: []ExportEvent{}}
		for href, it := range cs.Items {
			for _, ev := range ParseEvents(it.Data) {
				ec.Events = append(ec.Events, ExportEvent{Cal: cs.Href, Href: href, ETag: it.ETag, Event: ev})
			}
		}
		out = append(out, ec)
	}
	return out
}

// Contacts — контакты всех адресных книг (если включены).
func (e *Engine) Contacts() []Contact {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []Contact{}
	if e.st == nil || !e.st.Account.Contacts {
		return out
	}
	for _, cs := range e.st.Collections {
		if !cs.Contacts {
			continue
		}
		for _, it := range cs.Items {
			if c := ParseContact(it.Data); c.Name != "" || len(c.Phones) > 0 {
				out = append(out, c)
			}
		}
	}
	return out
}

// DialDirect — обычное соединение (нет ядра: туннель выключен).
func DialDirect(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}
