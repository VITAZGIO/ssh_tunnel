package calendar

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Occurrence — одно вхождение события.
type Occurrence struct {
	Event
	Recurring bool
}

// Expand разворачивает события одного файла (главное + изменённые повторы) в
// вхождения, пересекающие [from, to). Повторы: FREQ DAILY/WEEKLY/MONTHLY/
// YEARLY, INTERVAL, COUNT, UNTIL, BYDAY (в том числе «2-й вторник», «-1FR»),
// BYMONTHDAY; EXDATE; изменённые и отменённые вхождения (RECURRENCE-ID).
// Этого хватает для того, что создают Nextcloud, телефоны и Outlook; на
// Android события и так разворачивает системный календарь.
func Expand(evs []Event, from, to time.Time) []Occurrence {
	var master *Event
	overrides := map[int64]Event{}
	for i := range evs {
		if evs[i].RecurrenceID.IsZero() {
			if master == nil {
				master = &evs[i]
			}
		} else {
			overrides[evs[i].RecurrenceID.Unix()] = evs[i]
		}
	}
	var out []Occurrence
	add := func(e Event, rec bool) {
		if e.Cancelled {
			return
		}
		end := e.End
		if !end.After(e.Start) {
			end = e.Start.Add(time.Second) // событие-точка видно в своём дне
		}
		if end.After(from) && e.Start.Before(to) {
			out = append(out, Occurrence{e, rec})
		}
	}
	if master == nil {
		for _, o := range overrides {
			add(o, true)
		}
		return out
	}
	if master.RRule == "" {
		add(*master, false)
		return out
	}
	dur := master.End.Sub(master.Start)
	ex := map[int64]bool{}
	for _, t := range master.ExDates {
		ex[t.Unix()] = true
		if master.AllDay {
			ex[time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix()] = true
		}
	}
	for _, st := range recurrences(master.Start, master.RRule, to) {
		if ex[st.Unix()] {
			continue
		}
		if o, ok := overrides[st.Unix()]; ok {
			add(o, true)
			delete(overrides, st.Unix())
			continue
		}
		e := *master
		e.Start, e.End = st, st.Add(dur)
		add(e, true)
	}
	// Перенесённые вхождения, исходное время которых вне правила.
	for _, o := range overrides {
		add(o, true)
	}
	return out
}

type rrule struct {
	freq     string
	interval int
	count    int
	until    time.Time
	byDay    []string
	byMDay   []int
	byMonth  []int
}

func parseRRule(s string, loc *time.Location) rrule {
	r := rrule{interval: 1}
	for _, part := range strings.Split(s, ";") {
		k, v, _ := strings.Cut(part, "=")
		switch strings.ToUpper(k) {
		case "FREQ":
			r.freq = strings.ToUpper(v)
		case "INTERVAL":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				r.interval = n
			}
		case "COUNT":
			r.count, _ = strconv.Atoi(v)
		case "UNTIL":
			t, _, _ := parseTime(prop{Value: v})
			if len(v) == 8 {
				t = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, loc)
			}
			r.until = t
		case "BYDAY":
			r.byDay = strings.Split(strings.ToUpper(v), ",")
		case "BYMONTHDAY":
			for _, x := range strings.Split(v, ",") {
				if n, err := strconv.Atoi(x); err == nil {
					r.byMDay = append(r.byMDay, n)
				}
			}
		case "BYMONTH":
			for _, x := range strings.Split(v, ",") {
				if n, err := strconv.Atoi(x); err == nil {
					r.byMonth = append(r.byMonth, n)
				}
			}
		}
	}
	return r
}

var weekdays = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

// byDayRule — «MO», «2TU», «-1FR».
func byDayRule(s string) (int, time.Weekday, bool) {
	if len(s) < 2 {
		return 0, 0, false
	}
	wd, ok := weekdays[s[len(s)-2:]]
	if !ok {
		return 0, 0, false
	}
	n := 0
	if len(s) > 2 {
		n, _ = strconv.Atoi(s[:len(s)-2])
	}
	return n, wd, true
}

// maxOccurrences — защита от бесконечных правил.
const maxOccurrences = 5000

// recurrences — начала вхождений по правилу до to (первое — само начало).
func recurrences(start time.Time, rule string, to time.Time) []time.Time {
	loc := start.Location()
	r := parseRRule(rule, loc)
	var out []time.Time
	emit := func(t time.Time) bool {
		if t.Before(start) {
			return true
		}
		if !r.until.IsZero() && t.After(r.until) {
			return false
		}
		if r.count > 0 && len(out) >= r.count {
			return false
		}
		if !t.Before(to) || len(out) >= maxOccurrences {
			return false
		}
		out = append(out, t)
		return true
	}
	h, m, s := start.Clock()
	at := func(y int, mo time.Month, d int) time.Time { return time.Date(y, mo, d, h, m, s, 0, loc) }
	monthOK := func(mo time.Month) bool {
		if len(r.byMonth) == 0 {
			return true
		}
		for _, x := range r.byMonth {
			if time.Month(x) == mo {
				return true
			}
		}
		return false
	}
	// daysOfMonth — дни месяца по BYDAY/BYMONTHDAY (по возрастанию).
	daysOfMonth := func(y int, mo time.Month) []int {
		last := time.Date(y, mo+1, 0, 0, 0, 0, 0, loc).Day()
		pick := map[int]bool{}
		for _, d := range r.byMDay {
			if d < 0 {
				d = last + d + 1
			}
			if d >= 1 && d <= last {
				pick[d] = true
			}
		}
		for _, bd := range r.byDay {
			n, wd, ok := byDayRule(bd)
			if !ok {
				continue
			}
			var ds []int
			for d := 1; d <= last; d++ {
				if time.Date(y, mo, d, 0, 0, 0, 0, loc).Weekday() == wd {
					ds = append(ds, d)
				}
			}
			switch {
			case n == 0:
				for _, d := range ds {
					pick[d] = true
				}
			case n > 0 && n <= len(ds):
				pick[ds[n-1]] = true
			case n < 0 && -n <= len(ds):
				pick[ds[len(ds)+n]] = true
			}
		}
		var out []int
		for d := 1; d <= last; d++ {
			if pick[d] {
				out = append(out, d)
			}
		}
		return out
	}

	switch r.freq {
	case "DAILY":
		for i := 0; ; i += r.interval {
			t := at(start.Year(), start.Month(), start.Day()+i)
			if len(r.byDay) > 0 {
				keep := false
				for _, bd := range r.byDay {
					if _, wd, ok := byDayRule(bd); ok && wd == t.Weekday() {
						keep = true
					}
				}
				if !keep {
					if !t.Before(to) || i > 366*50 {
						break
					}
					continue
				}
			}
			if !monthOK(t.Month()) {
				if !t.Before(to) {
					break
				}
				continue
			}
			if !emit(t) {
				break
			}
		}
	case "WEEKLY":
		days := []time.Weekday{}
		for _, bd := range r.byDay {
			if _, wd, ok := byDayRule(bd); ok {
				days = append(days, wd)
			}
		}
		if len(days) == 0 {
			days = []time.Weekday{start.Weekday()}
		}
		// Неделя с понедельника (WKST=MO по умолчанию у всех клиентов).
		off := (int(start.Weekday()) + 6) % 7
		weekStart := at(start.Year(), start.Month(), start.Day()-off)
	weeks:
		for w := 0; ; w += r.interval {
			for d := 0; d < 7; d++ {
				t := at(weekStart.Year(), weekStart.Month(), weekStart.Day()+w*7+d)
				hit := false
				for _, wd := range days {
					hit = hit || wd == t.Weekday()
				}
				if hit && !emit(t) && !t.Before(start) {
					break weeks
				}
			}
			if w > 52*100 {
				break
			}
		}
	case "MONTHLY":
		for i := 0; i < 12*100; i += r.interval {
			y, mo := start.Year(), start.Month()+time.Month(i)
			first := time.Date(y, mo, 1, 0, 0, 0, 0, loc)
			days := []int{start.Day()}
			if len(r.byDay) > 0 || len(r.byMDay) > 0 {
				days = daysOfMonth(first.Year(), first.Month())
			}
			stop := false
			for _, d := range days {
				if d > time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, loc).Day() {
					continue // 31-го нет в этом месяце — пропуск, как в RFC
				}
				if !emit(at(first.Year(), first.Month(), d)) && !at(first.Year(), first.Month(), d).Before(start) {
					stop = true
					break
				}
			}
			if stop {
				break
			}
		}
	case "YEARLY":
		for i := 0; i < 200; i += r.interval {
			y := start.Year() + i
			months := []time.Month{start.Month()}
			if len(r.byMonth) > 0 {
				months = nil
				for _, x := range r.byMonth {
					months = append(months, time.Month(x))
				}
			}
			stop := false
			for _, mo := range months {
				days := []int{start.Day()}
				if len(r.byDay) > 0 || len(r.byMDay) > 0 {
					days = daysOfMonth(y, mo)
				}
				for _, d := range days {
					if d > time.Date(y, mo+1, 0, 0, 0, 0, 0, loc).Day() {
						continue
					}
					t := at(y, mo, d)
					if !emit(t) && !t.Before(start) {
						stop = true
						break
					}
				}
				if stop {
					break
				}
			}
			if stop {
				break
			}
		}
	default:
		emit(start)
	}
	return out
}

func removeFile(p string) error {
	err := os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
