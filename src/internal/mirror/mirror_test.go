package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Тесты ТЗ 6: в каждом сценарии проверяется главное — ни одно записанное
// содержимое не пропало (есть в папке сервера, у клиентов или в версиях
// сервера), а потом все сошлись к одному.

type env struct {
	t       *testing.T
	hub     *Hub
	hubDir  string
	root    string
	fid     string
	srv     *httptest.Server
	clients []*Replica
	trs     map[*Replica]*faultTr

	mu      sync.Mutex
	written map[string]string // хеш → что это было
	n       int
}

// faultTr — транспорт клиента: имя устройства и поломки.
type faultTr struct {
	dev string
	mu  sync.Mutex
	// cutUp/cutDown — оборвать тело загрузки/скачивания после N байт (один раз).
	cutUp, cutDown int64
	// dropCommit — сервер выполнил /commit, а ответ потерялся (один раз).
	dropCommit bool
	down       bool // сервер недоступен
	puts       int64
}

var errCut = errors.New("обрыв (тест)")

type cutReader struct {
	r    io.ReadCloser
	left int64
}

func (c *cutReader) Read(b []byte) (int, error) {
	if c.left <= 0 {
		return 0, errCut
	}
	if int64(len(b)) > c.left {
		b = b[:c.left]
	}
	n, err := c.r.Read(b)
	c.left -= int64(n)
	return n, err
}
func (c *cutReader) Close() error { return c.r.Close() }

func (f *faultTr) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	if f.down {
		f.mu.Unlock()
		return nil, &net.OpError{Op: "dial", Err: errors.New("сервер недоступен (тест)")}
	}
	req = req.Clone(req.Context())
	req.Header.Set("X-Dev", f.dev)
	if req.URL.Path == "/put" {
		atomic.AddInt64(&f.puts, 1)
		if f.cutUp > 0 && req.Body != nil {
			req.Body = &cutReader{req.Body, f.cutUp}
			req.ContentLength = -1
			f.cutUp = 0
		}
	}
	cutDown := int64(0)
	if req.URL.Path == "/get" && f.cutDown > 0 {
		cutDown, f.cutDown = f.cutDown, 0
	}
	drop := false
	if req.URL.Path == "/commit" && f.dropCommit {
		drop, f.dropCommit = true, false
	}
	f.mu.Unlock()
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if drop {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, errors.New("ответ потерян (тест)")
	}
	if cutDown > 0 {
		resp.Body = &cutReader{resp.Body, cutDown}
	}
	return resp, nil
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, hubDir: t.TempDir(), root: t.TempDir(), written: map[string]string{}, trs: map[*Replica]*faultTr{}}
	e.startHub()
	f, err := e.hub.AddFolder(e.root, "Тест")
	if err != nil {
		t.Fatal(err)
	}
	e.fid = f.ID
	t.Cleanup(func() {
		e.srv.Close()
		e.hub.Close()
	})
	return e
}

func (e *env) startHub() {
	h, err := OpenHub(e.hubDir, "server")
	if err != nil {
		e.t.Fatal(err)
	}
	e.hub = h
	e.srv = httptest.NewServer(h.Handler(func(r *http.Request) string { return r.Header.Get("X-Dev") }))
	for _, c := range e.clients {
		c.R.Base = e.srv.URL
	}
}

func (e *env) client(name, mode string) *Replica {
	tr := &faultTr{dev: name}
	c := &Replica{Folder: e.fid, Path: e.t.TempDir(), Mode: mode, Device: name,
		State: filepath.Join(e.t.TempDir(), "state.json"),
		R:     &Remote{Base: e.srv.URL, HC: &http.Client{Transport: tr}}}
	e.clients = append(e.clients, c)
	e.trs[c] = tr
	return c
}

func sum(b string) string {
	h := sha256.Sum256([]byte(b))
	return hex.EncodeToString(h[:])
}

// serverHas — знает ли сервер такое содержимое (файл или версия).
func (e *env) serverHas(h string) bool {
	f, err := e.hub.folder(e.fid)
	if err != nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fi := range f.ix.Files {
		if !fi.Deleted && fi.Hash == h {
			return true
		}
	}
	for _, v := range f.ix.Versions {
		if v.Hash == h {
			return true
		}
	}
	return false
}

// forget — человек сам затирает файл: если его содержимое ещё не дошло до
// сервера (или это правка прямо в папке сервера), его потеря — не наша.
func (e *env) forget(dir, rel string) {
	b, err := os.ReadFile(full(dir, rel))
	if err != nil {
		return
	}
	h := sum(string(b))
	if dir == e.root || !e.serverHas(h) {
		e.mu.Lock()
		delete(e.written, h)
		e.mu.Unlock()
	}
}

func (e *env) remove(dir, rel string) {
	e.forget(dir, rel)
	os.Remove(full(dir, rel))
}

// write пишет в папку dir уникальное содержимое и запоминает его.
func (e *env) write(dir, rel, what string) string {
	e.forget(dir, rel)
	e.mu.Lock()
	e.n++
	content := fmt.Sprintf("%s #%d %s", rel, e.n, what)
	e.written[sum(content)] = dir + ":" + rel + " " + what
	e.mu.Unlock()
	p := full(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return content
}

// sync — проход клиента. Перед ним — сверка сервера с диском (в программе
// её делает модуль по таймеру): так сервер видит правки прямо в своей папке.
func (e *env) sync(c *Replica) error {
	if err := e.hub.Scan(e.fid); err != nil {
		e.t.Fatal(err)
	}
	_, err := c.Sync(context.Background(), 0, false)
	return err
}

// settle гоняет всех клиентов, пока никому нечего делать.
func (e *env) settle() {
	e.t.Helper()
	for round := 0; round < 30; round++ {
		did := false
		if err := e.hub.Scan(e.fid); err != nil {
			e.t.Fatal(err)
		}
		for _, c := range e.clients {
			d, err := c.Sync(context.Background(), 0, false)
			if err != nil {
				e.t.Logf("%s: %v", c.Device, err)
			}
			did = did || d
		}
		if !did {
			return
		}
	}
	e.t.Fatal("клиенты не сошлись за 30 кругов")
}

// tree — файлы папки: путь → содержимое.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	disk, err := scanDir(root, NewIgnore(nil))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for p := range disk {
		b, err := os.ReadFile(full(root, p))
		if err != nil {
			t.Fatal(err)
		}
		out[p] = string(b)
	}
	return out
}

// noLoss — каждое записанное содержимое есть хоть где-то: в папке сервера,
// у клиента или в версиях (корзине) сервера.
func (e *env) noLoss() {
	e.t.Helper()
	have := map[string]bool{}
	add := func(root string) {
		for _, c := range tree(e.t, root) {
			have[sum(c)] = true
		}
	}
	add(e.root)
	for _, c := range e.clients {
		add(c.Path)
	}
	vs, _ := os.ReadDir(filepath.Join(e.hubDir, e.fid, "versions"))
	for _, v := range vs {
		b, err := os.ReadFile(filepath.Join(e.hubDir, e.fid, "versions", v.Name()))
		if err == nil {
			have[sum(string(b))] = true
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for h, what := range e.written {
		if !have[h] {
			e.t.Errorf("ПОТЕРЯНО: %s", what)
		}
	}
}

// converged — у клиентов «отправлять и получать» то же, что на сервере.
func (e *env) converged() {
	e.t.Helper()
	want := tree(e.t, e.root)
	for _, c := range e.clients {
		if c.Mode != ModeBoth {
			continue
		}
		got := tree(e.t, c.Path)
		if !sameTree(want, got) {
			e.t.Errorf("%s не сошёлся с сервером:\nсервер: %v\nклиент: %v", c.Device, keys(want), keys(got))
		}
	}
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func keys(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func conflictsIn(m map[string]string) []string {
	var out []string
	for k := range m {
		if strings.Contains(k, "(конфликт,") {
			out = append(out, k)
		}
	}
	return out
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(full(root, rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return string(b)
}

func exists(root, rel string) bool {
	_, err := os.Lstat(full(root, rel))
	return err == nil
}

func TestBasicSync(t *testing.T) {
	e := newEnv(t)
	a, b := e.client("A", ModeBoth), e.client("B", ModeBoth)
	e.write(e.root, "старое.md", "было на сервере")
	e.write(a.Path, "заметки/день.md", "от A")
	os.WriteFile(full(a.Path, "пустой.txt"), nil, 0o644)
	e.written[sum("")] = "пустой файл"
	e.settle()
	e.converged()
	if !exists(b.Path, "заметки/день.md") || !exists(a.Path, "старое.md") {
		t.Fatal("файлы не дошли")
	}
	v := e.write(b.Path, "заметки/день.md", "правка B")
	os.Remove(full(a.Path, "старое.md"))
	e.settle()
	e.converged()
	if read(t, a.Path, "заметки/день.md") != v {
		t.Fatal("правка не дошла")
	}
	if exists(b.Path, "старое.md") || exists(e.root, "старое.md") {
		t.Fatal("удаление не дошло")
	}
	e.noLoss()
	if st := a.Status(); st.State != "ok" {
		t.Fatalf("статус %+v", st)
	}
}

// Одновременная правка на двух клиентах: обе версии остаются.
func TestConcurrentEdit(t *testing.T) {
	e := newEnv(t)
	a, b := e.client("A", ModeBoth), e.client("B", ModeBoth)
	e.write(e.root, "note.md", "v0")
	e.settle()
	va := e.write(a.Path, "note.md", "правка A")
	vb := e.write(b.Path, "note.md", "правка B")
	e.settle()
	e.converged()
	e.noLoss()
	srv := tree(t, e.root)
	cs := conflictsIn(srv)
	if len(cs) != 1 {
		t.Fatalf("ждали одну конфликт-копию, есть %v", keys(srv))
	}
	if !strings.Contains(cs[0], "B") || srv["note.md"] != va || srv[cs[0]] != vb {
		t.Fatalf("не те версии: %v", srv)
	}
	if st := a.Status(); st.State != "conflict" || len(st.Conflicts) != 1 {
		t.Fatalf("статус %+v", st)
	}
	// «Оставить ту»: копия становится основным файлом, прежнее — в версиях.
	if err := a.Resolve(context.Background(), cs[0], "copy"); err != nil {
		t.Fatal(err)
	}
	e.settle()
	e.converged()
	if read(t, a.Path, "note.md") != vb || len(conflictsIn(tree(t, e.root))) != 0 {
		t.Fatal("конфликт не решился")
	}
	if st := b.Status(); st.State != "ok" {
		t.Fatalf("статус после решения %+v", st)
	}
	e.noLoss()
}

// Удаление против правки — правка важнее, в каком бы порядке ни пришли.
func TestDeleteVsEdit(t *testing.T) {
	for _, delFirst := range []bool{true, false} {
		t.Run(fmt.Sprint("delFirst=", delFirst), func(t *testing.T) {
			e := newEnv(t)
			a, b := e.client("A", ModeBoth), e.client("B", ModeBoth)
			e.write(e.root, "x.md", "v0")
			e.settle()
			os.Remove(full(a.Path, "x.md"))
			v := e.write(b.Path, "x.md", "правка B")
			if delFirst {
				e.sync(a)
				e.sync(b)
			} else {
				e.sync(b)
				e.sync(a)
			}
			e.settle()
			e.converged()
			e.noLoss()
			if read(t, a.Path, "x.md") != v {
				t.Fatal("правка потерялась")
			}
		})
	}
}

// Переименование: доходит, повторной передачи нет; правка старого имени
// на другом клиенте одновременно — не теряется.
func TestRename(t *testing.T) {
	e := newEnv(t)
	a, b := e.client("A", ModeBoth), e.client("B", ModeBoth)
	big := strings.Repeat("данные ", 50000)
	os.WriteFile(full(a.Path, "a.bin"), []byte(big), 0o644)
	e.written[sum(big)] = "a.bin"
	e.settle()
	puts := atomic.LoadInt64(&e.trs[a].puts)
	if err := os.Rename(full(a.Path, "a.bin"), full(a.Path, "dir/b.bin")); err != nil {
		os.MkdirAll(full(a.Path, "dir"), 0o755)
		if err := os.Rename(full(a.Path, "a.bin"), full(a.Path, "dir/b.bin")); err != nil {
			t.Fatal(err)
		}
	}
	e.settle()
	e.converged()
	if atomic.LoadInt64(&e.trs[a].puts) != puts {
		t.Fatal("переименование передало файл заново")
	}
	if exists(b.Path, "a.bin") || read(t, b.Path, "dir/b.bin") != big {
		t.Fatal("переименование не дошло")
	}

	// A переименовывает, B в это же время правит старое имя.
	e.write(e.root, "n.md", "v0")
	e.settle()
	os.Rename(full(a.Path, "n.md"), full(a.Path, "m.md"))
	v := e.write(b.Path, "n.md", "правка B")
	e.sync(b)
	e.settle()
	e.converged()
	e.noLoss()
	if read(t, a.Path, "n.md") != v || !exists(a.Path, "m.md") {
		t.Fatalf("после переименования с правкой: %v", keys(tree(t, a.Path)))
	}
}

// Обрыв посреди загрузки и скачивания: недокачанное нигде не появляется,
// докачка продолжает с места обрыва.
func TestInterruptedTransfer(t *testing.T) {
	e := newEnv(t)
	a, b := e.client("A", ModeBoth), e.client("B", ModeBoth)
	big := strings.Repeat("0123456789", 1<<20) // 10 МБ: больше куска
	os.WriteFile(full(a.Path, "big.bin"), []byte(big), 0o644)
	e.written[sum(big)] = "big.bin"
	e.trs[a].cutUp = 3 << 20
	if err := e.sync(a); err == nil {
		t.Fatal("обрыв загрузки не заметили")
	}
	if exists(e.root, "big.bin") {
		t.Fatal("на сервере появилось недокачанное")
	}
	have, _ := e.hub.PartSize(e.fid, "A", "big.bin", sum(big))
	if have == 0 {
		t.Fatal("докачке нечего продолжать")
	}
	e.trs[b].cutDown = 4 << 20
	e.settle()
	if read(t, e.root, "big.bin") != big {
		t.Fatal("на сервере не то")
	}
	if read(t, b.Path, "big.bin") != big {
		t.Fatal("у B не то")
	}
	e.converged()
	e.noLoss()

	// Обрыв ответа после записи на сервере: повтор не плодит копий.
	v := e.write(a.Path, "big.bin", "новая версия")
	e.trs[a].dropCommit = true
	e.sync(a)
	e.settle()
	e.converged()
	if read(t, b.Path, "big.bin") != v || len(conflictsIn(tree(t, e.root))) != 0 {
		t.Fatal("повтор после потерянного ответа сломал файл")
	}
	e.noLoss()
}

// Сервер недоступен сутки: правки копятся, потом уходят; ничего не потеряно.
func TestServerDownForADay(t *testing.T) {
	e := newEnv(t)
	a, b := e.client("A", ModeBoth), e.client("B", ModeBoth)
	e.write(e.root, "общий.md", "v0")
	e.settle()
	now := time.Now()
	e.hub.now = func() time.Time { return now }
	for _, c := range e.clients {
		e.trs[c].down = true
	}
	va := e.write(a.Path, "a.md", "офлайн A")
	vb := e.write(b.Path, "общий.md", "офлайн B")
	e.write(a.Path, "общий.md", "офлайн A общий")
	for i := 0; i < 3; i++ {
		e.sync(a)
		e.sync(b)
	}
	if st := a.Status(); st.State != "offline" || st.Pending != 2 {
		t.Fatalf("статус без сервера %+v", st)
	}
	// Сутки спустя сервер перезапущен.
	now = now.Add(25 * time.Hour)
	e.srv.Close()
	e.hub.Close()
	e.startHub()
	e.hub.now = func() time.Time { return now }
	e.hub.Prune()
	for _, c := range e.clients {
		e.trs[c].down = false
	}
	e.settle()
	e.converged()
	e.noLoss()
	if read(t, b.Path, "a.md") != va {
		t.Fatal("офлайн-правка A не дошла")
	}
	srv := tree(t, e.root)
	if len(conflictsIn(srv)) != 1 {
		t.Fatalf("ждали конфликт по общему файлу: %v", keys(srv))
	}
	_ = vb
}

// Часы клиента сбиты: время файлов в прошлом и будущем, правка с тем же
// размером и тем же временем — ловит полная сверка.
func TestClockSkew(t *testing.T) {
	e := newEnv(t)
	a, b := e.client("A", ModeBoth), e.client("B", ModeBoth)
	a.now = func() time.Time { return time.Now().Add(-3 * 365 * 24 * time.Hour) }
	b.now = func() time.Time { return time.Now().Add(400 * 24 * time.Hour) }
	past := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	future := time.Now().Add(5 * 365 * 24 * time.Hour)
	e.write(a.Path, "p.md", "aaaa")
	os.Chtimes(full(a.Path, "p.md"), past, past)
	e.write(b.Path, "f.md", "bbbb")
	os.Chtimes(full(b.Path, "f.md"), future, future)
	e.settle()
	e.converged()

	// Правка того же размера, время вернули прежним.
	p := full(a.Path, "p.md")
	fi, _ := os.Stat(p)
	old := read(t, a.Path, "p.md")
	nv := strings.Replace(old, "aaaa", "AAAA", 1)
	os.WriteFile(p, []byte(nv), 0o644)
	e.written[sum(nv)] = "p.md правка"
	os.Chtimes(p, fi.ModTime(), fi.ModTime())
	e.settle()
	if read(t, b.Path, "p.md") == nv {
		t.Log("заметили и без полной сверки")
	}
	if _, err := a.Sync(context.Background(), 0, true); err != nil {
		t.Fatal(err)
	}
	e.settle()
	if read(t, b.Path, "p.md") != nv {
		t.Fatal("полная сверка не заметила правку")
	}
	// Конфликт со сбитыми часами — тоже обе версии.
	e.write(a.Path, "f.md", "A")
	e.write(b.Path, "f.md", "B")
	e.settle()
	e.converged()
	e.noLoss()
}

// Режимы: «только получать» не отправляет и не теряет свои правки;
// «только отправлять» не получает и не отправляет удаления.
func TestModes(t *testing.T) {
	e := newEnv(t)
	a := e.client("A", ModeBoth)
	r := e.client("R", ModeRecv)
	s := e.client("S", ModeSend)
	e.write(e.root, "общий.md", "v0")
	e.write(s.Path, "фото.jpg", "фото с телефона")
	e.settle()
	if !exists(r.Path, "общий.md") || exists(s.Path, "общий.md") || !exists(a.Path, "фото.jpg") {
		t.Fatal("режимы не сработали")
	}
	mine := e.write(r.Path, "общий.md", "локальная правка R")
	e.write(r.Path, "только-R.md", "R")
	e.settle()
	if exists(e.root, "только-R.md") || read(t, e.root, "общий.md") == mine {
		t.Fatal("«только получать» отправил")
	}
	v := e.write(a.Path, "общий.md", "правка A")
	e.settle()
	if read(t, r.Path, "общий.md") != v {
		t.Fatal("R не получил")
	}
	if len(conflictsIn(tree(t, r.Path))) != 1 {
		t.Fatalf("правка R должна остаться копией: %v", keys(tree(t, r.Path)))
	}
	os.Remove(full(s.Path, "фото.jpg"))
	os.Remove(full(r.Path, "общий.md"))
	e.settle()
	if !exists(e.root, "фото.jpg") {
		t.Fatal("удаление на телефоне удалило бэкап")
	}
	if read(t, r.Path, "общий.md") != v {
		t.Fatal("«только получать» не вернул удалённый у себя файл")
	}
	e.noLoss()
}

func TestIgnore(t *testing.T) {
	e := newEnv(t)
	a, b := e.client("A", ModeBoth), e.client("B", ModeBoth)
	for _, p := range []string{".obsidian/workspace.json", ".obsidian/workspace-mobile.json", "x.sync-conflict-20260101-1.md",
		".DS_Store", "Thumbs.db", "~$doc.docx", ".stversions/old.md", "a.md.swp"} {
		e.write(a.Path, p, "мусор")
	}
	e.write(a.Path, ".obsidian/app.json", "настройки")
	e.settle()
	got := keys(tree(t, b.Path))
	if len(got) != 1 || got[0] != ".obsidian/app.json" {
		t.Fatalf("исключения не сработали: %v", got)
	}
	// Свои исключения сервера.
	if err := e.hub.SetIgnore(e.fid, []string{"*.log"}); err != nil {
		t.Fatal(err)
	}
	e.write(a.Path, "x.log", "лог")
	e.settle()
	if exists(b.Path, "x.log") {
		t.Fatal("своё исключение не сработало")
	}
}

// Версии и корзина: 30 дней, потом чистятся; восстановление возвращает.
func TestVersionsAndRetention(t *testing.T) {
	e := newEnv(t)
	a := e.client("A", ModeBoth)
	now := time.Now()
	e.hub.now = func() time.Time { return now }
	v1 := e.write(a.Path, "d.md", "v1")
	e.settle()
	e.write(a.Path, "d.md", "v2")
	e.settle()
	os.Remove(full(a.Path, "d.md"))
	e.settle()
	vs, _ := e.hub.Versions(e.fid, "d.md")
	if len(vs) != 2 || !vs[0].Deleted {
		t.Fatalf("версии: %+v", vs)
	}
	now = now.Add(29 * 24 * time.Hour)
	e.hub.Prune()
	if vs, _ := e.hub.Versions(e.fid, ""); len(vs) != 2 {
		t.Fatal("версии убраны раньше 30 дней")
	}
	// Вернуть первую версию из корзины.
	if _, err := e.hub.Restore(e.fid, "server", vs[1].Blob); err != nil {
		t.Fatal(err)
	}
	e.settle()
	if read(t, a.Path, "d.md") != v1 {
		t.Fatal("восстановление не дошло")
	}
	now = now.Add(2 * 24 * time.Hour)
	e.hub.Prune()
	if vs, _ := e.hub.Versions(e.fid, ""); len(vs) != 0 {
		t.Fatalf("старше 30 дней не убрано: %+v", vs)
	}
}

// Правка прямо на сервере (мимо программы) не затирается присланным.
func TestServerLocalEdit(t *testing.T) {
	e := newEnv(t)
	a := e.client("A", ModeBoth)
	e.write(e.root, "s.md", "v0")
	e.settle()
	e.write(e.root, "s.md", "правка на сервере")
	e.write(a.Path, "s.md", "правка A")
	e.settle()
	e.converged()
	e.noLoss()
	if len(conflictsIn(tree(t, e.root))) != 1 {
		t.Fatal("ждали конфликт")
	}
}

// Сервер потерял данные (новый индекс): клиенты сверяются по хешам и
// ничего не теряют.
func TestServerReset(t *testing.T) {
	e := newEnv(t)
	a, b := e.client("A", ModeBoth), e.client("B", ModeBoth)
	e.write(a.Path, "1.md", "один")
	e.write(a.Path, "2.md", "два")
	e.settle()
	e.write(b.Path, "2.md", "правка B без сервера")
	e.srv.Close()
	e.hub.Close()
	os.RemoveAll(e.hubDir)
	e.hubDir = t.TempDir()
	e.startHub()
	f, err := e.hub.AddFolder(e.root, "Тест")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range e.clients {
		c.Folder = f.ID
	}
	e.fid = f.ID
	e.settle()
	e.converged()
	e.noLoss()
}

// Оборванная замена (остался отодвинутый файл) разбирается без потерь.
func TestCrashMidReplace(t *testing.T) {
	e := newEnv(t)
	a := e.client("A", ModeBoth)
	e.write(e.root, "c.md", "v0")
	e.settle()
	// Как будто программа упала между «отодвинуть» и «поставить новое».
	os.Rename(full(a.Path, "c.md"), oldName(full(a.Path, "c.md")))
	e.settle()
	if !exists(a.Path, "c.md") || !exists(e.root, "c.md") {
		t.Fatal("отодвинутый файл не вернулся, а удаление ушло на сервер")
	}
	// Отодвинутый файл изменён (правка в момент замены) — станет копией.
	e.write(e.root, "c.md", "v1 с сервера")
	edited := "правка в момент замены"
	os.WriteFile(oldName(full(a.Path, "c.md")), []byte(edited), 0o644)
	e.written[sum(edited)] = "правка в момент замены"
	e.settle()
	e.converged()
	e.noLoss()
}

func TestBadPaths(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"../x", "/etc/passwd", "a/../../x", `..\x`, "C:/x", ""} {
		if _, err := e.hub.Commit(e.fid, "A", p, sum("x"), 0); err == nil {
			t.Errorf("путь %q принят", p)
		}
		if _, err := e.hub.WritePart(e.fid, "A", p, sum("x"), 0, strings.NewReader("x")); err == nil {
			t.Errorf("загрузка в %q принята", p)
		}
	}
	if _, err := e.hub.AddFolder(filepath.Join(e.root, "sub"), "x"); err == nil {
		t.Error("вложенную папку не должно быть можно раздать")
	}
}

// Длительный тест: тысячи файлов, случайные правки, удаления,
// переименования с трёх клиентов, обрывы связи.
func TestLongRandom(t *testing.T) {
	// По умолчанию 2000 файлов (CI гоняет с -race); MIRROR_LONG=1 — 5000.
	files, rounds := 2000, 12
	if os.Getenv("MIRROR_LONG") != "" {
		files, rounds = 5000, 20
	}
	if testing.Short() {
		files, rounds = 300, 5
	}
	e := newEnv(t)
	for i := 0; i < files; i++ {
		e.write(e.root, fmt.Sprintf("d%02d/f%04d.md", i%40, i), "исходный")
	}
	if err := e.hub.Scan(e.fid); err != nil {
		t.Fatal(err)
	}
	cs := []*Replica{e.client("A", ModeBoth), e.client("B", ModeBoth), e.client("C", ModeBoth)}
	e.settle()
	e.converged()
	rnd := rand.New(rand.NewSource(6))
	for round := 0; round < rounds; round++ {
		for _, c := range cs {
			names := keys(tree(t, c.Path))
			for k := 0; k < 15 && len(names) > 0; k++ {
				p := names[rnd.Intn(len(names))]
				if rnd.Intn(2) == 0 {
					// «Горячие» файлы: их правят все — так появляются конфликты.
					hot := fmt.Sprintf("d%02d/f%04d.md", 0, rnd.Intn(10)*40)
					if exists(c.Path, hot) {
						p = hot
					}
				}
				switch rnd.Intn(6) {
				case 0:
					e.remove(c.Path, p)
				case 1:
					np := fmt.Sprintf("d%02d/r%s%d-%d.md", rnd.Intn(40), c.Device, round, k)
					os.Rename(full(c.Path, p), full(c.Path, np))
				case 2:
					e.write(c.Path, fmt.Sprintf("d%02d/n%s%d-%d.md", rnd.Intn(40), c.Device, round, k), "новый")
				default:
					e.write(c.Path, p, "правка "+c.Device)
				}
			}
			if rnd.Intn(3) == 0 {
				e.trs[c].cutUp = int64(rnd.Intn(40))
				e.trs[c].cutDown = int64(rnd.Intn(40))
			}
		}
		// Каждый клиент синхронизируется в случайный момент, часть — дважды.
		for _, i := range rnd.Perm(3) {
			e.sync(cs[i])
			if rnd.Intn(2) == 0 {
				e.sync(cs[rnd.Intn(3)])
			}
		}
	}
	e.settle()
	e.converged()
	e.noLoss()
	srv := tree(t, e.root)
	t.Logf("файлов на сервере: %d, конфликтов: %d", len(srv), len(conflictsIn(srv)))
	if !testing.Short() && len(conflictsIn(srv)) == 0 {
		t.Error("длительный тест не создал ни одного конфликта — проверка слабая")
	}
}
