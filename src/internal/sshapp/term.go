package sshapp

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Встроенный терминал (телефон; ПК, где внешнего нет): SSH-клиент в ядре,
// страница с xterm.js. Вывод — потоком SSE (как /events), ввод — POST:
// без WebSocket, одинаково в WebView2, Android WebView и браузере.

// termIdle — сколько живёт сессия без читателя (страницу закрыли или
// WebView усыпили): потом закрывается сама.
const termIdle = 2 * time.Minute

// backlogMax — сколько вывода копить, пока страницы нет.
const backlogMax = 1 << 20

type termSession struct {
	id    string
	cl    *ssh.Client
	sess  *ssh.Session
	stdin io.WriteCloser

	mu      sync.Mutex
	backlog []byte
	wake    chan struct{} // есть новый вывод
	ended   bool
	endErr  string
	readers int
	idleAt  time.Time
}

var (
	termMu   sync.Mutex
	sessions = map[string]*termSession{}
)

// StartTerm входит на сервер ключом программы и открывает оболочку.
func StartTerm(s Server, cols, rows int) (*termSession, hostInfo, error) {
	auth, err := keyAuth()
	if err != nil {
		return nil, hostInfo{}, err
	}
	cl, info, err := dial(s, auth)
	if err != nil {
		return nil, info, err
	}
	sess, err := cl.NewSession()
	if err != nil {
		cl.Close()
		return nil, info, err
	}
	if cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		cl.Close()
		return nil, info, err
	}
	stdin, _ := sess.StdinPipe()
	b := make([]byte, 9)
	rand.Read(b)
	t := &termSession{id: hex.EncodeToString(b), cl: cl, sess: sess, stdin: stdin,
		wake: make(chan struct{}, 1), idleAt: time.Now()}
	w := termWriter{t}
	sess.Stdout, sess.Stderr = w, w
	if err := sess.Shell(); err != nil {
		cl.Close()
		return nil, info, err
	}
	termMu.Lock()
	sessions[t.id] = t
	termMu.Unlock()
	go func() {
		err := sess.Wait()
		t.finish(err)
	}()
	go t.reaper()
	return t, info, nil
}

type termWriter struct{ t *termSession }

func (w termWriter) Write(p []byte) (int, error) {
	t := w.t
	t.mu.Lock()
	t.backlog = append(t.backlog, p...)
	if len(t.backlog) > backlogMax {
		t.backlog = t.backlog[len(t.backlog)-backlogMax:]
	}
	t.mu.Unlock()
	t.notify()
	return len(p), nil
}

func (t *termSession) notify() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *termSession) finish(err error) {
	t.mu.Lock()
	if !t.ended {
		t.ended = true
		if err != nil {
			var ee *ssh.ExitError
			if !errors.As(err, &ee) {
				t.endErr = err.Error()
			}
		}
	}
	t.mu.Unlock()
	t.cl.Close()
	t.notify()
}

func (t *termSession) reaper() {
	for range time.Tick(15 * time.Second) {
		t.mu.Lock()
		ended := t.ended
		idle := t.readers == 0 && time.Since(t.idleAt) > termIdle
		t.mu.Unlock()
		if idle {
			t.finish(nil)
		}
		if ended || idle {
			time.Sleep(time.Minute) // дать странице дочитать конец
			termMu.Lock()
			delete(sessions, t.id)
			termMu.Unlock()
			return
		}
	}
}

func getTerm(id string) *termSession {
	termMu.Lock()
	defer termMu.Unlock()
	return sessions[id]
}

// take забирает накопленный вывод.
func (t *termSession) take() (data []byte, ended bool, errText string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	data, t.backlog = t.backlog, nil
	return data, t.ended, t.endErr
}

func (t *termSession) input(b64 string) error {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return err
	}
	_, err = t.stdin.Write(b)
	return err
}

func (t *termSession) resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return t.sess.WindowChange(rows, cols)
}
