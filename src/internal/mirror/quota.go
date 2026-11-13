package mirror

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Квота сервера папок (ТЗ 13). На устройстве её нет (Room == nil); на
// сервере плагинов (internal/node) Room говорит, сколько ещё можно записать
// в папку, а OnUsage узнаёт о каждом изменении занятого места сразу — не
// «до следующего пересчёта».
//
// Занятое место папки (Usage) — по индексу: живые файлы + версии и корзина
// (каждый blob один раз) + недогруженные куски. Индекс ведётся под замком
// папки, поэтому счёт точный в момент записи.

// QuotaError — место кончилось; Why — понятный человеку текст.
type QuotaError struct{ Why string }

func (e *QuotaError) Error() string { return e.Why }

// IsQuota — ошибка «нет места».
func IsQuota(err error) bool {
	var q *QuotaError
	return errors.As(err, &q)
}

// usageLocked — занятое место папки; f.mu взят.
func (f *hfolder) usageLocked() int64 {
	var n int64
	for p, fi := range f.ix.Files {
		if !fi.Deleted && !isDir(p) {
			n += fi.Size
		}
	}
	seen := map[string]bool{}
	for _, v := range f.ix.Versions {
		if !seen[v.Blob] {
			seen[v.Blob] = true
			n += v.Size
		}
	}
	if es, err := os.ReadDir(filepath.Join(f.dir, "incoming")); err == nil {
		for _, e := range es {
			if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
				n += fi.Size()
			}
		}
	}
	return n
}

// Usage — сколько байт занимает папка id на сервере.
func (h *Hub) Usage(id string) (int64, error) {
	f, err := h.folder(id)
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usageLocked(), nil
}

// roomLocked — сколько ещё можно записать в папку; -1 — без предела.
func (h *Hub) roomLocked(f *hfolder) (int64, string) {
	if h.Room == nil {
		return -1, ""
	}
	left, why := h.Room(f.ID, f.usageLocked())
	if left < 0 {
		left = 0
	}
	return left, why
}

// need — проверка «влезет ли ещё n байт»; f.mu взят.
func (h *Hub) needLocked(f *hfolder, n int64) error {
	if n <= 0 {
		return nil
	}
	left, why := h.roomLocked(f)
	if left >= 0 && n > left {
		return &QuotaError{why}
	}
	return nil
}

// tracked — выполнить изменение папки и сообщить, на сколько изменилось
// занятое место. f.mu взят.
func (h *Hub) trackLocked(f *hfolder, fn func() error) error {
	if h.OnUsage == nil {
		return fn()
	}
	before := f.usageLocked()
	err := fn()
	if d := f.usageLocked() - before; d != 0 {
		h.OnUsage(f.ID, d)
	}
	return err
}

// quotaReader пропускает не больше left байт; дальше — QuotaError (то, что
// уже записано, остаётся недогруженным куском вне папки).
type quotaReader struct {
	r    io.Reader
	left int64
	why  string
}

func (q *quotaReader) Read(p []byte) (int, error) {
	if q.left <= 0 {
		var one [1]byte
		if n, _ := q.r.Read(one[:]); n == 0 {
			return 0, io.EOF
		}
		return 0, &QuotaError{q.why}
	}
	if int64(len(p)) > q.left {
		p = p[:q.left]
	}
	n, err := q.r.Read(p)
	q.left -= int64(n)
	return n, err
}
