// Package devfiles — плагин «Файлы устройства» (ТЗ 4,
// п. 13 и бывший 15): смотреть, качать и загружать файлы других устройств
// сети.
//
// Служба файлов работает внутри программы на порту Port сети устройств
// (mesh.Serve): снаружи сети устройств её не достать, а кто пришёл, знает
// meshd. По умолчанию не открыто ничего: на каждом устройстве сам отмечаешь
// папки «видны другим», у папки — «можно менять» (выключено).
package devfiles

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

// Port — порт службы файлов в сети устройств. Настоящий порт 127.0.0.1 он
// не занимает: соединения на него обслуживает программа.
const Port = 47835

// Share — папка, открытая другим устройствам сети.
type Share struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	Write bool   `json:"write"` // «можно менять»
}

var (
	shMu sync.Mutex
)

func sharesPath() string { return filepath.Join(addons.DataDir(), "files.json") }

// Shares — открытые папки этого устройства.
func Shares() []Share {
	shMu.Lock()
	defer shMu.Unlock()
	return loadShares()
}

func loadShares() []Share {
	var out []Share
	if data, err := os.ReadFile(sharesPath()); err == nil {
		_ = json.Unmarshal(data, &out)
	}
	if out == nil {
		out = []Share{}
	}
	return out
}

func saveShares(list []Share) error {
	if err := os.MkdirAll(addons.DataDir(), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(list, "", "  ")
	tmp := sharesPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, sharesPath())
}

// AddShare открывает папку другим (только смотреть).
func AddShare(path, name string) (Share, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return Share{}, errors.New("нужен полный путь к папке")
	}
	path = filepath.Clean(path)
	st, err := os.Stat(path)
	if err != nil {
		return Share{}, errors.New("папки нет или к ней нет доступа")
	}
	if !st.IsDir() {
		return Share{}, errors.New("это не папка")
	}
	if name = strings.TrimSpace(name); name == "" {
		name = filepath.Base(path)
	}
	shMu.Lock()
	defer shMu.Unlock()
	list := loadShares()
	for _, s := range list {
		if s.Path == path {
			return s, nil
		}
	}
	b := make([]byte, 5)
	rand.Read(b)
	s := Share{ID: hex.EncodeToString(b), Name: name, Path: path}
	list = append(list, s)
	return s, saveShares(list)
}

// RemoveShare закрывает папку.
func RemoveShare(id string) error {
	shMu.Lock()
	defer shMu.Unlock()
	list := loadShares()
	out := list[:0]
	for _, s := range list {
		if s.ID != id {
			out = append(out, s)
		}
	}
	return saveShares(out)
}

// SetWrite — галочка «можно менять».
func SetWrite(id string, on bool) error {
	shMu.Lock()
	defer shMu.Unlock()
	list := loadShares()
	for i := range list {
		if list[i].ID == id {
			list[i].Write = on
			return saveShares(list)
		}
	}
	return errors.New("папка не найдена")
}

func findShare(id string) (Share, bool) {
	for _, s := range Shares() {
		if s.ID == id {
			return s, true
		}
	}
	return Share{}, false
}

var errOutside = errors.New("путь вне открытой папки")

// resolve — настоящий путь для p внутри папки s. p — путь с «/» от корня
// папки. Выйти наружу нельзя ни через «..», ни через ссылку: проверяется
// уже разрешённый путь (для ещё не существующего — его родитель).
func resolve(s Share, p string) (string, error) {
	rel := filepath.FromSlash(strings.TrimPrefix(cleanRel(p), "/"))
	full := filepath.Join(s.Path, rel)
	root, err := filepath.EvalSymlinks(s.Path)
	if err != nil {
		return "", errors.New("открытой папки больше нет")
	}
	check := full
	real, err := filepath.EvalSymlinks(check)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent, err2 := filepath.EvalSymlinks(filepath.Dir(full))
		if err2 != nil {
			return "", errors.New("папки нет")
		}
		real = filepath.Join(parent, filepath.Base(full))
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return "", errOutside
	}
	return full, nil
}

// cleanRel — путь вида «/a/b» без «..» и лишних слешей.
func cleanRel(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	parts := []string{}
	for _, x := range strings.Split(p, "/") {
		switch x {
		case "", ".":
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, x)
		}
	}
	return "/" + strings.Join(parts, "/")
}

// validName — имя файла или папки без разделителей и «..».
func validName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, `/\`+"\x00") && len(n) <= 255
}
