package mirror

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Файловые мелочи: хеш, атомарная запись, обход папки. Всё, что кладётся на
// место файла, сначала пишется во временный файл рядом и только потом
// переименовывается — оборванная запись не оставляет полфайла.

// tmpPrefix — наши временные файлы в папке (докачка, подмена). Их никто не
// синхронизирует (ignore.go), и сверка их не видит.
const tmpPrefix = ".~mirror-"

// hashFile — sha256 файла и его размер.
func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// cleanRel — путь внутри папки вида «a/b.md» или ok=false, если путь
// пустой, абсолютный, с «..» или недопустимыми символами. Пути в протоколе
// всегда через «/».
func cleanRel(p string) (string, bool) {
	p = strings.ReplaceAll(p, `\`, "/")
	dir := strings.HasSuffix(p, "/") && len(p) > 1
	if dir {
		p = strings.TrimSuffix(p, "/")
	}
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) || strings.Contains(p, ":") {
		return "", false
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." || len(s) > 255 {
			return "", false
		}
	}
	if dir {
		return path.Clean(p) + "/", true
	}
	return path.Clean(p), true
}

// DirHash — «хеш» записи папки. Папки синхронизируются записями «путь/»,
// иначе пустая папка (новая в Obsidian) не доезжала, пока в ней нет файла,
// а после удаления всех файлов пустая оставалась.
var DirHash = strings.Repeat("d", 64)

// isDir — запись папки («a/b/»).
func isDir(rel string) bool { return strings.HasSuffix(rel, "/") }

// full — настоящий путь rel внутри root.
func full(root, rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }

// writeJSONAtomic — JSON во временный файл, на диск, переименование.
func writeJSONAtomic(p string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// copyFile копирует src в dst через временный файл (с fsync) и
// возвращает sha256 скопированного.
func copyFile(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	tmp := dst + ".tmp" + randHex(3)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// stat — что сверка знает о файле, не читая его.
type stat struct {
	Size  int64
	MTime int64 // UnixNano; сравнивается только на равенство — часы не важны
}

func statOf(fi fs.FileInfo) stat { return stat{fi.Size(), fi.ModTime().UnixNano()} }

// scanDir — обычные файлы папки (без ссылок и исключённого).
func scanDir(root string, ig *Ignore) (map[string]stat, error) {
	out := map[string]stat{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			// Недоступный кусок папки пропускаем: файлы в нём не трогаются,
			// и удалением это не считается (см. Replica.reconcile).
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if _, ok := cleanRel(rel); !ok {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil // имя, которое не передать (например, с «:»)
		}
		if ig.Match(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			out[rel+"/"] = stat{} // запись папки: время не важно
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // ссылки и устройства — нет
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		out[rel] = statOf(fi)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// errBusy — файл поменяли прямо во время операции; повтор на следующем
// проходе.
var errBusy = errors.New("файл меняется, повтор позже")

// fresh — файл изменён только что (или время в будущем): размеру и времени
// верить нельзя, читать заново. Сравниваются часы того же устройства, что
// ставило время файлу, так что сбитые часы тут не мешают.
func fresh(st stat) bool { return time.Since(time.Unix(0, st.MTime)) < 3*time.Second }
