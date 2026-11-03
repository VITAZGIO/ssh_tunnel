package addons

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Entry — добавленное на этом устройстве приложение.
type Entry struct {
	ID   string `json:"id"`
	Star bool   `json:"star,omitempty"`
	// Off — фоновое приложение выключено выключателем на плитке.
	Off bool `json:"off,omitempty"`
	// Seen — первое открытие уже было, подсказку больше не показывать.
	Seen bool `json:"seen,omitempty"`
	// Win — галка «сразу в окне» (только ПК); nil — как задумано в каталоге
	// (Manifest.Window).
	Win *bool `json:"win,omitempty"`
}

type data struct {
	Added []Entry `json:"added"`
}

// Store — что добавлено на этом устройстве. Пишется в файл сразу при каждом
// изменении: всё должно пережить перезапуск и падение.
type Store struct {
	path string
	mu   sync.Mutex
	d    data
}

// Open читает addons.json из папки dir; файла нет — пустой список.
func Open(dir string) *Store {
	s := &Store{path: filepath.Join(dir, "addons.json")}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.d)
	}
	return s
}

var errUnknown = errors.New("нет такого приложения")

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Added — добавленные, в порядке добавления.
func (s *Store) Added() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Entry(nil), s.d.Added...)
}

// Get — запись о добавленном приложении.
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.d.Added {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Add добавляет приложение из каталога. Уже добавленное не трогает.
func (s *Store) Add(id string) error {
	if _, ok := Find(id); !ok {
		return errUnknown
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.d.Added {
		if e.ID == id {
			return nil
		}
	}
	s.d.Added = append(s.d.Added, Entry{ID: id})
	return s.save()
}

// Remove убирает приложение вместе со звёздочкой.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.d.Added[:0]
	for _, e := range s.d.Added {
		if e.ID != id {
			out = append(out, e)
		}
	}
	s.d.Added = out
	return s.save()
}

// update меняет запись добавленного приложения.
func (s *Store) update(id string, f func(*Entry)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.d.Added {
		if s.d.Added[i].ID == id {
			f(&s.d.Added[i])
			return s.save()
		}
	}
	return errors.New("приложение не добавлено")
}

func (s *Store) SetStar(id string, on bool) error {
	return s.update(id, func(e *Entry) { e.Star = on })
}

func (s *Store) SetWin(id string, on bool) error {
	return s.update(id, func(e *Entry) { e.Win = &on })
}

func (s *Store) SetOn(id string, on bool) error {
	return s.update(id, func(e *Entry) { e.Off = !on })
}

func (s *Store) SetSeen(id string) error {
	return s.update(id, func(e *Entry) { e.Seen = true })
}
