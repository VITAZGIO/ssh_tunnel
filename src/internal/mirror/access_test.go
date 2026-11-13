package mirror

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sshtunnel/internal/mesh"
)

// Тесты ТЗ 18: доступ к раздаваемым папкам по устройствам.

// withAccess включает списки доступа у сервера env. id устройства в
// тестах — заголовок X-ID, нет его — имя устройства (X-Dev).
func withAccess(e *env) *Access {
	e.t.Helper()
	acl, err := OpenAccess(filepath.Join(e.t.TempDir(), "access.json"), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.hub.Visible = acl.Guard(func(r *http.Request) string {
		if id := r.Header.Get("X-ID"); id != "" {
			return id
		}
		return r.Header.Get("X-Dev")
	})
	return acl
}

func setAccess(t *testing.T, e *env, acl *Access, dev string, on bool) {
	t.Helper()
	if err := acl.Set(e.fid, dev, dev, on); err != nil {
		t.Fatal(err)
	}
	e.hub.Changed()
}

// raw — ответ сервера как есть: код и тело.
func raw(t *testing.T, e *env, dev, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Dev", dev)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Неотмеченное устройство: /folders папку не называет, а любой другой
// запрос с её id отвечает ровно так же, как про несуществующую папку.
func TestДоступНеотмеченныйНеВидитПапку(t *testing.T) {
	e := newEnv(t)
	acl := withAccess(e)
	a := e.client("A", ModeBoth)
	va := e.write(a.Path, "a.txt", "от A")
	h := sum(va)
	setAccess(t, e, acl, "A", true)
	if err := e.sync(a); err != nil {
		t.Fatal(err)
	}
	vs, _ := e.hub.Versions(e.fid, "")
	blob := "x"
	if len(vs) > 0 {
		blob = vs[0].Blob
	}

	fs, err := (&Remote{Base: e.srv.URL, HC: &http.Client{Transport: &faultTr{dev: "B"}}}).Folders(context.Background())
	if err != nil || len(fs) != 0 {
		t.Fatalf("B видит папки: %v %v", fs, err)
	}
	if fs, _ := a.R.Folders(context.Background()); len(fs) != 1 {
		t.Fatalf("A не видит свою папку: %v", fs)
	}

	type call struct{ method, path, body string }
	calls := func(f string) []call {
		return []call{
			{"GET", "/index?f=" + f + "&since=0", ""},
			{"GET", "/index?f=" + f + "&since=0&wait=1", ""},
			{"GET", "/get?f=" + f + "&p=a.txt&hash=" + h, ""},
			{"GET", "/blob?f=" + f + "&b=" + blob, ""},
			{"GET", "/part?f=" + f + "&p=b.txt&hash=" + sum("b"), ""},
			{"PUT", "/put?f=" + f + "&p=b.txt&hash=" + sum("b") + "&off=0", "b"},
			{"POST", "/commit", `{"f":"` + f + `","p":"b.txt","hash":"` + sum("b") + `","base":0}`},
			{"POST", "/delete", `{"f":"` + f + `","p":"a.txt","base":1}`},
			{"POST", "/resolve", `{"f":"` + f + `","path":"a.txt","keep":"orig"}`},
			{"POST", "/restore", `{"f":"` + f + `","blob":"` + blob + `"}`},
			{"GET", "/versions?f=" + f, ""},
		}
	}
	hidden, missing := calls(e.fid), calls("0123456789")
	for i := range hidden {
		c1, b1 := raw(t, e, "B", hidden[i].method, hidden[i].path, hidden[i].body)
		c2, b2 := raw(t, e, "B", missing[i].method, missing[i].path, missing[i].body)
		if c1 != c2 || b1 != b2 || c1 == 200 {
			t.Errorf("%s %s: скрытая %d %q, несуществующая %d %q", hidden[i].method, hidden[i].path, c1, b1, c2, b2)
		}
	}
	if read(t, e.root, "a.txt") != va || exists(e.root, "b.txt") {
		t.Fatal("неотмеченное устройство изменило папку")
	}

	// Его страница: «владелец закрыл доступ», своё на месте.
	b := e.client("B", ModeBoth)
	vb := e.write(b.Path, "своё.txt", "B")
	if _, err := b.Sync(context.Background(), 0, false); !Closed(err) {
		t.Fatalf("B: %v", err)
	}
	if st := b.Status(); st.State != "closed" || st.Error != ClosedText {
		t.Fatalf("статус B: %+v", st)
	}
	if read(t, b.Path, "своё.txt") != vb || exists(b.Path, "a.txt") {
		t.Fatal("закрытая папка тронула файлы B")
	}
}

// getCount — сколько раз устройство скачивало содержимое.
type getCount struct {
	inner http.RoundTripper
	n     atomic.Int64
	after func(*http.Request) // после ответа (тесты)
}

func (g *getCount) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/get" {
		g.n.Add(1)
	}
	resp, err := g.inner.RoundTrip(r)
	if g.after != nil {
		g.after(r)
	}
	return resp, err
}

// Выдать → видно и синхронизируется; забрать → следующий вызов не
// проходит, копия на устройстве цела; вернуть → сверка продолжается без
// повторной загрузки того, что уже есть.
func TestДоступВыдатьЗабратьВернуть(t *testing.T) {
	e := newEnv(t)
	acl := withAccess(e)
	a, b := e.client("A", ModeBoth), e.client("B", ModeBoth)
	gc := &getCount{inner: b.R.HC.Transport}
	b.R.HC.Transport = gc
	setAccess(t, e, acl, "A", true)
	v1 := e.write(a.Path, "1.txt", "один")
	v2 := e.write(a.Path, "2.txt", "два")
	if err := e.sync(a); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Sync(context.Background(), 0, false); !Closed(err) {
		t.Fatalf("до галочки: %v", err)
	}
	setAccess(t, e, acl, "B", true)
	if err := e.sync(b); err != nil {
		t.Fatal(err)
	}
	if read(t, b.Path, "1.txt") != v1 || gc.n.Load() != 2 {
		t.Fatalf("после галочки: %d скачиваний", gc.n.Load())
	}

	setAccess(t, e, acl, "B", false)
	v3 := e.write(a.Path, "3.txt", "три")
	vB := e.write(b.Path, "от-B.txt", "B без доступа")
	if err := e.sync(a); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Sync(context.Background(), 0, false); !Closed(err) || b.Status().State != "closed" {
		t.Fatalf("после снятия: %v %+v", err, b.Status())
	}
	if read(t, b.Path, "1.txt") != v1 || read(t, b.Path, "2.txt") != v2 || exists(b.Path, "3.txt") {
		t.Fatal("снятие доступа тронуло копию на B")
	}
	if exists(e.root, "от-B.txt") {
		t.Fatal("B без доступа записал в папку")
	}

	setAccess(t, e, acl, "B", true)
	e.settle()
	if gc.n.Load() != 3 { // только новый 3.txt
		t.Fatalf("после возврата скачано заново: %d", gc.n.Load())
	}
	if read(t, b.Path, "3.txt") != v3 || read(t, e.root, "от-B.txt") != vB {
		t.Fatal("после возврата не сошлось")
	}
	e.converged()
	e.noLoss()
}

// Доступ забрали посреди загрузки большого файла: следующий запрос не
// проходит, на сервер ничего не попало, у устройства файл цел; вернули —
// загрузка доходит.
func TestДоступЗабралиПосредиЗагрузки(t *testing.T) {
	e := newEnv(t)
	acl := withAccess(e)
	a := e.client("A", ModeBoth)
	setAccess(t, e, acl, "A", true)
	big := strings.Repeat("0123456789", 1<<20) // 10 МБ: несколько кусков
	os.WriteFile(full(a.Path, "big.bin"), []byte(big), 0o644)
	e.written[sum(big)] = "big.bin"
	var once sync.Once
	gc := &getCount{inner: a.R.HC.Transport, after: func(r *http.Request) {
		if r.URL.Path == "/put" {
			once.Do(func() { setAccess(t, e, acl, "A", false) })
		}
	}}
	a.R.HC.Transport = gc
	if _, err := a.Sync(context.Background(), 0, false); err == nil {
		t.Fatal("загрузка прошла без доступа")
	}
	if exists(e.root, "big.bin") {
		t.Fatal("на сервере файл без доступа")
	}
	if _, err := a.Sync(context.Background(), 0, false); !Closed(err) {
		t.Fatalf("следующий проход: %v", err)
	}
	if b, _ := os.ReadFile(full(a.Path, "big.bin")); string(b) != big {
		t.Fatal("файл на устройстве тронут")
	}
	setAccess(t, e, acl, "A", true)
	e.settle()
	if read(t, e.root, "big.bin") != big {
		t.Fatal("после возврата доступа файл не дошёл")
	}
	e.noLoss()
}

// Долгий опрос того, у кого забрали доступ, кончается сразу, а не через
// минуту, и отвечает как про несуществующую папку.
func TestДоступДолгийОпросЗакрываетсяСразу(t *testing.T) {
	e := newEnv(t)
	acl := withAccess(e)
	a := e.client("A", ModeBoth)
	setAccess(t, e, acl, "A", true)
	if err := e.sync(a); err != nil {
		t.Fatal(err)
	}
	rep, _ := e.hub.Index(e.fid, 0, 0, nil)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := a.R.Index(context.Background(), e.fid, rep.Seq, 30*time.Second)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	setAccess(t, e, acl, "A", false)
	select {
	case err := <-done:
		if !Closed(err) {
			t.Fatalf("опрос: %v", err)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("опрос ждал до конца")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("опрос не закончился")
	}
	// Тот, у кого доступ остался, ждёт как обычно и получает изменение.
	setAccess(t, e, acl, "A", true)
	go func() {
		_, err := a.R.Index(context.Background(), e.fid, rep.Seq, 30*time.Second)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := acl.Set(e.fid, "B", "B", true); err != nil { // чужая галочка
		t.Fatal(err)
	}
	e.hub.Changed()
	e.write(e.root, "new.txt", "сервер")
	if err := e.hub.Scan(e.fid); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("опрос с доступом: %v", err)
	}
}

// Правило обновления: папки, которые раздавались до этой версии, видны всем,
// пока не известны id устройств; потом — каждому устройству, известному в
// тот момент. Новая папка — ни для кого. Устройство, пришедшее позже, —
// ничего.
func TestДоступПравилоОбновления(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.json")
	acl, err := OpenAccess(path, []string{"old1", "old2"})
	if err != nil {
		t.Fatal(err)
	}
	if !acl.Pending() || !acl.Allowed("old1", "") || !acl.Allowed("old2", "кто-угодно") {
		t.Fatal("до id устройств старые папки должны быть видны всем")
	}
	if acl.Allowed("new", "pc") {
		t.Fatal("новая папка видна без галочки")
	}
	if did, _ := acl.Migrate(nil); did || !acl.Pending() {
		t.Fatal("без id устройств правило применять нечего")
	}
	if did, err := acl.Migrate([]Known{{ID: "pc", Name: "ПК"}, {ID: "phone", Name: "Телефон"}}); !did || err != nil {
		t.Fatal(did, err)
	}
	for _, f := range []string{"old1", "old2"} {
		if !acl.Allowed(f, "pc") || !acl.Allowed(f, "phone") || acl.Allowed(f, "later") || acl.Allowed(f, "") {
			t.Fatalf("после правила, %s", f)
		}
	}
	if acl.Allowed("new", "pc") || acl.Pending() {
		t.Fatal("новая папка получила устройства по правилу обновления")
	}
	// Правило — один раз: открыли снова (файл есть) — те же списки, а не
	// «старые» заново.
	acl2, err := OpenAccess(path, []string{"old1", "old2", "new"})
	if err != nil {
		t.Fatal(err)
	}
	if acl2.Pending() || !acl2.Allowed("old1", "phone") || acl2.Allowed("new", "phone") || acl2.Name("phone") != "Телефон" {
		t.Fatal("после перезапуска")
	}
	g := acl2.Granted()
	if g["pc"] != 2 || g["phone"] != 2 {
		t.Fatalf("сколько папок: %v", g)
	}
	// Первая правка старой папки до правила делает её обычной.
	acl3, _ := OpenAccess(filepath.Join(dir, "a3.json"), []string{"old"})
	if err := acl3.Set("old", "pc", "", true); err != nil {
		t.Fatal(err)
	}
	if acl3.Allowed("old", "") || !acl3.Allowed("old", "pc") || acl3.Allowed("old", "phone") {
		t.Fatal("правка старой папки")
	}
}

// Старое устройство (без id: старый meshd у владельца) после правила
// видит меньше папок; до правила — всё, как раньше. Владелец старой версии
// (Visible нет) пускает всех — это весь набор тестов ТЗ 6 выше.
func TestДоступСтарыйКлиент(t *testing.T) {
	e := newEnv(t)
	acl, err := OpenAccess(filepath.Join(t.TempDir(), "access.json"), []string{e.fid})
	if err != nil {
		t.Fatal(err)
	}
	e.hub.Visible = acl.Guard(func(r *http.Request) string { return r.Header.Get("X-ID") })
	old := e.client("", ModeBoth) // без id
	vx := e.write(e.root, "x.txt", "x")
	e.hub.Scan(e.fid)
	if err := e.sync(old); err != nil || read(t, old.Path, "x.txt") != vx {
		t.Fatalf("до правила старый клиент: %v", err)
	}
	acl.Migrate([]Known{{ID: "pc"}})
	e.hub.Changed()
	if fs, _ := old.R.Folders(context.Background()); len(fs) != 0 {
		t.Fatalf("после правила старый клиент видит: %v", fs)
	}
	if _, err := old.Sync(context.Background(), 0, false); !Closed(err) || read(t, old.Path, "x.txt") != vx {
		t.Fatalf("после правила: %v", err)
	}
}

// idTr — устройство с постоянным id и меняющимися именем и адресом.
type idTr struct {
	inner *faultTr
	id    string
}

func (t *idTr) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-ID", t.id)
	return t.inner.RoundTrip(r)
}

// Доступ — по id: сменились адрес и имя устройства — доступ остался;
// другое устройство взяло старое имя — доступа нет.
func TestДоступПослеСменыАдресаИИмени(t *testing.T) {
	e := newEnv(t)
	acl := withAccess(e)
	a := e.client("pc", ModeBoth)
	ft := e.trs[a]
	a.R.HC.Transport = &idTr{inner: ft, id: "id-pc"}
	if err := acl.Set(e.fid, "id-pc", "pc", true); err != nil {
		t.Fatal(err)
	}
	e.write(a.Path, "a.txt", "a")
	if err := e.sync(a); err != nil {
		t.Fatal(err)
	}
	ft.dev = "pc-2" // новое имя (и адрес: сервер его не смотрит)
	vb := e.write(a.Path, "b.txt", "b")
	if err := e.sync(a); err != nil || read(t, e.root, "b.txt") != vb {
		t.Fatalf("после смены имени: %v", err)
	}
	other := e.client("pc", ModeBoth) // чужое устройство со старым именем
	other.R.HC.Transport = &idTr{inner: e.trs[other], id: "id-other"}
	if _, err := other.Sync(context.Background(), 0, false); !Closed(err) {
		t.Fatalf("имя дало доступ: %v", err)
	}
}

// Список «Доступ к папкам»: сначала серверы, потом остальные, по имени;
// не в сети — тоже; убранные из сети, у которых остались папки, — в конце
// своей группы с Gone; без id (старый meshd) и сам себя — нет.
func TestДоступСписокУстройств(t *testing.T) {
	acl, _ := OpenAccess(filepath.Join(t.TempDir(), "a.json"), nil)
	acl.Set("f1", "id-phone", "", true)
	acl.Set("f2", "id-phone", "", true)
	acl.Set("f1", "id-gone", "Старый ноут", true)
	ip := func(s string) netip.Addr { return netip.MustParseAddr(s) }
	peers := []mesh.Peer{
		{ID: "id-self", Name: "ПК", IP: ip("198.19.0.1"), Self: true},
		{ID: "id-phone", Name: "телефон", IP: ip("198.19.0.2"), Online: true},
		{ID: "id-tv", Name: "ТВ", IP: ip("198.19.0.3")},
		{ID: "id-lon", Name: "London", IP: ip("198.19.255.20"), Platform: "server", Online: true},
		{ID: "id-fra", Name: "Frankfurt", IP: ip("198.19.255.254"), Platform: "server", Online: true},
		{Name: "старый", IP: ip("198.19.0.9")},
	}
	got := listDevices(peers, acl)
	var names []string
	for _, d := range got {
		names = append(names, d.Name)
	}
	want := "Frankfurt,London,Старый ноут,ТВ,телефон"
	if strings.Join(names, ",") != want {
		t.Fatalf("порядок: %v, ждали %s", names, want)
	}
	for _, d := range got {
		switch d.ID {
		case "id-phone":
			if d.Folders != 2 || !d.Online {
				t.Fatalf("телефон: %+v", d)
			}
		case "id-gone":
			if !d.Gone || d.Folders != 1 {
				t.Fatalf("убранный: %+v", d)
			}
		case "id-tv":
			if d.Online || d.Folders != 0 {
				t.Fatalf("ТВ: %+v", d)
			}
		}
	}
}
