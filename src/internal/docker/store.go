package docker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Store — выбор человека на диске (docker.json в папке настроек).
type Store struct {
	mu     sync.Mutex
	path   string
	ch     Choices
	strict map[string]bool // ключ контейнера → «без VPN — без интернета»
}

// OpenStore читает выбор из dir (пусто, если файла ещё нет).
func OpenStore(dir string) *Store {
	s := &Store{path: filepath.Join(dir, "docker.json"), ch: Choices{}, strict: map[string]bool{}}
	if b, err := os.ReadFile(s.path); err == nil {
		var f struct {
			Choices Choices         `json:"choices"`
			Strict  map[string]bool `json:"strict"`
		}
		if json.Unmarshal(b, &f) == nil {
			for k, v := range f.Strict {
				if v {
					s.strict[k] = true
				}
			}
		}
		if json.Unmarshal(b, &f) == nil && f.Choices != nil {
			for k, v := range f.Choices {
				if c, ok := ParseChoice(string(v)); ok {
					s.ch[k] = c
				}
			}
		}
	}
	return s
}

// Get — копия выбора.
func (s *Store) Get() Choices {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(Choices, len(s.ch))
	for k, v := range s.ch {
		out[k] = v
	}
	return out
}

// Strict — копия галочек «без VPN — без интернета».
func (s *Store) Strict() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.strict))
	for k := range s.strict {
		out[k] = true
	}
	return out
}

// SetStrict — галочка «без VPN — без интернета» для контейнера c.
func (s *Store) SetStrict(c Container, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		s.strict[c.Key()] = true
	} else {
		delete(s.strict, c.Key())
	}
	return s.saveLocked()
}

// SetContainer — выбор для контейнера c. «Как сам компьютер» хранится явно,
// только если у его проекта выбрано другое (иначе запись просто убирается).
func (s *Store) SetContainer(c Container, v Choice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := c.Key()
	proj := ChoiceHost
	if c.Project != "" {
		if p, ok := s.ch["project:"+c.Project]; ok {
			proj = p
		}
	}
	if v == ChoiceHost && proj == ChoiceHost {
		delete(s.ch, key)
	} else {
		s.ch[key] = v
	}
	return s.saveLocked()
}

// SetProject — выбор для проекта compose целиком: отдельные выборы его
// служб снимаются, все контейнеры проекта идут одинаково (метка — сильнее).
func (s *Store) SetProject(project string, v Choice) error {
	if project == "" {
		return errors.New("пустое имя проекта")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := "service:" + project + "/"
	for k := range s.ch {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(s.ch, k)
		}
	}
	if v == ChoiceHost {
		delete(s.ch, "project:"+project)
	} else {
		s.ch["project:"+project] = v
	}
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	b, _ := json.MarshalIndent(struct {
		Choices Choices         `json:"choices"`
		Strict  map[string]bool `json:"strict,omitempty"`
	}{s.ch, s.strict}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
