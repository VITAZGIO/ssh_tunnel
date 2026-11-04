package traffic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// keepDays — сколько дней хранить: 3 месяца. Суточные итоги по программам —
// меньше мегабайта, старое удаляется само.
const keepDays = 92

// key — строка суточного итога: программа, путь, сеть телефона.
type key struct{ proc, route, net string }

// Rec — суточный итог в файле.
type Rec struct {
	Proc  string `json:"p"`
	Route string `json:"r"`
	Net   string `json:"n,omitempty"`
	Up    int64  `json:"u"`
	Down  int64  `json:"d"`
}

// Limit — предупреждение о расходе. VPN не выключается никогда.
type Limit struct {
	On bool `json:"on"`
	// GB — сколько можно за период, гигабайты.
	GB float64 `json:"gb"`
	// Period — "month" (с 1-го числа) или "day".
	Period string `json:"period"`
	// Scope — что считать: "all", "server" (только через VPN) или "mobile"
	// (только мобильная сеть телефона).
	Scope string `json:"scope"`
}

type fileData struct {
	Days  map[string][]Rec `json:"days"`
	Limit Limit            `json:"limit"`
	// Warned — до скольких процентов уже предупредили в периоде (ключ —
	// период: «2026-11» или «2026-11-03»).
	Warned map[string]int `json:"warned,omitempty"`
	// Hours — по часам за двое суток (график «День»): «2026-11-04T13|wifi» →
	// байты через сервер, напрямую, в сеть устройств.
	Hours map[string][3]int64 `json:"hours,omitempty"`
}

// store — итоги по дням в памяти и в traffic.json.
type store struct {
	path string

	mu     sync.Mutex
	days   map[string]map[key]*[2]int64
	limit  Limit
	warned map[string]int
	hours  map[string]*[3]int64
	dirty  bool
	now    func() time.Time
}

func openStore(dir string) *store {
	s := &store{path: filepath.Join(dir, "traffic.json"), days: map[string]map[key]*[2]int64{},
		warned: map[string]int{}, hours: map[string]*[3]int64{}, now: time.Now,
		limit: Limit{GB: 50, Period: "month", Scope: "all"}}
	var d fileData
	if b, err := os.ReadFile(s.path); err == nil && json.Unmarshal(b, &d) == nil {
		for day, recs := range d.Days {
			m := map[key]*[2]int64{}
			for _, r := range recs {
				k := key{r.Proc, r.Route, r.Net}
				if v, ok := m[k]; ok {
					v[0] += r.Up
					v[1] += r.Down
				} else {
					m[k] = &[2]int64{r.Up, r.Down}
				}
			}
			s.days[day] = m
		}
		if d.Limit.Period != "" {
			s.limit = d.Limit
		}
		if d.Warned != nil {
			s.warned = d.Warned
		}
		for h, v := range d.Hours {
			v := v
			s.hours[h] = &v
		}
	}
	s.prune()
	return s
}

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

func hourKey(t time.Time, net string) string { return t.Format("2006-01-02T15") + "|" + net }

// routeIndex — место пути в тройке часа: сервер, напрямую, сеть устройств.
func routeIndex(route string) int {
	switch route {
	case RouteDirect:
		return 1
	case RouteMesh:
		return 2
	}
	return 0
}

func (s *store) add(proc, route string, up, down int64) {
	k := key{proc, route, currentNet()}
	s.mu.Lock()
	now := s.now()
	day := dayKey(now)
	hk := hourKey(now, k.net)
	h := s.hours[hk]
	if h == nil {
		h = &[3]int64{}
		s.hours[hk] = h
	}
	h[routeIndex(route)] += up + down
	m := s.days[day]
	if m == nil {
		m = map[key]*[2]int64{}
		s.days[day] = m
	}
	v := m[k]
	if v == nil {
		v = &[2]int64{}
		m[k] = v
	}
	v[0] += up
	v[1] += down
	s.dirty = true
	s.mu.Unlock()
}

// prune удаляет дни старше keepDays и старые отметки предупреждений.
func (s *store) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	edge := dayKey(s.now().AddDate(0, 0, -keepDays))
	for d := range s.days {
		if d < edge {
			delete(s.days, d)
			s.dirty = true
		}
	}
	hedge := dayKey(s.now().AddDate(0, 0, -2))
	for h := range s.hours {
		if h[:10] < hedge {
			delete(s.hours, h)
			s.dirty = true
		}
	}
	for p := range s.warned {
		if p < edge[:7] {
			delete(s.warned, p)
		}
	}
}

// save пишет файл, если есть что писать.
func (s *store) save() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	d := fileData{Days: map[string][]Rec{}, Limit: s.limit, Warned: s.warned, Hours: map[string][3]int64{}}
	for h, v := range s.hours {
		d.Hours[h] = *v
	}
	for day, m := range s.days {
		recs := make([]Rec, 0, len(m))
		for k, v := range m {
			recs = append(recs, Rec{k.proc, k.route, k.net, v[0], v[1]})
		}
		sort.Slice(recs, func(i, j int) bool { return recs[i].Up+recs[i].Down > recs[j].Up+recs[j].Down })
		d.Days[day] = recs
	}
	b, err := json.Marshal(d)
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// hour — байты часа t по путям; net — "all", "wifi" или "mobile".
func (s *store) hour(t time.Time, net string) Day {
	s.mu.Lock()
	defer s.mu.Unlock()
	pfx := t.Format("2006-01-02T15") + "|"
	var d Day
	for k, v := range s.hours {
		if len(k) < len(pfx) || k[:len(pfx)] != pfx || (net != "all" && k[len(pfx):] != net) {
			continue
		}
		d.Server += v[0]
		d.Direct += v[1]
		d.Mesh += v[2]
	}
	return d
}

// recs — итоги дня (копия).
func (s *store) recs(day string) []Rec {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.days[day]
	out := make([]Rec, 0, len(m))
	for k, v := range m {
		out = append(out, Rec{k.proc, k.route, k.net, v[0], v[1]})
	}
	return out
}

func (s *store) getLimit() Limit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}

func (s *store) setLimit(l Limit) {
	s.mu.Lock()
	if l.Period != "day" {
		l.Period = "month"
	}
	switch l.Scope {
	case "server", "mobile":
	default:
		l.Scope = "all"
	}
	if l.GB < 0 {
		l.GB = 0
	}
	// Новый лимит — предупреждать заново.
	if l != s.limit {
		s.warned = map[string]int{}
	}
	s.limit = l
	s.dirty = true
	s.mu.Unlock()
}

// periodDays — дни периода лимита, в который попадает now, и ключ периода.
func periodDays(period string, now time.Time) ([]string, string) {
	if period == "day" {
		d := dayKey(now)
		return []string{d}, d
	}
	var out []string
	for t := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()); !t.After(now); t = t.AddDate(0, 0, 1) {
		out = append(out, dayKey(t))
	}
	return out, now.Format("2006-01")
}

// counts — подходит ли строка под «что считать» лимита.
func (l Limit) counts(r Rec) bool {
	switch l.Scope {
	case "server":
		return r.Route == RouteServer
	case "mobile":
		return r.Net == "mobile"
	}
	return true
}

// used — сколько израсходовано в текущем периоде лимита, байты.
func (s *store) used(l Limit) int64 {
	days, _ := periodDays(l.Period, s.now())
	var sum int64
	for _, d := range days {
		for _, r := range s.recs(d) {
			if l.counts(r) {
				sum += r.Up + r.Down
			}
		}
	}
	return sum
}

// checkLimit — пора ли предупредить: вернёт 80 или 100 один раз за период.
func (s *store) checkLimit() (int, Limit, int64) {
	l := s.getLimit()
	if !l.On || l.GB <= 0 {
		return 0, l, 0
	}
	used := s.used(l)
	pct := int(float64(used) * 100 / (l.GB * 1e9))
	_, pk := periodDays(l.Period, s.now())
	level := 0
	switch {
	case pct >= 100:
		level = 100
	case pct >= 80:
		level = 80
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if level == 0 || s.warned[pk] >= level {
		return 0, l, used
	}
	s.warned[pk] = level
	s.dirty = true
	return level, l, used
}
