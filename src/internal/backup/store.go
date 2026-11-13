// Package backup — «Резервные копии» (ТЗ 13): устройство по расписанию
// кладёт папку на сервер сети с версиями и может восстановить из неё файл
// или всю папку.
//
// Серверная половина — Store (этот файл и proto.go): работает в хосте
// плагинов сервера (internal/node), по одному Store на сеть. Устройство —
// модуль приложения (module.go, client.go).
//
// Хранение (всё в папке Store, на сервере — nets/<сеть>/backup/):
//
//	<scope>/blobs/<2 знака>/<sha256>          содержимое файла, один раз
//	<scope>/incoming/<sha256>.part            недогруженное (докачка)
//	<scope>/sets/<устройство>/<копия>/set.json  имя копии, сколько версий держать
//	<scope>/sets/<устройство>/<копия>/<версия>.json  снимок: список файлов
//
// scope — «чьё»: SSH-пользователь устройства (u-<хеш>) или, если его нет,
// само устройство (d-<хеш>). Одинаковое содержимое хранится один раз только
// внутри scope: так копии одного человека с разных устройств не занимают
// места дважды, а чужой scope нельзя ни прочитать, ни даже проверить, есть
// ли там такой файл (сравнение хешей не выходит за scope).
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Caller — кто пришёл (meshd: op who).
type Caller struct {
	Device string
	Name   string
	User   string
}

// Scope — чьи копии: пользователь или (без него) само устройство.
func (c Caller) Scope() string {
	if c.User != "" {
		return "u-" + short(c.User)
	}
	return "d-" + short(c.Device)
}

func short(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:16])
}

// File — файл в снимке.
type File struct {
	P     string `json:"p"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime,omitempty"` // UnixNano
	Mode  uint32 `json:"mode,omitempty"`
	Hash  string `json:"hash"`
}

// Skipped — файл, который не попал в копию, и почему.
type Skipped struct {
	P   string `json:"p"`
	Why string `json:"why"`
}

// Manifest — одна версия копии.
type Manifest struct {
	ID         string    `json:"id"`
	Time       int64     `json:"time"` // unix, часы сервера
	Device     string    `json:"device"`
	DeviceName string    `json:"deviceName,omitempty"`
	Set        string    `json:"set"`
	Name       string    `json:"name"`
	Files      []File    `json:"files"`
	Dirs       []string  `json:"dirs,omitempty"`
	Skipped    []Skipped `json:"skipped,omitempty"`
}

// Bytes — сколько весят файлы версии.
func (m Manifest) Bytes() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// Version — версия в списке.
type Version struct {
	ID      string `json:"id"`
	Time    int64  `json:"time"`
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
	Skipped int    `json:"skipped,omitempty"`
}

// SetInfo — копия (папка одного устройства) на сервере.
type SetInfo struct {
	Device     string `json:"device"`
	DeviceName string `json:"deviceName,omitempty"`
	Set        string `json:"set"`
	Name       string `json:"name"`
	Keep       int    `json:"keep"`
	Versions   int    `json:"versions"`
	Bytes      int64  `json:"bytes"` // последняя версия
	Last       int64  `json:"last,omitempty"`
	Own        bool   `json:"own,omitempty"` // копия того, кто спрашивает
	Scope      string `json:"scope,omitempty"`
}

type setConf struct {
	Device     string `json:"device"`
	DeviceName string `json:"deviceName,omitempty"`
	Name       string `json:"name"`
	Keep       int    `json:"keep"`
}

// MaxKeep — больше версий одной копии не держим.
const MaxKeep = 365

// Store — резервные копии одной сети на сервере.
type Store struct {
	dir string
	now func() time.Time
	// Room — сколько ещё можно записать (общая квота плагинов); nil — без
	// предела. OnUsage — занятое место изменилось на delta байт (сразу).
	Room    func() (left int64, why string)
	OnUsage func(delta int64)
	// OnEvent — заметное для журнала сервера.
	OnEvent func(text string, isErr bool)

	mu     sync.Mutex
	scopes map[string]*sync.Mutex
}

// OpenStore — хранилище в dir (создаётся по нужде).
func OpenStore(dir string) *Store {
	return &Store{dir: dir, now: time.Now, scopes: map[string]*sync.Mutex{}}
}

// lock — замок scope: загрузки, снимки и чистка одного scope по очереди.
func (s *Store) lock(scope string) func() {
	s.mu.Lock()
	m := s.scopes[scope]
	if m == nil {
		m = &sync.Mutex{}
		s.scopes[scope] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (s *Store) usage(d int64) {
	if d != 0 && s.OnUsage != nil {
		s.OnUsage(d)
	}
}

func (s *Store) event(text string, isErr bool) {
	if s.OnEvent != nil {
		s.OnEvent(text, isErr)
	}
}

// ErrFull — место кончилось.
type ErrFull struct{ Why string }

func (e *ErrFull) Error() string { return e.Why }

// ErrChanged — загруженное не совпало с хешем: файл менялся при чтении.
var ErrChanged = errors.New("содержимое не совпало с хешем: файл менялся, пока его читали")

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// validID — id копии или версии: из него собирается путь.
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (s *Store) blobPath(scope, h string) string {
	return filepath.Join(s.dir, scope, "blobs", h[:2], h)
}

func (s *Store) partPath(scope, h string) string {
	return filepath.Join(s.dir, scope, "incoming", h+".part")
}

func (s *Store) setDir(scope, device, set string) string {
	return filepath.Join(s.dir, scope, "sets", short(device), set)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Missing — каких содержимых из hashes нет в scope.
func (s *Store) Missing(c Caller, hashes []string) []string {
	scope := c.Scope()
	out := []string{}
	for _, h := range hashes {
		if validHash(h) && !exists(s.blobPath(scope, h)) {
			out = append(out, h)
		}
	}
	return out
}

// PartSize — сколько уже загружено содержимого h.
func (s *Store) PartSize(c Caller, h string) int64 {
	if !validHash(h) {
		return 0
	}
	if st, err := os.Stat(s.partPath(c.Scope(), h)); err == nil {
		return st.Size()
	}
	return 0
}

// Put — кусок содержимого h с места off; size — полный размер. Дошли до
// size — содержимое проверяется по хешу и ложится в blobs. set/keep —
// какая копия грузит: если места нет, сначала уходит её самая старая
// версия сверх keep (она всё равно выпала бы после этой копии).
func (s *Store) Put(c Caller, h string, off, size int64, set string, keep int, r io.Reader) (int64, error) {
	if !validHash(h) || size < 0 || off < 0 || off > size {
		return 0, errors.New("неверный хеш или размер")
	}
	scope := c.Scope()
	unlock := s.lock(scope)
	defer unlock()
	if exists(s.blobPath(scope, h)) {
		return size, nil
	}
	p := s.partPath(scope, h)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return 0, err
	}
	pf, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer pf.Close()
	st, err := pf.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size() != off {
		return st.Size(), errOffset
	}
	if need := size - off; need > 0 {
		if err := s.makeRoom(scope, c.Device, set, keep, need); err != nil {
			return off, err
		}
	}
	if _, err := pf.Seek(off, io.SeekStart); err != nil {
		return off, err
	}
	n, err := io.Copy(pf, io.LimitReader(r, size-off))
	if serr := pf.Sync(); err == nil {
		err = serr
	}
	s.usage(n)
	have := off + n
	if err != nil || have < size {
		return have, err
	}
	pf.Close()
	// Целиком: проверить и положить.
	got, err := hashOf(p)
	if err != nil {
		return have, err
	}
	if got != h {
		os.Remove(p)
		s.usage(-have)
		return 0, ErrChanged
	}
	dst := s.blobPath(scope, h)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return have, err
	}
	if err := os.Rename(p, dst); err != nil {
		return have, err
	}
	return size, nil
}

var errOffset = errors.New("не то место докачки")

func hashOf(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// makeRoom — нужно ещё need байт. Нет места — по очереди убираются самые
// старые версии этой копии сверх keep-1 (с новой их станет keep); всё
// равно мало — ErrFull. Под замком scope.
func (s *Store) makeRoom(scope, device, set string, keep int, need int64) error {
	if s.Room == nil {
		return nil
	}
	for {
		left, why := s.Room()
		if need <= left {
			return nil
		}
		if keep <= 0 || !validID(set) {
			return &ErrFull{why}
		}
		vs := s.versionsLocked(scope, device, set)
		if len(vs) < keep || len(vs) == 0 {
			return &ErrFull{why}
		}
		old := vs[len(vs)-1] // список — новые сверху
		if err := s.dropVersionLocked(scope, device, set, old.ID); err != nil {
			return err
		}
		s.event(fmt.Sprintf("место кончилось: убрана самая старая версия копии (%s) — с новой их будет %d", time.Unix(old.Time, 0).Format("02.01.2006 15:04"), keep), false)
		s.gcLocked(scope)
	}
}

func readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(p string, v any) (int64, error) {
	b, _ := json.Marshal(v)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return 0, err
	}
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return int64(len(b)), nil
}

func fileSize(p string) int64 {
	if st, err := os.Stat(p); err == nil {
		return st.Size()
	}
	return 0
}

// CommitReq — новая версия копии.
type CommitReq struct {
	Set     string    `json:"set"`
	Name    string    `json:"name"`
	Keep    int       `json:"keep"`
	Files   []File    `json:"files"`
	Dirs    []string  `json:"dirs,omitempty"`
	Skipped []Skipped `json:"skipped,omitempty"`
}

// cleanRel — путь внутри копии: относительный, через «/», без «..».
func cleanRel(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Commit — записать версию: все её содержимые уже должны лежать в scope.
// Лишние версии сверх keep и ничьи больше содержимые убираются.
func (s *Store) Commit(c Caller, req CommitReq) (Version, error) {
	if !validID(req.Set) {
		return Version{}, errors.New("неверный id копии")
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 100 {
		return Version{}, errors.New("имя копии — от 1 до 100 символов")
	}
	if req.Keep < 1 || req.Keep > MaxKeep {
		return Version{}, fmt.Errorf("версий — от 1 до %d", MaxKeep)
	}
	scope := c.Scope()
	unlock := s.lock(scope)
	defer unlock()
	var missing []string
	for _, f := range req.Files {
		if !cleanRel(f.P) || !validHash(f.Hash) || f.Size < 0 {
			return Version{}, fmt.Errorf("неверная запись файла %q", f.P)
		}
		if !exists(s.blobPath(scope, f.Hash)) {
			missing = append(missing, f.P)
		}
	}
	for _, d := range req.Dirs {
		if !cleanRel(d) {
			return Version{}, fmt.Errorf("неверная папка %q", d)
		}
	}
	if len(missing) > 0 {
		return Version{}, fmt.Errorf("на сервере нет содержимого %d файлов (например, %s) — загрузи их", len(missing), missing[0])
	}
	now := s.now()
	m := Manifest{ID: now.UTC().Format("20060102T150405") + "-" + short(fmt.Sprint(now.UnixNano(), c.Device))[:6],
		Time: now.Unix(), Device: c.Device, DeviceName: c.Name, Set: req.Set, Name: req.Name,
		Files: req.Files, Dirs: req.Dirs, Skipped: req.Skipped}
	dir := s.setDir(scope, c.Device, req.Set)
	before := fileSize(filepath.Join(dir, "set.json"))
	n, err := writeJSON(filepath.Join(dir, "set.json"), setConf{Device: c.Device, DeviceName: c.Name, Name: req.Name, Keep: req.Keep})
	if err != nil {
		return Version{}, err
	}
	s.usage(n - before)
	if n, err = writeJSON(filepath.Join(dir, m.ID+".json"), m); err != nil {
		return Version{}, err
	}
	s.usage(n)
	vs := s.versionsLocked(scope, c.Device, req.Set)
	for i := req.Keep; i < len(vs); i++ {
		_ = s.dropVersionLocked(scope, c.Device, req.Set, vs[i].ID)
	}
	s.gcLocked(scope)
	return Version{ID: m.ID, Time: m.Time, Files: len(m.Files), Bytes: m.Bytes(), Skipped: len(m.Skipped)}, nil
}

// versionsLocked — версии копии, новые сверху.
func (s *Store) versionsLocked(scope, device, set string) []Version {
	dir := s.setDir(scope, device, set)
	es, _ := os.ReadDir(dir)
	out := []Version{}
	for _, e := range es {
		name := e.Name()
		if e.IsDir() || name == "set.json" || !strings.HasSuffix(name, ".json") {
			continue
		}
		var m Manifest
		if readJSON(filepath.Join(dir, name), &m) != nil || m.ID+".json" != name {
			continue
		}
		out = append(out, Version{ID: m.ID, Time: m.Time, Files: len(m.Files), Bytes: m.Bytes(), Skipped: len(m.Skipped)})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Time > out[j].Time || out[i].Time == out[j].Time && out[i].ID > out[j].ID
	})
	return out
}

func (s *Store) dropVersionLocked(scope, device, set, id string) error {
	if !validID(id) {
		return errors.New("неверная версия")
	}
	p := filepath.Join(s.setDir(scope, device, set), id+".json")
	n := fileSize(p)
	if err := os.Remove(p); err != nil {
		return err
	}
	s.usage(-n)
	return nil
}

// gcLocked — убрать содержимые, на которые не ссылается ни одна версия
// scope, и брошенные загрузки старше двух суток.
func (s *Store) gcLocked(scope string) {
	used := map[string]bool{}
	setsDir := filepath.Join(s.dir, scope, "sets")
	_ = filepath.WalkDir(setsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "set.json" || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		var m Manifest
		if readJSON(p, &m) == nil {
			for _, f := range m.Files {
				used[f.Hash] = true
			}
		}
		return nil
	})
	blobs := filepath.Join(s.dir, scope, "blobs")
	_ = filepath.WalkDir(blobs, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !used[d.Name()] {
			n := fileSize(p)
			if os.Remove(p) == nil {
				s.usage(-n)
			}
		}
		return nil
	})
	if es, err := os.ReadDir(filepath.Join(s.dir, scope, "incoming")); err == nil {
		for _, e := range es {
			if fi, err := e.Info(); err == nil && s.now().Sub(fi.ModTime()) > 48*time.Hour {
				if os.Remove(filepath.Join(s.dir, scope, "incoming", e.Name())) == nil {
					s.usage(-fi.Size())
				}
			}
		}
	}
	// Пустые папки копий (все версии убраны) — прочь вместе с set.json.
	devs, _ := os.ReadDir(setsDir)
	for _, d := range devs {
		sets, _ := os.ReadDir(filepath.Join(setsDir, d.Name()))
		for _, st := range sets {
			dir := filepath.Join(setsDir, d.Name(), st.Name())
			es, _ := os.ReadDir(dir)
			if len(es) == 1 && es[0].Name() == "set.json" {
				n := fileSize(filepath.Join(dir, "set.json"))
				if os.RemoveAll(dir) == nil {
					s.usage(-n)
				}
			}
		}
		if es, _ := os.ReadDir(filepath.Join(setsDir, d.Name())); len(es) == 0 {
			os.Remove(filepath.Join(setsDir, d.Name()))
		}
	}
}

// scopeSets — копии одного scope.
func (s *Store) scopeSets(scope string) []SetInfo {
	setsDir := filepath.Join(s.dir, scope, "sets")
	out := []SetInfo{}
	devs, _ := os.ReadDir(setsDir)
	for _, d := range devs {
		sets, _ := os.ReadDir(filepath.Join(setsDir, d.Name()))
		for _, st := range sets {
			var sc setConf
			if readJSON(filepath.Join(setsDir, d.Name(), st.Name(), "set.json"), &sc) != nil {
				continue
			}
			vs := s.versionsLocked(scope, sc.Device, st.Name())
			if len(vs) == 0 {
				continue
			}
			out = append(out, SetInfo{Device: sc.Device, DeviceName: sc.DeviceName, Set: st.Name(), Name: sc.Name,
				Keep: sc.Keep, Versions: len(vs), Bytes: vs[0].Bytes, Last: vs[0].Time, Scope: scope})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last > out[j].Last })
	return out
}

// Sets — копии, которые видит c: свои и других устройств того же
// пользователя.
func (s *Store) Sets(c Caller) []SetInfo {
	scope := c.Scope()
	unlock := s.lock(scope)
	defer unlock()
	out := s.scopeSets(scope)
	for i := range out {
		out[i].Own = out[i].Device == c.Device
		out[i].Scope = ""
	}
	return out
}

// Versions — версии копии (device, set), если c может её читать.
func (s *Store) Versions(c Caller, device, set string) ([]Version, error) {
	if !validID(set) {
		return nil, errors.New("неверный id копии")
	}
	scope := c.Scope()
	unlock := s.lock(scope)
	defer unlock()
	vs := s.versionsLocked(scope, device, set)
	if len(vs) == 0 {
		return nil, errors.New("такой копии нет")
	}
	return vs, nil
}

// Manifest — список файлов версии.
func (s *Store) Manifest(c Caller, device, set, id string) (Manifest, error) {
	if !validID(set) || !validID(id) {
		return Manifest{}, errors.New("неверная копия или версия")
	}
	var m Manifest
	if err := readJSON(filepath.Join(s.setDir(c.Scope(), device, set), id+".json"), &m); err != nil {
		return Manifest{}, errors.New("такой версии нет")
	}
	return m, nil
}

// OpenBlob — содержимое h из scope c (чужого scope не видно вовсе).
func (s *Store) OpenBlob(c Caller, h string) (*os.File, error) {
	if !validHash(h) {
		return nil, errors.New("неверный хеш")
	}
	f, err := os.Open(s.blobPath(c.Scope(), h))
	if err != nil {
		return nil, errors.New("нет такого содержимого")
	}
	return f, nil
}

// Delete — убрать копию целиком (version == "") или одну версию.
func (s *Store) Delete(c Caller, device, set, version string) error {
	return s.deleteIn(c.Scope(), device, set, version)
}

func (s *Store) deleteIn(scope, device, set, version string) error {
	if !validID(set) {
		return errors.New("неверный id копии")
	}
	unlock := s.lock(scope)
	defer unlock()
	vs := s.versionsLocked(scope, device, set)
	if len(vs) == 0 {
		return errors.New("такой копии нет")
	}
	for _, v := range vs {
		if version == "" || v.ID == version {
			if err := s.dropVersionLocked(scope, device, set, v.ID); err != nil {
				return err
			}
		}
	}
	s.gcLocked(scope)
	return nil
}

// AllSets — все копии сети (панель сервера).
func (s *Store) AllSets() []SetInfo {
	out := []SetInfo{}
	scopes, _ := os.ReadDir(s.dir)
	for _, sc := range scopes {
		if !sc.IsDir() || !(strings.HasPrefix(sc.Name(), "u-") || strings.HasPrefix(sc.Name(), "d-")) {
			continue
		}
		unlock := s.lock(sc.Name())
		out = append(out, s.scopeSets(sc.Name())...)
		unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last > out[j].Last })
	return out
}

// DeleteAny — панель сервера: убрать копию (scope из AllSets).
func (s *Store) DeleteAny(scope, device, set string) error {
	if !(strings.HasPrefix(scope, "u-") || strings.HasPrefix(scope, "d-")) || strings.ContainsAny(scope, `/\.`) {
		return errors.New("неверный scope")
	}
	return s.deleteIn(scope, device, set, "")
}

// Stored — сколько байт на диске занимают копии сети.
func (s *Store) Stored() int64 {
	var n int64
	_ = filepath.WalkDir(s.dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
