package core

// Статистика блокировки рекламы (плагин «Блокировка рекламы»): сколько и
// каких имён отвергнуто — по дням за 30 дней, самые частые и последние.
// Пишет DNS (dns.go) при каждом отказе; файл задаёт тот, кто знает папку
// данных (слой VPN на ПК, mobile на телефоне). Без файла — только в памяти.

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

const adKeepDays = 30

type adStats struct {
	mu     sync.Mutex
	path   string
	Days   map[string]map[string]int `json:"days"` // «2026-11-05» → имя → сколько
	recent []AdHit
	dirty  bool
	saveAt time.Time
}

// AdHit — одно заблокированное имя.
type AdHit struct {
	Name string `json:"name"`
	Time int64  `json:"time"` // Unix, с
}

var ads = &adStats{Days: map[string]map[string]int{}}

// SetAdStatsFile — где хранить статистику; читает то, что уже там есть.
func SetAdStatsFile(path string) {
	ads.mu.Lock()
	defer ads.mu.Unlock()
	if ads.path == path {
		return
	}
	ads.path = path
	var d struct {
		Days map[string]map[string]int `json:"days"`
	}
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &d) == nil && d.Days != nil {
		for day, m := range d.Days {
			cur := ads.Days[day]
			if cur == nil {
				ads.Days[day] = m
				continue
			}
			for n, c := range m {
				cur[n] += c
			}
		}
	}
}

func (s *adStats) add(name string) {
	now := time.Now()
	day := now.Format("2006-01-02")
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.Days[day]
	if m == nil {
		m = map[string]int{}
		s.Days[day] = m
		edge := now.AddDate(0, 0, -adKeepDays).Format("2006-01-02")
		for d := range s.Days {
			if d < edge {
				delete(s.Days, d)
			}
		}
	}
	// Имён в сутки бывает тысячи; потолок — чтобы файл не рос без меры.
	if _, ok := m[name]; ok || len(m) < 5000 {
		m[name]++
	}
	s.recent = append(s.recent, AdHit{name, now.Unix()})
	if len(s.recent) > 100 {
		s.recent = s.recent[len(s.recent)-100:]
	}
	s.dirty = true
	if s.path != "" && now.After(s.saveAt) {
		s.saveAt = now.Add(time.Minute)
		b, _ := json.Marshal(struct {
			Days map[string]map[string]int `json:"days"`
		}{s.Days})
		path := s.path
		go func() {
			tmp := path + ".tmp"
			if os.WriteFile(tmp, b, 0o600) == nil {
				_ = os.Rename(tmp, path)
			}
		}()
	}
}

// AdStatsJSON — для страницы плагина: по дням (14), самые частые за сегодня и
// за 7 дней, последние, итоги.
func AdStatsJSON() string {
	type day struct {
		Date string `json:"date"`
		N    int    `json:"n"`
	}
	type top struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}
	ads.mu.Lock()
	now := time.Now()
	var days []day
	week := map[string]int{}
	total, today, week7 := 0, 0, 0
	for i := 13; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		n := 0
		for name, c := range ads.Days[d] {
			n += c
			if i < 7 {
				week[name] += c
			}
		}
		days = append(days, day{d, n})
		if i == 0 {
			today = n
		}
		if i < 7 {
			week7 += n
		}
	}
	for _, m := range ads.Days {
		for _, c := range m {
			total += c
		}
	}
	tops := func(m map[string]int) []top {
		out := make([]top, 0, len(m))
		for n, c := range m {
			out = append(out, top{n, c})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].N > out[j].N || out[i].N == out[j].N && out[i].Name < out[j].Name })
		if len(out) > 30 {
			out = out[:30]
		}
		return out
	}
	todayTop := tops(ads.Days[now.Format("2006-01-02")])
	weekTop := tops(week)
	recent := make([]AdHit, len(ads.recent))
	for i, h := range ads.recent {
		recent[len(recent)-1-i] = h
	}
	ads.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"days": days, "today": today, "week": week7, "month": total,
		"topToday": todayTop, "topWeek": weekTop, "recent": recent})
	return string(b)
}
