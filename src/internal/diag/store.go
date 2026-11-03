package diag

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Store — что диагностика помнит между запусками (diag.json в папке
// настроек): список «популярных», история проверок сайтов, свежие
// результаты, «Что я отключал». Переживает перезапуск и обновление.
type Store struct {
	path string
	mu   sync.Mutex
	d    storeData
}

type storeData struct {
	Popular  []string     `json:"popular,omitempty"`
	History  []SiteResult `json:"history,omitempty"`
	Meddlers *MeddlerScan `json:"meddlers,omitempty"`
	Disabled []Undo       `json:"disabled,omitempty"`
	Leaks    *LeakReport  `json:"leaks,omitempty"`
}

// historyMax — сколько последних проверок сайтов помнить.
const historyMax = 10

// defaultPopular — «Проверить популярные» по умолчанию; список правится.
var defaultPopular = []string{
	"youtube.com", "web.telegram.org", "discord.com", "instagram.com",
	"x.com", "chatgpt.com", "linkedin.com", "facebook.com",
}

// OpenStore читает diag.json из папки dir.
func OpenStore(dir string) *Store {
	s := &Store{path: filepath.Join(dir, "diag.json")}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.d)
	}
	return s
}

func (s *Store) save() {
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o700)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

func (s *Store) update(f func(d *storeData)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.d)
	s.save()
}

func (s *Store) read() storeData {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.d
	d.History = append([]SiteResult(nil), s.d.History...)
	d.Disabled = append([]Undo(nil), s.d.Disabled...)
	d.Popular = append([]string(nil), s.d.Popular...)
	if len(d.Popular) == 0 {
		d.Popular = append([]string(nil), defaultPopular...)
	}
	return d
}

// addHistory — новая проверка сайта в начало, хвост за пределами 10 — прочь.
func (s *Store) addHistory(r SiteResult) {
	s.update(func(d *storeData) {
		d.History = append([]SiteResult{r}, d.History...)
		if len(d.History) > historyMax {
			d.History = d.History[:historyMax]
		}
	})
}

// putUndo — запись «что я отключал» (новая или замена по ID).
func (s *Store) putUndo(u Undo) {
	s.update(func(d *storeData) {
		for i := range d.Disabled {
			if d.Disabled[i].ID == u.ID {
				d.Disabled[i] = u
				return
			}
		}
		d.Disabled = append(d.Disabled, u)
	})
}

func (s *Store) getUndo(id string) (Undo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.d.Disabled {
		if u.ID == id {
			return u, true
		}
	}
	return Undo{}, false
}

func (s *Store) dropUndo(id string) {
	s.update(func(d *storeData) {
		out := d.Disabled[:0]
		for _, u := range d.Disabled {
			if u.ID != id {
				out = append(out, u)
			}
		}
		d.Disabled = out
	})
}
