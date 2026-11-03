package calendar

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDAV — маленький Nextcloud: один календарь, sync-token, ETag, 412.
type fakeDAV struct {
	mu    sync.Mutex
	objs  map[string]string // href → ics
	etags map[string]int
	gen   int
	// removed — href, удалённые после токена gen.
	removed map[string]int
}

const calHref = "/remote.php/dav/calendars/vitaz/personal/"

func newFake() *fakeDAV {
	return &fakeDAV{objs: map[string]string{}, etags: map[string]int{}, removed: map[string]int{}}
}

func (f *fakeDAV) put(href, ics string) {
	f.gen++
	f.objs[href] = ics
	f.etags[href] = f.gen
	delete(f.removed, href)
}

func (f *fakeDAV) etag(href string) string { return fmt.Sprintf(`"e%d"`, f.etags[href]) }

func (f *fakeDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, _ := r.BasicAuth(); u != "vitaz" || p != "app-pass" {
		w.WriteHeader(401)
		return
	}
	body, _ := io.ReadAll(r.Body)
	ms := func(inner string) {
		w.WriteHeader(207)
		fmt.Fprintf(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:ic="http://apple.com/ns/ical/">%s</d:multistatus>`, inner)
	}
	resp := func(href, props string) string {
		return fmt.Sprintf(`<d:response><d:href>%s</d:href><d:propstat><d:prop>%s</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, href, props)
	}
	b := string(body)
	switch {
	case r.Method == "PROPFIND" && r.URL.Path == "/remote.php/dav/":
		ms(resp(r.URL.Path, `<d:current-user-principal><d:href>/remote.php/dav/principals/users/vitaz/</d:href></d:current-user-principal>`))
	case r.Method == "PROPFIND" && strings.HasPrefix(r.URL.Path, "/remote.php/dav/principals/"):
		ms(resp(r.URL.Path, `<c:calendar-home-set><d:href>/remote.php/dav/calendars/vitaz/</d:href></c:calendar-home-set>`))
	case r.Method == "PROPFIND" && r.URL.Path == "/remote.php/dav/calendars/vitaz/":
		ms(resp(r.URL.Path, `<d:resourcetype><d:collection/></d:resourcetype>`) +
			resp(calHref, `<d:resourcetype><d:collection/><c:calendar/></d:resourcetype><d:displayname>Личный</d:displayname><ic:calendar-color>#0082c9ff</ic:calendar-color><c:supported-calendar-component-set><c:comp name="VEVENT"/></c:supported-calendar-component-set><d:current-user-privilege-set><d:privilege><d:write/></d:privilege></d:current-user-privilege-set>`) +
			resp("/remote.php/dav/calendars/vitaz/tasks/", `<d:resourcetype><d:collection/><c:calendar/></d:resourcetype><c:supported-calendar-component-set><c:comp name="VTODO"/></c:supported-calendar-component-set>`))
	case r.Method == "PROPFIND" && r.URL.Path == calHref:
		out := resp(calHref, fmt.Sprintf(`<d:sync-token>t%d</d:sync-token>`, f.gen))
		for h := range f.objs {
			out += resp(h, `<d:getetag>`+f.etag(h)+`</d:getetag>`)
		}
		ms(out)
	case r.Method == "REPORT" && strings.Contains(b, "sync-collection"):
		var since int
		if _, err := fmt.Sscanf(between(b, "<d:sync-token>", "</d:sync-token>"), "t%d", &since); err != nil {
			w.WriteHeader(403)
			fmt.Fprint(w, `<d:error xmlns:d="DAV:"><d:valid-sync-token/></d:error>`)
			return
		}
		out := ""
		for h, g := range f.etags {
			if _, ok := f.objs[h]; ok && g > since {
				out += resp(h, `<d:getetag>`+f.etag(h)+`</d:getetag>`)
			}
		}
		for h, g := range f.removed {
			if g > since {
				out += `<d:response><d:href>` + h + `</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response>`
			}
		}
		ms(out + fmt.Sprintf(`<d:sync-token>t%d</d:sync-token>`, f.gen))
	case r.Method == "REPORT" && strings.Contains(b, "calendar-multiget"):
		out := ""
		for _, part := range strings.Split(b, "<d:href>")[1:] {
			h := part[:strings.Index(part, "</d:href>")]
			if ics, ok := f.objs[h]; ok {
				out += resp(h, `<d:getetag>`+f.etag(h)+`</d:getetag><c:calendar-data>`+xmlEscape(ics)+`</c:calendar-data>`)
			}
		}
		ms(out)
	case r.Method == "PUT":
		_, exists := f.objs[r.URL.Path]
		if m := r.Header.Get("If-Match"); m != "" && (!exists || m != f.etag(r.URL.Path)) {
			w.WriteHeader(412)
			return
		}
		if r.Header.Get("If-None-Match") == "*" && exists {
			w.WriteHeader(412)
			return
		}
		f.put(r.URL.Path, b)
		w.Header().Set("ETag", f.etag(r.URL.Path))
		w.WriteHeader(201)
	case r.Method == "DELETE":
		if m := r.Header.Get("If-Match"); m != "" && m != f.etag(r.URL.Path) {
			w.WriteHeader(412)
			return
		}
		f.gen++
		delete(f.objs, r.URL.Path)
		f.removed[r.URL.Path] = f.gen
		w.WriteHeader(204)
	default:
		w.WriteHeader(405)
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return s
}

const meetingICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:m1\r\nDTSTART;TZID=Europe/Moscow:20261102T100000\r\nDTEND;TZID=Europe/Moscow:20261102T110000\r\nSUMMARY:Планёрка\r\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE\r\nEXDATE;TZID=Europe/Moscow:20261104T100000\r\nBEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT15M\r\nDESCRIPTION:скоро\r\nEND:VALARM\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:m1\r\nRECURRENCE-ID;TZID=Europe/Moscow:20261109T100000\r\nDTSTART;TZID=Europe/Moscow:20261109T120000\r\nDTEND;TZID=Europe/Moscow:20261109T130000\r\nSUMMARY:Планёрка (перенос)\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func newEngine(t *testing.T) (*Engine, *fakeDAV, *httptest.Server) {
	t.Helper()
	f := newFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	e := &Engine{dir: t.TempDir()}
	e.st = loadState(e.dir)
	if err := e.SetAccount(Account{URL: srv.URL, User: "vitaz"}, "app-pass"); err != nil {
		t.Fatal(err)
	}
	return e, f, srv
}

func TestExpandПовторыИсключенияПереносы(t *testing.T) {
	msk, _ := time.LoadLocation("Europe/Moscow")
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, msk)
	occ := Expand(ParseEvents(meetingICS), from, from.AddDate(0, 0, 14))
	var got []string
	for _, o := range occ {
		got = append(got, o.Start.In(msk).Format("01-02 15:04")+" "+o.Title)
	}
	// 04.11 исключён, 09.11 перенесён на 12:00.
	want := "11-02 10:00 Планёрка|11-09 12:00 Планёрка (перенос)|11-11 10:00 Планёрка"
	if strings.Join(sortStrings(got), "|") != want {
		t.Fatalf("вхождения: %v", got)
	}
}

func sortStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

func TestRecurrencesМесяцГод(t *testing.T) {
	start := time.Date(2026, 1, 30, 9, 0, 0, 0, time.UTC)
	got := recurrences(start, "FREQ=MONTHLY;BYDAY=-1FR;COUNT=3", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if len(got) != 3 || got[0].Day() != 30 || got[1].Day() != 27 || got[2].Day() != 27 {
		t.Fatalf("последняя пятница: %v", got)
	}
	got = recurrences(time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC), "FREQ=MONTHLY", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if len(got) != 3 { // январь, март, май — в остальных нет 31-го
		t.Fatalf("31-е числа: %v", got)
	}
	got = recurrences(time.Date(2020, 2, 29, 0, 0, 0, 0, time.UTC), "FREQ=YEARLY;UNTIL=20290101", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if len(got) != 3 { // 2020, 2024, 2028
		t.Fatalf("29 февраля: %v", got)
	}
}

func TestUpdateEventСохраняетЧужое(t *testing.T) {
	ed := Edit{Title: "Новое; название", Start: time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)}
	out := UpdateEvent(meetingICS, ed, time.Now())
	if !strings.Contains(out, "BEGIN:VALARM") || !strings.Contains(out, "RRULE:FREQ=WEEKLY") {
		t.Fatal("потеряны напоминание или повтор")
	}
	evs := ParseEvents(out)
	if len(evs) != 2 || evs[0].Title != "Новое; название" || !evs[0].Start.Equal(ed.Start) {
		t.Fatalf("главное событие: %+v", evs[0])
	}
	if evs[1].Title != "Планёрка (перенос)" {
		t.Fatal("перенос не должен меняться")
	}
	if strings.Count(out, "SUMMARY:Новое") != 1 || !strings.Contains(out, "SEQUENCE:1") {
		t.Fatal("строки события должны замениться, а не добавиться")
	}
}

func TestСинхронизацияИПравки(t *testing.T) {
	e, f, _ := newEngine(t)
	f.mu.Lock()
	f.put(calHref+"m1.ics", meetingICS)
	f.mu.Unlock()
	if err := e.SyncNow(); err != nil {
		t.Fatal(err)
	}
	st := e.Status()
	if len(st.Calendars) != 1 || st.Calendars[0].Name != "Личный" || st.Calendars[0].Color != "#0082c9" || st.Calendars[0].Count != 1 {
		t.Fatalf("календари (задачи не нужны): %+v", st.Calendars)
	}
	msk, _ := time.LoadLocation("Europe/Moscow")
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, msk)
	if n := len(e.Events(from, from.AddDate(0, 0, 7), false)); n != 1 {
		t.Fatalf("событий на неделе: %d", n)
	}

	// Создали на телефоне → появилось на сервере.
	href, err := e.SaveEvent(calHref, "", Edit{Title: "Врач", Start: time.Date(2026, 11, 5, 9, 0, 0, 0, msk), End: time.Date(2026, 11, 5, 10, 0, 0, 0, msk)})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	ics := f.objs[href]
	f.mu.Unlock()
	if !strings.Contains(ics, "SUMMARY:Врач") || !strings.Contains(ics, "DTSTART:20261105T060000Z") {
		t.Fatalf("на сервере: %q", ics)
	}

	// Изменили в Nextcloud → после сверки видно здесь (по sync-token).
	f.mu.Lock()
	f.put(href, strings.Replace(ics, "Врач", "Стоматолог", 1))
	f.mu.Unlock()
	if err := e.SyncNow(); err != nil {
		t.Fatal(err)
	}
	if ev := e.Events(from, from.AddDate(0, 0, 7), false); len(ev) != 2 || !hasTitle(ev, "Стоматолог") {
		t.Fatalf("после правки на сервере: %+v", ev)
	}

	// Конфликт: сервер изменил, а мы правим старую версию → 412, видна
	// версия сервера.
	f.mu.Lock()
	f.put(href, strings.Replace(f.objs[href], "Стоматолог", "Хирург", 1))
	f.mu.Unlock()
	_, err = e.SaveEvent(calHref, href, Edit{Title: "Моё", Start: time.Date(2026, 11, 5, 9, 0, 0, 0, msk)})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("ждали конфликт, получили %v", err)
	}
	if ev := e.Events(from, from.AddDate(0, 0, 7), false); !hasTitle(ev, "Хирург") || e.Status().Conflict == "" {
		t.Fatal("после конфликта должна быть версия сервера и пометка")
	}

	// Удалили здесь → нет на сервере; удалили там → нет здесь.
	if err := e.DeleteEvent(href); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	_, still := f.objs[href]
	delete(f.objs, calHref+"m1.ics")
	f.gen++
	f.removed[calHref+"m1.ics"] = f.gen
	f.mu.Unlock()
	if still {
		t.Fatal("событие осталось на сервере")
	}
	if err := e.SyncNow(); err != nil {
		t.Fatal(err)
	}
	if n := e.Status().Calendars[0].Count; n != 0 {
		t.Fatalf("после удаления осталось %d", n)
	}

	// Перезапуск: всё читается с диска, пароль расшифровывается.
	e2 := &Engine{dir: e.dir}
	e2.st = loadState(e.dir)
	if !e2.Configured() || loadPassword(e.dir) != "app-pass" {
		t.Fatal("аккаунт не пережил перезапуск")
	}
	if err := e2.SyncNow(); err != nil {
		t.Fatal(err)
	}
}

func TestНеверныйПароль(t *testing.T) {
	srv := httptest.NewServer(newFake())
	defer srv.Close()
	e := &Engine{dir: t.TempDir()}
	e.st = loadState(e.dir)
	err := e.SetAccount(Account{URL: srv.URL, User: "vitaz"}, "wrong")
	if err == nil || !strings.Contains(err.Error(), "пароль") {
		t.Fatalf("ждали ошибку пароля, получили %v", err)
	}
	if e.Configured() {
		t.Fatal("неверный аккаунт не должен сохраняться")
	}
}

func hasTitle(ev []Instance, title string) bool {
	for _, e := range ev {
		if e.Title == title {
			return true
		}
	}
	return false
}

func TestParseContact(t *testing.T) {
	c := ParseContact("BEGIN:VCARD\r\nVERSION:3.0\r\nN:Иванов;Пётр;;;\r\nitem1.TEL;TYPE=CELL:+7 900 000-00-00\r\nEMAIL:p@example.com\r\nEND:VCARD\r\n")
	if c.Name != "Пётр Иванов" || len(c.Phones) != 1 || c.Emails[0] != "p@example.com" {
		t.Fatalf("%+v", c)
	}
}
