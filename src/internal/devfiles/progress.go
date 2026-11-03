package devfiles

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Передачи файлов — для уведомления на телефоне («только во время передачи,
// потом убирать»). Ядро телефона ставит Progress; на ПК его нет.

// ProgressFunc получает сводку: сколько передач идёт, имя последней,
// сколько передано и сколько всего (0 — неизвестно). active == 0 — всё
// закончилось, уведомление убрать.
type ProgressFunc func(active int, name string, done, total int64)

var (
	progMu    sync.Mutex
	progress  ProgressFunc
	transfers = map[*transfer]struct{}{}
	lastSent  time.Time
)

// SetProgress — куда сообщать о передачах (nil — никуда).
func SetProgress(f ProgressFunc) {
	progMu.Lock()
	progress = f
	progMu.Unlock()
}

type transfer struct {
	name  string
	total int64
	done  atomic.Int64
	once  sync.Once
}

func track(name string, total int64) *transfer {
	t := &transfer{name: name, total: total}
	progMu.Lock()
	transfers[t] = struct{}{}
	progMu.Unlock()
	report(true)
	return t
}

func (t *transfer) add(n int64) {
	t.done.Add(n)
	report(false)
}

func (t *transfer) end() {
	t.once.Do(func() {
		progMu.Lock()
		delete(transfers, t)
		progMu.Unlock()
		report(true)
	})
}

// report — не чаще раза в секунду, кроме начала и конца.
func report(force bool) {
	progMu.Lock()
	f := progress
	if f == nil || (!force && time.Since(lastSent) < time.Second) {
		progMu.Unlock()
		return
	}
	lastSent = time.Now()
	var name string
	var done, total int64
	for t := range transfers {
		name = t.name
		done += t.done.Load()
		total += t.total
	}
	n := len(transfers)
	progMu.Unlock()
	f(n, name, done, total)
}

type progressReader struct {
	r  io.Reader
	tr *transfer
}

func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.tr.add(int64(n))
	}
	return n, err
}

// progressFile — то же для http.ServeContent (нужен Seek).
type progressFile struct {
	*os.File
	tr *transfer
}

func (p progressFile) Read(b []byte) (int, error) {
	n, err := p.File.Read(b)
	if n > 0 {
		p.tr.add(int64(n))
	}
	return n, err
}
