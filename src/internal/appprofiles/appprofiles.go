// Пакет appprofiles — профили приложения: несколько полных наборов настроек
// (серверы, программы, галочки, оформление), между которыми можно быстро
// переключаться. Например, свой и мамин: второй настраиваешь с нуля, потом
// экспортируешь и отдаёшь ей.
//
// Действующий профиль живёт в обычном config.json — программа о профилях
// ничего не знает. Остальные лежат снимками в папке appprofiles/ рядом с
// ним, а список и выбранный — в appprofiles.json. Переключение: снимок
// текущих настроек в свой файл, настройки выбранного — в config.json.
package appprofiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"sshtunnel/internal/config"
)

// Profile — один профиль. Name пустое — интерфейс пишет «Профиль N».
type Profile struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// State — список профилей. Enabled — галочка «Профили приложения»: без неё
// полоска профилей в настройках не показывается, но сами профили целы.
type State struct {
	Enabled  bool      `json:"enabled"`
	Active   string    `json:"active"`
	Profiles []Profile `json:"profiles"`
}

// Store хранит профили в папке настроек dir.
type Store struct {
	dir string
	mu  sync.Mutex
}

func New(dir string) *Store { return &Store{dir: dir} }

func (s *Store) indexPath() string { return filepath.Join(s.dir, "appprofiles.json") }

func (s *Store) snapPath(id string) string {
	return filepath.Join(s.dir, "appprofiles", id+".json")
}

// Load — текущий список. Файла ещё нет — один профиль, он же действующий.
func (s *Store) Load() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() State {
	st := State{}
	if data, err := os.ReadFile(s.indexPath()); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if len(st.Profiles) == 0 {
		st.Profiles = []Profile{{ID: "p1"}}
	}
	if st.index(st.Active) < 0 {
		st.Active = st.Profiles[0].ID
	}
	return st
}

func (st State) index(id string) int {
	for i, p := range st.Profiles {
		if p.ID == id {
			return i
		}
	}
	return -1
}

func (s *Store) save(st State) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.indexPath(), data, 0o600)
}

func (s *Store) writeSnap(id string, cfg config.Config) error {
	if err := os.MkdirAll(filepath.Dir(s.snapPath(id)), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.snapPath(id), data, 0o600)
}

// readSnap — настройки профиля. Снимка нет (профиль только что создан и
// ещё ни разу не покидался) — настройки с нуля.
func (s *Store) readSnap(id string, base config.Config) (config.Config, error) {
	data, err := os.ReadFile(s.snapPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return fresh(base), nil
	}
	if err != nil {
		return config.Config{}, err
	}
	return config.Parse(data)
}

// fresh — настройки нового профиля: «с нуля», без серверов. Язык и
// оформление берём текущие — менять их ради нового профиля никто не хочет.
func fresh(base config.Config) config.Config {
	c := config.Default()
	c.Language, c.Theme, c.Background = base.Language, base.Theme, base.Background
	return c
}

// SetEnabled включает или выключает полоску профилей.
func (s *Store) SetEnabled(on bool) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.load()
	st.Enabled = on
	return st, s.save(st)
}

// Add создаёт новый профиль и делает его действующим. cur — настройки,
// которые сейчас в config.json: они уходят в снимок прежнего профиля.
// Возвращает настройки нового профиля — их надо применить.
func (s *Store) Add(cur config.Config) (State, config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.load()
	if err := s.writeSnap(st.Active, cur); err != nil {
		return st, config.Config{}, err
	}
	id := ""
	for n := len(st.Profiles) + 1; ; n++ {
		id = fmt.Sprintf("p%d", n)
		if st.index(id) < 0 {
			break
		}
	}
	// Снимок с тем же именем мог остаться от удалённого профиля.
	_ = os.Remove(s.snapPath(id))
	st.Profiles = append(st.Profiles, Profile{ID: id})
	st.Active = id
	if err := s.save(st); err != nil {
		return st, config.Config{}, err
	}
	return st, fresh(cur), nil
}

// Switch делает действующим профиль id. Возвращает его настройки.
func (s *Store) Switch(id string, cur config.Config) (State, config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.load()
	if st.index(id) < 0 {
		return st, config.Config{}, errors.New("нет такого профиля")
	}
	if id == st.Active {
		return st, cur, nil
	}
	if err := s.writeSnap(st.Active, cur); err != nil {
		return st, config.Config{}, err
	}
	next, err := s.readSnap(id, cur)
	if err != nil {
		return st, config.Config{}, err
	}
	st.Active = id
	return st, next, s.save(st)
}

// Delete удаляет профиль со всеми его настройками. Последний не удаляется.
// Удалили действующий — действующим становится соседний, и его настройки
// возвращаются вторым значением (иначе — nil: применять нечего).
func (s *Store) Delete(id string, cur config.Config) (State, *config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.load()
	i := st.index(id)
	if i < 0 {
		return st, nil, errors.New("нет такого профиля")
	}
	if len(st.Profiles) == 1 {
		return st, nil, errors.New("последний профиль удалить нельзя")
	}
	var next *config.Config
	if id == st.Active {
		j := i - 1
		if j < 0 {
			j = 1
		}
		c, err := s.readSnap(st.Profiles[j].ID, cur)
		if err != nil {
			return st, nil, err
		}
		next = &c
		st.Active = st.Profiles[j].ID
	}
	st.Profiles = append(st.Profiles[:i:i], st.Profiles[i+1:]...)
	if err := s.save(st); err != nil {
		return st, nil, err
	}
	_ = os.Remove(s.snapPath(id))
	return st, next, nil
}
