package calendar

// Разбор и запись iCalendar (RFC 5545) — ровно столько, сколько нужно
// календарю: события (VEVENT), их время, повторы и исключения. Остальное
// (напоминания, участники, свои поля Nextcloud) не разбирается, а хранится
// как есть: правка события меняет только свои строки, и чужое не теряется.

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
	_ "time/tzdata" // часовые пояса TZID на Windows и Android без системной базы
)

// prop — одна строка iCalendar: ИМЯ;ПАРАМ=…:значение.
type prop struct {
	Name   string
	Params map[string]string
	Value  string
}

// unfold склеивает перенесённые строки (продолжение начинается с пробела
// или табуляции) и делит текст на строки.
func unfold(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\n ", "")
	text = strings.ReplaceAll(text, "\n\t", "")
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, "\r"))
		}
	}
	return out
}

// parseLine разбирает строку; двоеточие внутри кавычек параметра — не
// разделитель.
func parseLine(l string) prop {
	inQ := false
	cut := -1
	for i, c := range l {
		if c == '"' {
			inQ = !inQ
		} else if c == ':' && !inQ {
			cut = i
			break
		}
	}
	if cut < 0 {
		return prop{Name: strings.ToUpper(l)}
	}
	head, val := l[:cut], l[cut+1:]
	parts := strings.Split(head, ";")
	p := prop{Name: strings.ToUpper(parts[0]), Value: val}
	for _, kv := range parts[1:] {
		k, v, _ := strings.Cut(kv, "=")
		if p.Params == nil {
			p.Params = map[string]string{}
		}
		p.Params[strings.ToUpper(k)] = strings.Trim(v, `"`)
	}
	return p
}

// unescape — текстовое значение без экранирования (\n, \, \; \\).
func unescape(s string) string {
	r := strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`)
	return r.Replace(s)
}

func escape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`)
	return r.Replace(s)
}

// fold переносит длинную строку (не длиннее 75 байт, не разрывая символ).
func fold(l string) string {
	if len(l) <= 75 {
		return l
	}
	var b strings.Builder
	n := 0
	for _, r := range l {
		size := len(string(r))
		if n+size > 75 {
			b.WriteString("\r\n ")
			n = 1
		}
		b.WriteRune(r)
		n += size
	}
	return b.String()
}

// Event — событие в удобном виде. Время — с часовым поясом; у события на
// весь день — полночь даты по UTC, End — следующий день после последнего.
type Event struct {
	UID         string    `json:"uid"`
	Title       string    `json:"title"`
	Location    string    `json:"location,omitempty"`
	Description string    `json:"description,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"allDay"`
	// TZ — часовой пояс начала (TZID), если был; для Android-календаря.
	TZ string `json:"tz,omitempty"`
	// RRule — правило повтора как есть (FREQ=WEEKLY;BYDAY=MO…).
	RRule   string      `json:"rrule,omitempty"`
	ExDates []time.Time `json:"exdates,omitempty"`
	// RecurrenceID — у изменённого повтора: какое по счёту вхождение он
	// заменяет (исходное время начала).
	RecurrenceID time.Time `json:"recurrenceId,omitempty"`
	Cancelled    bool      `json:"cancelled,omitempty"`
}

// parseTime — DATE или DATE-TIME с TZID/Z/«плавающим» временем.
func parseTime(p prop) (time.Time, bool, string) {
	v := p.Value
	if p.Params["VALUE"] == "DATE" || len(v) == 8 {
		t, err := time.Parse("20060102", v)
		if err != nil {
			return time.Time{}, true, ""
		}
		return t, true, ""
	}
	if strings.HasSuffix(v, "Z") {
		t, _ := time.Parse("20060102T150405Z", v)
		return t, false, "UTC"
	}
	tzid := p.Params["TZID"]
	loc := time.Local
	if tzid != "" {
		if l, err := time.LoadLocation(tzid); err == nil {
			loc = l
		} else if l, ok := windowsZones[tzid]; ok {
			if l2, err := time.LoadLocation(l); err == nil {
				loc, tzid = l2, l
			}
		}
	}
	t, _ := time.ParseInLocation("20060102T150405", v, loc)
	return t, false, tzid
}

// windowsZones — имена поясов Windows/Outlook, которые встречаются в
// приглашениях, → имена IANA. Только частые; остальное — местное время.
var windowsZones = map[string]string{
	"Russian Standard Time":        "Europe/Moscow",
	"W. Europe Standard Time":      "Europe/Berlin",
	"Central Europe Standard Time": "Europe/Budapest",
	"GMT Standard Time":            "Europe/London",
	"UTC":                          "UTC",
	"Ekaterinburg Standard Time":   "Asia/Yekaterinburg",
}

// components делит календарь на компоненты VEVENT (строки каждой).
func components(ics, kind string) [][]string {
	var out [][]string
	var cur []string
	depth := 0
	for _, l := range unfold(ics) {
		up := strings.ToUpper(l)
		if up == "BEGIN:"+kind && depth == 0 {
			cur = []string{}
			depth = 1
			continue
		}
		if depth > 0 {
			if strings.HasPrefix(up, "BEGIN:") {
				depth++
			}
			if strings.HasPrefix(up, "END:") {
				depth--
				if depth == 0 {
					out = append(out, cur)
					cur = nil
					continue
				}
			}
			if depth == 1 {
				cur = append(cur, l)
			}
		}
	}
	return out
}

// ParseEvents — все VEVENT файла: главное событие и изменённые повторы.
func ParseEvents(ics string) []Event {
	var out []Event
	for _, lines := range components(ics, "VEVENT") {
		var e Event
		hasEnd := false
		var dur time.Duration
		for _, l := range lines {
			p := parseLine(l)
			switch p.Name {
			case "UID":
				e.UID = p.Value
			case "SUMMARY":
				e.Title = unescape(p.Value)
			case "LOCATION":
				e.Location = unescape(p.Value)
			case "DESCRIPTION":
				e.Description = unescape(p.Value)
			case "DTSTART":
				e.Start, e.AllDay, e.TZ = parseTime(p)
			case "DTEND":
				e.End, _, _ = parseTime(p)
				hasEnd = true
			case "DURATION":
				dur = parseDuration(p.Value)
			case "RRULE":
				e.RRule = p.Value
			case "EXDATE":
				for _, v := range strings.Split(p.Value, ",") {
					t, _, _ := parseTime(prop{Params: p.Params, Value: v})
					e.ExDates = append(e.ExDates, t)
				}
			case "RECURRENCE-ID":
				e.RecurrenceID, _, _ = parseTime(p)
			case "STATUS":
				e.Cancelled = strings.EqualFold(p.Value, "CANCELLED")
			}
		}
		if !hasEnd {
			switch {
			case dur > 0:
				e.End = e.Start.Add(dur)
			case e.AllDay:
				e.End = e.Start.AddDate(0, 0, 1)
			default:
				e.End = e.Start
			}
		}
		out = append(out, e)
	}
	return out
}

// parseDuration — DURATION вида P1D, PT1H30M, P2W.
func parseDuration(s string) time.Duration {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimLeft(s, "+-")
	s = strings.TrimPrefix(s, "P")
	var d time.Duration
	num := 0
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			num = num*10 + int(c-'0')
		case c == 'W':
			d += time.Duration(num) * 7 * 24 * time.Hour
			num = 0
		case c == 'D':
			d += time.Duration(num) * 24 * time.Hour
			num = 0
		case c == 'H':
			d += time.Duration(num) * time.Hour
			num = 0
		case c == 'M':
			d += time.Duration(num) * time.Minute
			num = 0
		case c == 'S':
			d += time.Duration(num) * time.Second
			num = 0
		}
	}
	if neg {
		d = -d
	}
	return d
}

// NewUID — уникальный UID для нового события.
func NewUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b) + "@ssh_tunnel"
}

// Edit — что меняет человек в событии.
type Edit struct {
	Title       string    `json:"title"`
	Location    string    `json:"location"`
	Description string    `json:"description"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"allDay"`
}

func timeLines(name string, t time.Time, allDay bool) string {
	if allDay {
		return name + ";VALUE=DATE:" + t.Format("20060102")
	}
	return name + ":" + t.UTC().Format("20060102T150405Z")
}

// owned — строки главного события, которые пишет программа; при правке они
// заменяются, остальные остаются как были.
var owned = map[string]bool{
	"SUMMARY": true, "LOCATION": true, "DESCRIPTION": true, "DTSTART": true,
	"DTEND": true, "DURATION": true, "DTSTAMP": true, "LAST-MODIFIED": true, "SEQUENCE": true,
}

// BuildEvent — новый файл .ics с одним событием.
func BuildEvent(uid string, ed Edit, now time.Time) string {
	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//ssh_tunnel//calendar//RU",
		"BEGIN:VEVENT",
		"UID:" + uid,
	}
	lines = append(lines, eventLines(ed, now, 0)...)
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")
	return joinLines(lines)
}

func eventLines(ed Edit, now time.Time, seq int) []string {
	end := ed.End
	if ed.AllDay && !end.After(ed.Start) {
		end = ed.Start.AddDate(0, 0, 1)
	}
	if !ed.AllDay && end.Before(ed.Start) {
		end = ed.Start
	}
	out := []string{
		"DTSTAMP:" + now.UTC().Format("20060102T150405Z"),
		"LAST-MODIFIED:" + now.UTC().Format("20060102T150405Z"),
		timeLines("DTSTART", ed.Start, ed.AllDay),
		timeLines("DTEND", end, ed.AllDay),
		"SUMMARY:" + escape(ed.Title),
	}
	if seq > 0 {
		out = append(out, "SEQUENCE:"+itoa(seq))
	}
	if ed.Location != "" {
		out = append(out, "LOCATION:"+escape(ed.Location))
	}
	if ed.Description != "" {
		out = append(out, "DESCRIPTION:"+escape(ed.Description))
	}
	return out
}

// UpdateEvent меняет главное событие (без RECURRENCE-ID) в существующем
// файле: свои строки заменяет, чужие — напоминания, повторы, участники —
// оставляет. Повторяющееся событие меняется целиком (вся серия).
func UpdateEvent(ics string, ed Edit, now time.Time) string {
	lines := unfold(ics)
	var out []string
	depth := 0
	inMain := false
	var block []string
	seq := 0
	for _, l := range lines {
		up := strings.ToUpper(l)
		if up == "BEGIN:VEVENT" && depth == 0 {
			depth = 1
			block = []string{l}
			continue
		}
		if depth > 0 {
			block = append(block, l)
			if strings.HasPrefix(up, "BEGIN:") {
				depth++
			} else if strings.HasPrefix(up, "END:") {
				depth--
			}
			if depth == 0 {
				out = append(out, rewriteBlock(block, ed, now, &seq, &inMain)...)
			}
			continue
		}
		out = append(out, l)
	}
	return joinLines(out)
}

func rewriteBlock(block []string, ed Edit, now time.Time, seq *int, done *bool) []string {
	isMain := true
	for _, l := range block {
		if strings.HasPrefix(strings.ToUpper(l), "RECURRENCE-ID") {
			isMain = false
		}
	}
	if !isMain || *done {
		return block
	}
	*done = true
	var out []string
	depth := 0
	for i, l := range block {
		up := strings.ToUpper(l)
		if i == 0 {
			out = append(out, l)
			depth = 1
			continue
		}
		if strings.HasPrefix(up, "BEGIN:") {
			depth++
		}
		if depth == 1 && up == "END:VEVENT" {
			out = append(out, eventLines(ed, now, *seq+1)...)
			out = append(out, l)
			continue
		}
		if depth == 1 {
			p := parseLine(l)
			if p.Name == "SEQUENCE" {
				n := 0
				for _, c := range p.Value {
					if c >= '0' && c <= '9' {
						n = n*10 + int(c-'0')
					}
				}
				*seq = n
			}
			if owned[p.Name] {
				continue
			}
		}
		if strings.HasPrefix(up, "END:") {
			depth--
		}
		out = append(out, l)
	}
	return out
}

func joinLines(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(fold(l))
		b.WriteString("\r\n")
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Contact — карточка контакта (vCard), только то, что пишется в контакты
// телефона.
type Contact struct {
	UID    string   `json:"uid"`
	Name   string   `json:"name"`
	Phones []string `json:"phones,omitempty"`
	Emails []string `json:"emails,omitempty"`
	Org    string   `json:"org,omitempty"`
}

// ParseContact — vCard 3/4.
func ParseContact(vcf string) Contact {
	var c Contact
	var n string
	for _, l := range unfold(vcf) {
		p := parseLine(l)
		// Группы Apple: «item1.TEL» — имя после точки.
		if i := strings.LastIndex(p.Name, "."); i >= 0 {
			p.Name = p.Name[i+1:]
		}
		switch p.Name {
		case "UID":
			c.UID = p.Value
		case "FN":
			c.Name = unescape(p.Value)
		case "N":
			parts := strings.Split(p.Value, ";")
			if len(parts) >= 2 {
				n = strings.TrimSpace(unescape(parts[1]) + " " + unescape(parts[0]))
			}
		case "TEL":
			c.Phones = append(c.Phones, strings.TrimPrefix(p.Value, "tel:"))
		case "EMAIL":
			c.Emails = append(c.Emails, p.Value)
		case "ORG":
			c.Org = unescape(strings.Split(p.Value, ";")[0])
		}
	}
	if c.Name == "" {
		c.Name = n
	}
	return c
}
