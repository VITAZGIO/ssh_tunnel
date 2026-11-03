// Package sshapp — плагин «SSH» (ТЗ 4, п. 12): свой
// список серверов на каждом устройстве, вход по ключу, который создаёт
// программа. Ключ лежит только зашифрованным (seal.go); терминалу ПК он
// отдаётся через свой агент на время входа (agent*.go), встроенный терминал
// телефона входит сам (term.go).
package sshapp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"sshtunnel/internal/addons"
)

// Server — сервер в списке.
type Server struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Host — адрес: имя, IP или адрес в сети устройств (198.19.x.y).
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	// HostKey — ключ сервера (как в known_hosts, без имени), запомненный
	// при первом входе. Другой ключ — отказ: сервер подменили или
	// переустановили («Забыть ключ сервера»).
	HostKey string `json:"hostKey,omitempty"`
}

type stored struct {
	Servers []Server `json:"servers"`
}

var (
	mu     sync.Mutex // список серверов
	dirMu  sync.Mutex
	dirSet string
)

// SetDir — папка данных, если программа не поднимала addons.Service (окно
// VPN-версии Windows: там SSH обслуживает процесс окна пользователя).
func SetDir(dir string) {
	dirMu.Lock()
	dirSet = dir
	dirMu.Unlock()
}

func dir() string {
	dirMu.Lock()
	d := dirSet
	dirMu.Unlock()
	if d == "" {
		d = addons.DataDir()
	}
	return d
}

func listPath() string { return filepath.Join(dir(), "ssh.json") }

func load() stored {
	var s stored
	if data, err := os.ReadFile(listPath()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	if s.Servers == nil {
		s.Servers = []Server{}
	}
	return s
}

func save(s stored) error {
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	tmp := listPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, listPath())
}

// Servers — список серверов этого устройства.
func Servers() []Server {
	mu.Lock()
	defer mu.Unlock()
	return load().Servers
}

// Get — сервер по id.
func Get(id string) (Server, bool) {
	for _, s := range Servers() {
		if s.ID == id {
			return s, true
		}
	}
	return Server{}, false
}

// Save добавляет (пустой ID) или меняет сервер. Сменился адрес или порт —
// запомненный ключ сервера забывается: это уже другой сервер.
func Save(in Server) (Server, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.TrimSpace(in.Host)
	in.User = strings.TrimSpace(in.User)
	if in.Host == "" || in.User == "" {
		return in, errors.New("нужны адрес и логин")
	}
	if strings.ContainsAny(in.Host+in.User, " \t'\"`;&|<>$") {
		return in, errors.New("в адресе или логине недопустимые символы")
	}
	if in.Port <= 0 || in.Port > 65535 {
		in.Port = 22
	}
	if in.Name == "" {
		in.Name = in.Host
	}
	mu.Lock()
	defer mu.Unlock()
	s := load()
	if in.ID == "" {
		b := make([]byte, 6)
		rand.Read(b)
		in.ID = hex.EncodeToString(b)
		in.HostKey = ""
		s.Servers = append(s.Servers, in)
		return in, save(s)
	}
	for i, old := range s.Servers {
		if old.ID == in.ID {
			in.HostKey = old.HostKey
			if old.Host != in.Host || old.Port != in.Port {
				in.HostKey = ""
			}
			s.Servers[i] = in
			return in, save(s)
		}
	}
	return in, errors.New("сервер не найден")
}

// Delete убирает сервер из списка.
func Delete(id string) error {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	out := s.Servers[:0]
	for _, x := range s.Servers {
		if x.ID != id {
			out = append(out, x)
		}
	}
	s.Servers = out
	return save(s)
}

// setHostKey запоминает (или забывает — "") ключ сервера.
func setHostKey(id, key string) error {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	for i := range s.Servers {
		if s.Servers[i].ID == id {
			s.Servers[i].HostKey = key
			return save(s)
		}
	}
	return errors.New("сервер не найден")
}
