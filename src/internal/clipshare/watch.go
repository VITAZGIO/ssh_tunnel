package clipshare

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/devnet"
)

// Clip — содержимое буфера этого устройства.
type Clip struct {
	Kind string
	Name string // имя файла
	Mime string
	Data []byte
	// Path — файл с содержимым на диске (для Write вида file: буфер
	// системы держит путь, а не байты).
	Path string
}

// Board — буфер обмена пользователя.
type Board interface {
	// Token меняется при каждой смене содержимого (на Windows — номер
	// изменения буфера, иначе — отпечаток).
	Token() string
	// Read — что сейчас в буфере. ok == false — отправлять нечего (пусто,
	// папка, несколько файлов). Больше MaxBytes — Data пустое, Name и Kind
	// заполнены: предупредим человека.
	Read() (c Clip, ok bool)
	Write(c Clip) error
}

// headOnly — «after» для вопроса «какой сейчас последний номер», без ожидания.
const headOnly int64 = 1 << 62

// link — как наблюдатель говорит с ядром: в том же процессе или через API
// службы (VPN-версия Windows).
type link interface {
	active() bool
	push(c Clip) error
	// next ждёт записей новее after (до ~25 с).
	next(after int64) ([]Item, int64, error)
	// data — содержимое записи.
	data(it Item) ([]byte, error)
}

// tempDir — где лежат полученные файлы, пока они в буфере (путь в буфере
// должен жить, пока его могут вставить).
func tempDir() string { return filepath.Join(os.TempDir(), "ssh_tunnel-clip") }

// cleanTemp — оставить последние keepItems полученных файлов.
func cleanTemp() {
	ents, err := os.ReadDir(tempDir())
	if err != nil || len(ents) <= keepItems {
		return
	}
	type de struct {
		name string
		t    time.Time
	}
	var all []de
	for _, e := range ents {
		if fi, err := e.Info(); err == nil {
			all = append(all, de{e.Name(), fi.ModTime()})
		}
	}
	for len(all) > keepItems {
		old := 0
		for i := range all {
			if all[i].t.Before(all[old].t) {
				old = i
			}
		}
		os.RemoveAll(filepath.Join(tempDir(), all[old].name))
		all = append(all[:old], all[old+1:]...)
	}
}

// safeName — только имя файла, без путей.
func safeName(n string) string {
	n = filepath.Base(strings.ReplaceAll(n, "\\", "/"))
	if n == "." || n == "/" || n == "" || n == ".." {
		return "file"
	}
	return n
}

var (
	boardMu   sync.Mutex
	boardSeen bool
)

// boardHere — следит ли это устройство за своим буфером (для страницы).
func boardHere() bool {
	boardMu.Lock()
	defer boardMu.Unlock()
	return boardSeen
}

// watch — наблюдатель: своё — всем, полученное — в свой буфер. Возвращает
// «остановить».
func watch(b Board, l link) func() {
	stop := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	lastApplied, lastPushed := "", ""
	boardMu.Lock()
	boardSeen = true
	boardMu.Unlock()

	// Своё — всем.
	go func() {
		tok := b.Token()
		t := time.NewTicker(700 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			nt := b.Token()
			if nt == tok {
				continue
			}
			tok = nt
			c, ok := b.Read()
			if !ok || !l.active() {
				continue
			}
			if len(c.Data) == 0 && c.Name != "" {
				devnet.Warn(fmt.Sprintf("Общий буфер: «%s» больше %d МБ — передай через «Файлы устройства».", c.Name, MaxBytes>>20))
				continue
			}
			h := hashOf(c.Kind, c.Data)
			mu.Lock()
			skip := h == lastApplied || h == lastPushed
			if !skip {
				lastPushed = h
			}
			mu.Unlock()
			if skip {
				continue
			}
			_ = l.push(c)
		}
	}()

	// Полученное — в свой буфер. Сначала узнаём, где история сейчас: старое
	// не кладём.
	go func() {
		var seq int64 = -1
		for {
			select {
			case <-stop:
				return
			default:
			}
			if seq < 0 {
				if !l.active() {
					sleep(stop, 5*time.Second)
					continue
				}
				_, s, err := l.next(headOnly)
				if err != nil {
					sleep(stop, 5*time.Second)
					continue
				}
				seq = s
				continue
			}
			items, s, err := l.next(seq)
			if err != nil {
				seq = -1
				sleep(stop, 3*time.Second)
				continue
			}
			seq = s
			// Новые первыми — в буфер нужна только последняя полученная.
			for _, it := range items {
				if it.Out || it.Gone {
					continue
				}
				data, err := l.data(it)
				if err != nil {
					break
				}
				c := Clip{Kind: it.Kind, Name: it.Name, Mime: it.Mime, Data: data}
				if it.Kind == KindFile {
					dir := filepath.Join(tempDir(), it.ID)
					_ = os.MkdirAll(dir, 0o700)
					c.Path = filepath.Join(dir, safeName(it.Name))
					if os.WriteFile(c.Path, data, 0o600) != nil {
						break
					}
					cleanTemp()
				}
				mu.Lock()
				lastApplied = hashOf(c.Kind, data)
				mu.Unlock()
				_ = b.Write(c)
				break
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(stop)
			boardMu.Lock()
			boardSeen = false
			boardMu.Unlock()
		})
	}
}

func sleep(stop chan struct{}, d time.Duration) {
	select {
	case <-stop:
	case <-time.After(d):
	}
}

// localLink — ядро в этом же процессе.
type localLink struct{}

func (localLink) active() bool { return current() != nil }

func (localLink) push(c Clip) error {
	n := current()
	if n == nil {
		return errOff
	}
	_, err := n.push(Item{Kind: c.Kind, Name: c.Name, Mime: c.Mime}, c.Data)
	return err
}

func (localLink) next(after int64) ([]Item, int64, error) {
	n := current()
	if n == nil {
		return nil, 0, errOff
	}
	items, seq, wake := n.h.list(after)
	if len(items) == 0 && after < headOnly {
		select {
		case <-wake:
			items, seq, _ = n.h.list(after)
		case <-time.After(25 * time.Second):
		}
	}
	return items, seq, nil
}

func (localLink) data(it Item) ([]byte, error) {
	n := current()
	if n == nil {
		return nil, errOff
	}
	return os.ReadFile(n.h.path(it.ID))
}

// WatchRemote — наблюдатель в окне VPN-версии Windows: ядро в службе,
// буфер — у окна. ep отдаёт текущие адрес («127.0.0.1:порт») и ключ службы.
// Работает, пока жив процесс; приложение выключено — ждёт.
func WatchRemote(ep func() (addr, token string)) {
	b := localBoard()
	if b == nil {
		return
	}
	watch(b, &remoteLink{ep: ep, hc: &http.Client{Timeout: 2 * time.Minute}})
}

type remoteLink struct {
	ep func() (string, string)
	hc *http.Client
}

func (r *remoteLink) do(method, path string, body io.Reader) (*http.Response, error) {
	addr, tok := r.ep()
	if addr == "" {
		return nil, errors.New("служба не отвечает")
	}
	req, err := http.NewRequest(method, "http://"+addr+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Token", tok)
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("служба ответила %s", resp.Status)
	}
	return resp, nil
}

func (r *remoteLink) active() bool {
	resp, err := r.do(http.MethodGet, fmt.Sprintf("/api/clipboard?after=%d", headOnly), nil)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v struct {
		Error string `json:"error"`
	}
	return json.NewDecoder(resp.Body).Decode(&v) == nil && v.Error == ""
}

func (r *remoteLink) push(c Clip) error {
	q := url.Values{"kind": {c.Kind}, "name": {c.Name}, "mime": {c.Mime}}
	resp, err := r.do(http.MethodPost, "/api/clipboard/push?"+q.Encode(), bytesReader(c.Data))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (r *remoteLink) next(after int64) ([]Item, int64, error) {
	path := fmt.Sprintf("/api/clipboard?after=%d", after)
	if after < headOnly {
		path += "&wait=1"
	}
	resp, err := r.do(http.MethodGet, path, nil)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var v struct {
		Items []Item `json:"items"`
		Seq   int64  `json:"seq"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, 0, err
	}
	if v.Error != "" {
		return nil, 0, errors.New(v.Error)
	}
	return v.Items, v.Seq, nil
}

func (r *remoteLink) data(it Item) ([]byte, error) {
	resp, err := r.do(http.MethodGet, "/api/clipboard/data?id="+url.QueryEscape(it.ID), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
}
