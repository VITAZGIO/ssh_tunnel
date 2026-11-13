package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Устройство: одна копия (RunBackup) и восстановление (Restore). Без
// глобального состояния — модуль приложения (module.go) только зовёт их по
// расписанию и с кнопок.

// Job — папка, которую это устройство копирует на сервер.
type Job struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Schedule string `json:"schedule"`           // daily | weekly | change
	DelayMin int    `json:"delayMin,omitempty"` // change: сколько ждать тишины после правки
	Keep     int    `json:"keep"`               // сколько версий держать
	Metered  bool   `json:"metered,omitempty"`  // можно по лимитной (мобильной) сети
	Server   string `json:"server,omitempty"`   // имя сервера сети; "" — любой с копиями
}

// Расписания.
const (
	Daily  = "daily"
	Weekly = "weekly"
	Change = "change"
)

// Check — проверка настроек копии.
func (j *Job) Check() error {
	j.Name = strings.TrimSpace(j.Name)
	if j.Name == "" {
		j.Name = filepath.Base(j.Path)
	}
	if len([]rune(j.Name)) > 100 {
		return errors.New("имя копии — до 100 символов")
	}
	if j.Path == "" || !filepath.IsAbs(j.Path) {
		return errors.New("нужен полный путь к папке")
	}
	if st, err := os.Stat(j.Path); err != nil || !st.IsDir() {
		return errors.New("папки нет")
	}
	switch j.Schedule {
	case Daily, Weekly:
	case Change:
		if j.DelayMin < 1 {
			j.DelayMin = 10
		}
		if j.DelayMin > 24*60 {
			return errors.New("задержка — до суток")
		}
	default:
		return errors.New("расписание: daily, weekly или change")
	}
	if j.Keep == 0 {
		j.Keep = 10
	}
	if j.Keep < 1 || j.Keep > MaxKeep {
		return fmt.Errorf("версий — от 1 до %d", MaxKeep)
	}
	return nil
}

// HashCache — хеши уже прочитанных файлов (путь → размер, время, хеш):
// неизменённое не читается заново.
type HashCache struct {
	mu    sync.Mutex
	path  string
	Files map[string]cached `json:"files"`
	dirty bool
}

type cached struct {
	Size  int64  `json:"s"`
	MTime int64  `json:"m"`
	Hash  string `json:"h"`
}

// LoadCache — кэш из файла p ("" — только в памяти).
func LoadCache(p string) *HashCache {
	c := &HashCache{path: p, Files: map[string]cached{}}
	if p != "" {
		_ = readJSON(p, c)
		if c.Files == nil {
			c.Files = map[string]cached{}
		}
	}
	return c
}

func (c *HashCache) get(rel string, size, mtime int64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.Files[rel]; ok && e.Size == size && e.MTime == mtime {
		return e.Hash
	}
	return ""
}

func (c *HashCache) put(rel string, size, mtime int64, h string) {
	c.mu.Lock()
	c.Files[rel] = cached{size, mtime, h}
	c.dirty = true
	c.mu.Unlock()
}

// Save — на диск (и выкинуть пропавшие файлы).
func (c *HashCache) Save(keep map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p := range c.Files {
		if keep != nil && !keep[p] {
			delete(c.Files, p)
			c.dirty = true
		}
	}
	if c.path != "" && c.dirty {
		if _, err := writeJSON(c.path, c); err == nil {
			c.dirty = false
		}
	}
}

// Progress — как идёт копия: этап, сделано и всего (байт).
type Progress func(phase string, done, total int64)

// Result — итог копии.
type Result struct {
	Version  Version   `json:"version"`
	Files    int       `json:"files"`
	Uploaded int64     `json:"uploaded"`
	Skipped  []Skipped `json:"skipped,omitempty"`
}

// Сколько раз перечитывать файл, который меняется, пока его читают.
var (
	stableTries = 3
	stableWait  = time.Second
)

const putChunk = 8 << 20

type entry struct {
	rel   string
	abs   string
	size  int64
	mtime int64
	mode  uint32
	hash  string
}

// scan — файлы и папки job.Path.
func scan(root string) (files []*entry, dirs []string, skipped []Skipped, err error) {
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if p == root {
			return werr
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if werr != nil {
			skipped = append(skipped, Skipped{rel, "не читается: " + werr.Error()})
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, rel)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			skipped = append(skipped, Skipped{rel, err.Error()})
			return nil
		}
		if !info.Mode().IsRegular() {
			if info.Mode()&fs.ModeSymlink != 0 {
				skipped = append(skipped, Skipped{rel, "ссылка — не копируется"})
			}
			return nil
		}
		files = append(files, &entry{rel: rel, abs: p, size: info.Size(), mtime: info.ModTime().UnixNano(), mode: uint32(info.Mode().Perm())})
		return nil
	})
	return
}

// stableHash — хеш файла, который не менялся, пока его читали: размер и
// время до и после чтения совпали. Меняется — перечитать через паузу; так и
// не успокоился — ошибка (файл в копию не попадёт, но и половиной не ляжет).
func stableHash(ctx context.Context, e *entry, cache *HashCache) error {
	for try := 0; try < stableTries; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(stableWait):
			}
		}
		st, err := os.Stat(e.abs)
		if err != nil {
			return err
		}
		e.size, e.mtime = st.Size(), st.ModTime().UnixNano()
		if h := cache.get(e.rel, e.size, e.mtime); h != "" {
			e.hash = h
			return nil
		}
		f, err := os.Open(e.abs)
		if err != nil {
			return err
		}
		hh := sha256.New()
		n, err := io.Copy(hh, f)
		f.Close()
		if err != nil {
			return err
		}
		st2, err := os.Stat(e.abs)
		if err != nil {
			return err
		}
		if n == e.size && st2.Size() == e.size && st2.ModTime().UnixNano() == e.mtime {
			e.hash = hex.EncodeToString(hh.Sum(nil))
			cache.put(e.rel, e.size, e.mtime, e.hash)
			return nil
		}
	}
	return errors.New("файл менялся, пока его читали — пропущен, попадёт в следующую копию")
}

// RunBackup — одна копия папки job на сервер rm.
func RunBackup(ctx context.Context, rm *Remote, job Job, cache *HashCache, prog Progress) (Result, error) {
	if prog == nil {
		prog = func(string, int64, int64) {}
	}
	if cache == nil {
		cache = LoadCache("")
	}
	prog("scan", 0, 0)
	files, dirs, skipped, err := scan(job.Path)
	if err != nil {
		return Result{}, fmt.Errorf("папка не читается: %w", err)
	}
	var total int64
	for _, e := range files {
		total += e.size
	}
	var done int64
	var ok []*entry
	for _, e := range files {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if err := stableHash(ctx, e, cache); err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			skipped = append(skipped, Skipped{e.rel, err.Error()})
			continue
		}
		done += e.size
		prog("hash", done, total)
		ok = append(ok, e)
	}

	// Каких содержимых нет на сервере.
	byHash := map[string][]*entry{}
	var hashes []string
	for _, e := range ok {
		if byHash[e.hash] == nil {
			hashes = append(hashes, e.hash)
		}
		byHash[e.hash] = append(byHash[e.hash], e)
	}
	missing := map[string]bool{}
	for i := 0; i < len(hashes); i += 2000 {
		end := min(i+2000, len(hashes))
		m, err := rm.Missing(ctx, hashes[i:end])
		if err != nil {
			return Result{}, err
		}
		for _, h := range m {
			missing[h] = true
		}
	}
	var upTotal, uploaded int64
	for h := range missing {
		upTotal += byHash[h][0].size
	}
	prog("upload", 0, upTotal)
	drop := map[*entry]string{}
	for _, h := range hashes {
		if !missing[h] {
			continue
		}
		for _, e := range byHash[h] {
			n, err := uploadEntry(ctx, rm, job, e, cache, func(d int64) { prog("upload", uploaded+d, upTotal) })
			if err == nil {
				uploaded += n
				// Тот же файл мог смениться при отправке: тогда у него новый
				// хеш, и другие файлы с тем же содержимым ещё надо проверить.
				if e.hash == h {
					break
				}
				continue
			}
			var re *RemoteError
			if errors.As(err, &re) || ctx.Err() != nil || !errors.Is(err, errUnstable) {
				return Result{}, err
			}
			drop[e] = err.Error()
		}
	}
	keepFiles := map[string]bool{}
	var list []File
	for _, e := range ok {
		if why, bad := drop[e]; bad {
			skipped = append(skipped, Skipped{e.rel, why})
			continue
		}
		keepFiles[e.rel] = true
		list = append(list, File{P: e.rel, Size: e.size, MTime: e.mtime, Mode: e.mode, Hash: e.hash})
	}
	cache.Save(keepFiles)
	sort.Slice(list, func(i, j int) bool { return list[i].P < list[j].P })
	prog("commit", upTotal, upTotal)
	v, err := rm.Commit(ctx, CommitReq{Set: job.ID, Name: job.Name, Keep: job.Keep, Files: list, Dirs: dirs, Skipped: skipped})
	if err != nil {
		return Result{}, err
	}
	return Result{Version: v, Files: len(list), Uploaded: uploaded, Skipped: skipped}, nil
}

var errUnstable = errors.New("файл менялся, пока его отправляли — пропущен, попадёт в следующую копию")

// uploadEntry — отправить содержимое файла с докачкой. Сервер проверяет
// хеш: файл поменялся по пути — 409, тогда перечитать и отправить заново
// (не больше stableTries раз).
func uploadEntry(ctx context.Context, rm *Remote, job Job, e *entry, cache *HashCache, onSent func(int64)) (int64, error) {
	var sent int64
	for try := 0; try < stableTries; try++ {
		if try > 0 {
			if err := stableHash(ctx, e, cache); err != nil {
				return sent, errUnstable
			}
		}
		have, err := rm.Part(ctx, e.hash)
		if err != nil {
			return sent, err
		}
		changed := false
		for have < e.size || e.size == 0 {
			f, err := os.Open(e.abs)
			if err != nil {
				return sent, err
			}
			if _, err := f.Seek(have, io.SeekStart); err != nil {
				f.Close()
				return sent, err
			}
			n := min(e.size-have, putChunk)
			got, err := rm.Put(ctx, e.hash, have, e.size, job.ID, job.Keep, io.LimitReader(f, n), n)
			f.Close()
			if err != nil {
				var re *RemoteError
				if errors.As(err, &re) && re.Code == 409 {
					changed = true
					break
				}
				return sent, err
			}
			sent += got - have
			onSent(sent)
			have = got
			if e.size == 0 {
				break
			}
		}
		if !changed {
			return sent, nil
		}
		// Сервер не принял: перечитать файл (кэш хеша сбросить).
		cache.mu.Lock()
		delete(cache.Files, e.rel)
		cache.mu.Unlock()
	}
	return sent, errUnstable
}

// ---------- восстановление ----------

// Что делать, если файл на месте уже есть и он другой.
const (
	KeepBoth  = "rename"    // положить рядом «имя (из копии …)»
	Skip      = "skip"      // оставить как есть
	Overwrite = "overwrite" // заменить (только если человек явно выбрал)
)

// RestoreReq — что и куда восстановить.
type RestoreReq struct {
	Device  string `json:"device"`
	Set     string `json:"set"`
	Version string `json:"version"`
	Path    string `json:"path,omitempty"` // файл или папка внутри копии; "" — вся копия
	Target  string `json:"target"`         // куда (полный путь)
	Mode    string `json:"mode,omitempty"` // rename (по умолчанию) | skip | overwrite
}

// RestoreResult — итог.
type RestoreResult struct {
	Written int      `json:"written"`
	Same    int      `json:"same"`    // уже такие же — не тронуты
	Renamed int      `json:"renamed"` // положены рядом под другим именем
	Skipped int      `json:"skipped"` // другие, оставлены как есть
	Failed  []string `json:"failed,omitempty"`
}

// Restore — файлы версии req (всё или под req.Path) в req.Target. Молча
// ничего не перезаписывает: другой файл на месте — рядом копия с пометкой
// (или пропуск, или замена — если так выбрано).
func Restore(ctx context.Context, rm *Remote, req RestoreReq, now time.Time) (RestoreResult, error) {
	var res RestoreResult
	if req.Target == "" || !filepath.IsAbs(req.Target) {
		return res, errors.New("куда восстановить — нужен полный путь")
	}
	switch req.Mode {
	case "":
		req.Mode = KeepBoth
	case KeepBoth, Skip, Overwrite:
	default:
		return res, errors.New("mode: rename, skip или overwrite")
	}
	sel := strings.Trim(req.Path, "/")
	if sel != "" && !cleanRel(sel) {
		return res, errors.New("неверный путь внутри копии")
	}
	m, err := rm.Manifest(ctx, req.Device, req.Set, req.Version)
	if err != nil {
		return res, err
	}
	// Выбранное ложится в Target своим последним именем: «Документы/а.txt»
	// из папки «Документы» → Target/Документы/а.txt; один файл → Target/а.txt.
	parent := ""
	if sel != "" {
		parent = path.Dir(sel)
		if parent == "." {
			parent = ""
		}
	}
	inSel := func(p string) bool { return sel == "" || p == sel || strings.HasPrefix(p, sel+"/") }
	dst := func(p string) string {
		rel := p
		if parent != "" {
			rel = strings.TrimPrefix(p, parent+"/")
		}
		return filepath.Join(req.Target, filepath.FromSlash(rel))
	}
	found := false
	for _, d := range m.Dirs {
		if inSel(d) {
			found = true
			if err := os.MkdirAll(dst(d), 0o755); err != nil {
				return res, err
			}
		}
	}
	day := now.Format("02.01.2006")
	for _, f := range m.Files {
		if !inSel(f.P) {
			continue
		}
		found = true
		if err := ctx.Err(); err != nil {
			return res, err
		}
		out := dst(f.P)
		if st, err := os.Lstat(out); err == nil {
			if st.Mode().IsRegular() && st.Size() == f.Size {
				if h, err := hashOf(out); err == nil && h == f.Hash {
					res.Same++
					continue
				}
			}
			switch req.Mode {
			case Skip:
				res.Skipped++
				continue
			case KeepBoth:
				out = freeName(out, day)
				res.Renamed++
			case Overwrite:
				if !st.Mode().IsRegular() {
					res.Failed = append(res.Failed, f.P+": на месте не файл")
					continue
				}
			}
		}
		if err := fetch(ctx, rm, f, out); err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			res.Failed = append(res.Failed, f.P+": "+err.Error())
			continue
		}
		res.Written++
	}
	if !found {
		return res, errors.New("в этой версии нет такого файла или папки")
	}
	return res, nil
}

// freeName — «имя (из копии 13.11.2026).ext», свободное.
func freeName(p, day string) string {
	dir, base := filepath.Split(p)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" {
		stem, ext = base, ""
	}
	for i := 1; ; i++ {
		n := fmt.Sprintf("%s (из копии %s)%s", stem, day, ext)
		if i > 1 {
			n = fmt.Sprintf("%s (из копии %s, %d)%s", stem, day, i, ext)
		}
		if _, err := os.Lstat(filepath.Join(dir, n)); errors.Is(err, os.ErrNotExist) {
			return filepath.Join(dir, n)
		}
	}
}

// fetch — скачать содержимое f в out: во временный файл рядом (с
// докачкой), проверить хеш, потом переименовать. Обрыв не оставляет
// половины файла под настоящим именем.
func fetch(ctx context.Context, rm *Remote, f File, out string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	part := filepath.Join(filepath.Dir(out), ".~backup-"+f.Hash[:16]+".part")
	for try := 0; try < 3; try++ {
		pf, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		st, _ := pf.Stat()
		off := st.Size()
		if off > f.Size {
			pf.Truncate(0)
			off = 0
		}
		if off < f.Size {
			body, err := rm.Blob(ctx, f.Hash, off)
			if err != nil && off > 0 {
				pf.Truncate(0)
				off = 0
				body, err = rm.Blob(ctx, f.Hash, 0)
			}
			if err != nil {
				pf.Close()
				return err
			}
			pf.Seek(off, io.SeekStart)
			_, err = io.Copy(pf, io.LimitReader(body, f.Size-off))
			body.Close()
			if err == nil {
				err = pf.Sync()
			}
			if err != nil {
				pf.Close()
				if ctx.Err() != nil {
					return err
				}
				continue
			}
		}
		pf.Close()
		h, err := hashOf(part)
		if err != nil {
			return err
		}
		if h != f.Hash {
			os.Remove(part)
			continue
		}
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o644
		}
		os.Chmod(part, mode)
		if f.MTime > 0 {
			t := time.Unix(0, f.MTime)
			os.Chtimes(part, t, t)
		}
		return os.Rename(part, out)
	}
	return errors.New("скачанное не совпало с копией")
}
