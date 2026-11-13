package backup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// В тестах «кто пришёл» подставляется прямо в контекст запроса, как это в
// бою делает хост плагинов по ответу meshd (op who).

func init() { stableWait = 20 * time.Millisecond }

type env struct {
	t     *testing.T
	store *Store
	dir   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	return &env{t: t, store: OpenStore(dir), dir: dir}
}

// cutTr — транспорт с обрывом загрузки после N байт (один раз).
type cutTr struct {
	mu  sync.Mutex
	cut int64
}

type cutBody struct {
	r    io.Reader
	left int64
}

func (c *cutBody) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errors.New("обрыв (тест)")
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}
func (c *cutBody) Close() error { return nil }

func (c *cutTr) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	cut := c.cut
	if req.URL.Path == "/put" && cut > 0 && req.Body != nil {
		c.cut = 0
		req = req.Clone(req.Context())
		req.Body = &cutBody{req.Body, cut}
		req.ContentLength = -1
	}
	c.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

// as — клиент, которого сервер считает устройством c.
func (e *env) as(c Caller) (*Remote, *cutTr) {
	inner := e.store.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(WithCaller(r.Context(), c)))
	}))
	e.t.Cleanup(srv.Close)
	tr := &cutTr{}
	return &Remote{Base: srv.URL, HC: &http.Client{Transport: tr}}, tr
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var (
	pc    = Caller{Device: "pc", Name: "ПК", User: "vitaly"}
	phone = Caller{Device: "phone", Name: "Телефон", User: "vitaly"}
	alien = Caller{Device: "tv", Name: "Чужой", User: "guest"}
)

func job(path string) Job {
	return Job{ID: "docs", Name: "Документы", Path: path, Schedule: Daily, Keep: 3}
}

func TestКопияИВосстановление(t *testing.T) {
	e := newEnv(t)
	rm, _ := e.as(pc)
	src := t.TempDir()
	write(t, src, "a.txt", "первый")
	write(t, src, "папка/b.txt", "второй")
	write(t, src, "папка/копия b.txt", "второй") // то же содержимое
	os.MkdirAll(filepath.Join(src, "пустая"), 0o755)
	cache := LoadCache(filepath.Join(t.TempDir(), "c.json"))
	ctx := context.Background()

	res, err := RunBackup(ctx, rm, job(src), cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Uploaded != int64(len("первый")+len("второй")) {
		t.Fatalf("первая копия: %+v", res)
	}
	// Ничего не изменилось — вторая копия ничего не отправляет.
	res2, err := RunBackup(ctx, rm, job(src), cache, nil)
	if err != nil || res2.Uploaded != 0 {
		t.Fatalf("повтор отправил %d байт: %v", res2.Uploaded, err)
	}

	// Правка → третья версия; восстановление старой версии в другую папку.
	write(t, src, "a.txt", "изменён")
	if _, err := RunBackup(ctx, rm, job(src), cache, nil); err != nil {
		t.Fatal(err)
	}
	vs, err := rm.Versions(ctx, "pc", "docs")
	if err != nil || len(vs) != 3 {
		t.Fatalf("версии: %v %v", vs, err)
	}
	dst := t.TempDir()
	r, err := Restore(ctx, rm, RestoreReq{Device: "pc", Set: "docs", Version: vs[2].ID, Target: dst}, time.Now())
	if err != nil || r.Written != 3 {
		t.Fatalf("восстановление: %+v %v", r, err)
	}
	if read(t, filepath.Join(dst, "a.txt")) != "первый" || read(t, filepath.Join(dst, "папка", "b.txt")) != "второй" {
		t.Fatal("восстановлено не то")
	}
	if st, err := os.Stat(filepath.Join(dst, "пустая")); err != nil || !st.IsDir() {
		t.Fatal("пустая папка не восстановлена")
	}

	// Один файл новой версии — на место, где лежит другой: не перезаписан,
	// восстановленное легло рядом.
	r, err = Restore(ctx, rm, RestoreReq{Device: "pc", Set: "docs", Version: vs[0].ID, Path: "a.txt", Target: dst}, time.Date(2026, 11, 13, 0, 0, 0, 0, time.Local))
	if err != nil || r.Renamed != 1 {
		t.Fatalf("восстановление поверх: %+v %v", r, err)
	}
	if read(t, filepath.Join(dst, "a.txt")) != "первый" || read(t, filepath.Join(dst, "a (из копии 13.11.2026).txt")) != "изменён" {
		t.Fatal("перезаписан молча или положен не туда")
	}
	// Тот же файл ещё раз — уже такой же, ничего не делается.
	r, _ = Restore(ctx, rm, RestoreReq{Device: "pc", Set: "docs", Version: vs[2].ID, Path: "a.txt", Target: dst, Mode: Skip}, time.Now())
	if r.Same != 1 || r.Written != 0 {
		t.Fatalf("одинаковый файл: %+v", r)
	}
	// Явная замена.
	r, _ = Restore(ctx, rm, RestoreReq{Device: "pc", Set: "docs", Version: vs[0].ID, Path: "a.txt", Target: dst, Mode: Overwrite}, time.Now())
	if r.Written != 1 || read(t, filepath.Join(dst, "a.txt")) != "изменён" {
		t.Fatalf("замена: %+v", r)
	}
	// Папка целиком: ложится своим именем.
	dst2 := t.TempDir()
	r, err = Restore(ctx, rm, RestoreReq{Device: "pc", Set: "docs", Version: vs[0].ID, Path: "папка", Target: dst2}, time.Now())
	if err != nil || r.Written != 2 || read(t, filepath.Join(dst2, "папка", "копия b.txt")) != "второй" {
		t.Fatalf("папка: %+v %v", r, err)
	}
}

func TestХранитНеБольшеKeepИЧистит(t *testing.T) {
	e := newEnv(t)
	rm, _ := e.as(pc)
	src := t.TempDir()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		write(t, src, "f.txt", strings.Repeat("v", i+1))
		if _, err := RunBackup(ctx, rm, job(src), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	vs, _ := rm.Versions(ctx, "pc", "docs")
	if len(vs) != 3 {
		t.Fatalf("версий %d, ждали 3", len(vs))
	}
	// Содержимое выпавших версий убрано с диска.
	blobs := 0
	filepath.WalkDir(filepath.Join(e.dir, pc.Scope(), "blobs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobs++
		}
		return nil
	})
	if blobs != 3 {
		t.Fatalf("на диске %d содержимых, ждали 3", blobs)
	}
	// Удаление копии целиком — пусто.
	if err := rm.Delete(ctx, "pc", "docs", ""); err != nil {
		t.Fatal(err)
	}
	if sets, _ := rm.Sets(ctx); len(sets) != 0 {
		t.Fatalf("после удаления: %+v", sets)
	}
	if n := e.store.Stored(); n != 0 {
		t.Fatalf("после удаления на диске %d байт", n)
	}
}

func TestДокачкаПослеОбрыва(t *testing.T) {
	e := newEnv(t)
	rm, tr := e.as(pc)
	src := t.TempDir()
	big := strings.Repeat("0123456789", 3<<20) // 30 МБ: несколько кусков
	write(t, src, "big.bin", big)
	ctx := context.Background()
	tr.cut = 5 << 20
	if _, err := RunBackup(ctx, rm, job(src), nil, nil); err == nil {
		t.Fatal("обрыв не заметили")
	}
	if sets, _ := rm.Sets(ctx); len(sets) != 0 {
		t.Fatal("версия записана по оборванной загрузке")
	}
	var sent int64
	if _, err := RunBackup(ctx, rm, job(src), nil, func(ph string, done, total int64) {
		if ph == "upload" {
			sent = done
		}
	}); err != nil {
		t.Fatal(err)
	}
	if sent >= int64(len(big)) {
		t.Fatalf("после обрыва отправлено всё заново: %d", sent)
	}
	dst := t.TempDir()
	vs, _ := rm.Versions(ctx, "pc", "docs")
	if _, err := Restore(ctx, rm, RestoreReq{Device: "pc", Set: "docs", Version: vs[0].ID, Target: dst}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dst, "big.bin")) != big {
		t.Fatal("восстановлено не то")
	}
}

// Файл, который всё время меняется, в копию не попадает (с пометкой), а
// половиной не ложится никогда; остальное копируется.
func TestМеняющийсяФайлПропущен(t *testing.T) {
	e := newEnv(t)
	rm, _ := e.as(pc)
	src := t.TempDir()
	write(t, src, "ok.txt", "спокойный")
	write(t, src, "log.txt", "старт")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			f, _ := os.OpenFile(filepath.Join(src, "log.txt"), os.O_APPEND|os.O_WRONLY, 0o644)
			f.WriteString(strings.Repeat("x", 1000))
			f.Close()
			os.Chtimes(filepath.Join(src, "log.txt"), time.Now(), time.Unix(int64(i), 0))
			time.Sleep(time.Millisecond)
		}
	}()
	res, err := RunBackup(context.Background(), rm, job(src), nil, nil)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	skipped := false
	for _, s := range res.Skipped {
		if s.P == "log.txt" {
			skipped = true
		}
	}
	m, _ := rm.Manifest(context.Background(), "pc", "docs", res.Version.ID)
	for _, f := range m.Files {
		if f.P == "log.txt" {
			// Если всё же попал — то только целиком проверенным по хешу.
			skipped = true
		}
	}
	if !skipped || res.Files < 1 {
		t.Fatalf("итог: %+v", res)
	}
}

// Сервер сам проверяет хеш: прислали не то — не принимает (содержимое не
// ложится половиной или подменой).
func TestСерверНеПринимаетНеТо(t *testing.T) {
	e := newEnv(t)
	h := strings.Repeat("a", 64)
	if _, err := e.store.Put(pc, h, 0, 4, "docs", 3, strings.NewReader("abcd")); !errors.Is(err, ErrChanged) {
		t.Fatalf("ждали ErrChanged: %v", err)
	}
	if e.store.PartSize(pc, h) != 0 || len(e.store.Missing(pc, []string{h})) != 1 {
		t.Fatal("подложное содержимое осталось")
	}
	if _, err := e.store.Commit(pc, CommitReq{Set: "docs", Name: "x", Keep: 1, Files: []File{{P: "a", Size: 4, Hash: h}}}); err == nil {
		t.Fatal("версия без содержимого записана")
	}
	for _, bad := range []string{"../x", "/etc/passwd", `a\..\b`, "a//b", ""} {
		if _, err := e.store.Commit(pc, CommitReq{Set: "docs", Name: "x", Keep: 1, Files: []File{{P: bad, Hash: h}}}); err == nil {
			t.Fatalf("принят путь %q", bad)
		}
	}
	if _, err := e.store.Versions(pc, "pc", "../../x"); err == nil {
		t.Fatal("принят id копии с «..»")
	}
}

// Читать копию может только её устройство и устройства того же
// SSH-пользователя. Другое устройство сети не видит ни копий, ни даже того,
// есть ли на сервере такое содержимое.
func TestЧужиеНеВидятКопий(t *testing.T) {
	e := newEnv(t)
	rmPC, _ := e.as(pc)
	rmPhone, _ := e.as(phone)
	rmAlien, _ := e.as(alien)
	rmNoUserA, _ := e.as(Caller{Device: "a", Name: "A"})
	rmNoUserB, _ := e.as(Caller{Device: "b", Name: "B"})
	src := t.TempDir()
	write(t, src, "секрет.txt", "пароль от всего")
	ctx := context.Background()
	res, err := RunBackup(ctx, rmPC, job(src), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := rmPC.Manifest(ctx, "pc", "docs", res.Version.ID)
	hash := m.Files[0].Hash

	// Телефон того же пользователя видит и восстанавливает.
	sets, err := rmPhone.Sets(ctx)
	if err != nil || len(sets) != 1 || sets[0].Own || sets[0].DeviceName != "ПК" {
		t.Fatalf("телефон: %+v %v", sets, err)
	}
	if _, err := Restore(ctx, rmPhone, RestoreReq{Device: "pc", Set: "docs", Version: res.Version.ID, Target: t.TempDir()}, time.Now()); err != nil {
		t.Fatalf("телефон не восстановил: %v", err)
	}

	for name, rm := range map[string]*Remote{"чужой пользователь": rmAlien, "без пользователя": rmNoUserA} {
		if sets, _ := rm.Sets(ctx); len(sets) != 0 {
			t.Fatalf("%s видит копии: %+v", name, sets)
		}
		if _, err := rm.Versions(ctx, "pc", "docs"); err == nil {
			t.Fatalf("%s видит версии", name)
		}
		if _, err := rm.Manifest(ctx, "pc", "docs", res.Version.ID); err == nil {
			t.Fatalf("%s видит список файлов", name)
		}
		if b, err := rm.Blob(ctx, hash, 0); err == nil {
			b.Close()
			t.Fatalf("%s читает содержимое", name)
		}
		if miss, _ := rm.Missing(ctx, []string{hash}); len(miss) != 1 {
			t.Fatalf("%s узнал, что такое содержимое на сервере есть", name)
		}
		if err := rm.Delete(ctx, "pc", "docs", ""); err == nil {
			t.Fatalf("%s удалил чужую копию", name)
		}
	}
	// Устройства без SSH-пользователя разделены между собой тоже.
	if _, err := RunBackup(ctx, rmNoUserA, job(src), nil, nil); err != nil {
		t.Fatal(err)
	}
	if sets, _ := rmNoUserB.Sets(ctx); len(sets) != 0 {
		t.Fatalf("устройство без пользователя видит чужие копии: %+v", sets)
	}
	if sets, _ := rmPC.Sets(ctx); len(sets) != 1 {
		t.Fatalf("у ПК копий: %+v", sets)
	}
}

// Место кончилось: сначала уходит самая старая версия сверх keep (с новой
// их станет keep), дальше — отказ с понятным текстом.
func TestКвотаСначалаСтарыеВерсии(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var used int64
	limit := int64(2500)
	e.store.Room = func() (int64, string) {
		mu.Lock()
		defer mu.Unlock()
		return limit - used, "на сервере кончилось место для плагинов"
	}
	e.store.OnUsage = func(d int64) { mu.Lock(); used += d; mu.Unlock() }
	rm, _ := e.as(pc)
	src := t.TempDir()
	ctx := context.Background()
	j := job(src)
	j.Keep = 2
	for i := 0; i < 2; i++ {
		write(t, src, "f.bin", strings.Repeat(string(rune('a'+i)), 1000))
		if _, err := RunBackup(ctx, rm, j, nil, nil); err != nil {
			t.Fatalf("копия %d: %v", i, err)
		}
	}
	// Третья: места на 1000 байт нет, пока не уйдёт первая версия.
	write(t, src, "f.bin", strings.Repeat("c", 1000))
	if _, err := RunBackup(ctx, rm, j, nil, nil); err != nil {
		t.Fatalf("третья копия: %v", err)
	}
	vs, _ := rm.Versions(ctx, "pc", "docs")
	if len(vs) != 2 {
		t.Fatalf("версий %d", len(vs))
	}
	// Файл больше всей квоты — отказ, понятный текст, версии целы.
	write(t, src, "f.bin", strings.Repeat("d", 5000))
	_, err := RunBackup(ctx, rm, j, nil, nil)
	var re *RemoteError
	if !errors.As(err, &re) || !re.Full() || !strings.Contains(re.Text, "кончилось место") {
		t.Fatalf("ждали отказ «нет места»: %v", err)
	}
	if vs, _ := rm.Versions(ctx, "pc", "docs"); len(vs) < 1 {
		t.Fatal("после отказа пропали все версии")
	}
	mu.Lock()
	u := used
	mu.Unlock()
	if st := e.store.Stored(); st != u {
		t.Fatalf("учёт места разошёлся: по событиям %d, на диске %d", u, st)
	}
}

func TestНастройкиКопии(t *testing.T) {
	dir := t.TempDir()
	j := Job{Path: dir, Schedule: Change}
	if err := j.Check(); err != nil || j.DelayMin != 10 || j.Keep != 10 || j.Name == "" {
		t.Fatalf("умолчания: %+v %v", j, err)
	}
	for _, bad := range []Job{{Path: "rel", Schedule: Daily}, {Path: dir, Schedule: "hourly"}, {Path: dir, Schedule: Daily, Keep: MaxKeep + 1}} {
		if err := bad.Check(); err == nil {
			t.Fatalf("принято %+v", bad)
		}
	}
	now := time.Unix(1_000_000, 0)
	r := &jobRun{Job: Job{Schedule: Daily}}
	if !due(r, now) {
		t.Fatal("первая копия не нужна?")
	}
	r.st.LastOK = now.Add(-23 * time.Hour).Unix()
	if due(r, now) {
		t.Fatal("ежедневная раньше суток")
	}
	r.st.LastOK = now.Add(-25 * time.Hour).Unix()
	if !due(r, now) {
		t.Fatal("ежедневная не пошла")
	}
	r.st.LastErr, r.st.LastRun = "ошибка", now.Add(-time.Minute).Unix()
	if due(r, now) {
		t.Fatal("после ошибки повтор сразу же")
	}
	c := &jobRun{Job: Job{Schedule: Change, DelayMin: 5}, st: Status{LastOK: 1, Changed: now.Add(-4 * time.Minute).Unix()}}
	if due(c, now) {
		t.Fatal("по изменению раньше задержки")
	}
	c.st.Changed = now.Add(-6 * time.Minute).Unix()
	if !due(c, now) {
		t.Fatal("по изменению не пошла")
	}
}
