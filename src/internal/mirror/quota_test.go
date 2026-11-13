package mirror

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// Набор тестов зеркала прогоняется ещё раз против сервера папок с квотой
// (так работает сервер плагинов, internal/node): квота большая, сценарии
// должны пройти как без неё, а учёт места — сойтись с индексом точно, без
// «до следующего пересчёта».

type quotaMeter struct {
	mu    sync.Mutex
	limit int64
	used  map[string]int64 // по папке: сумма OnUsage
	full  int              // сколько раз отказали
}

// quotaOn — тесты набора включают квоту в newEnv (не параллельно).
var quotaOn *quotaMeter

func (m *quotaMeter) attach(h *Hub) {
	h.Room = func(folder string, used int64) (int64, string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		left := m.limit - used
		if left <= 0 {
			m.full++
		}
		return left, fmt.Sprintf("папка на сервере заполнена (квота %d байт)", m.limit)
	}
	h.OnUsage = func(folder string, d int64) {
		m.mu.Lock()
		m.used[folder] += d
		m.mu.Unlock()
	}
}

// check — счёт по OnUsage равен месту по индексу.
func (m *quotaMeter) check(t *testing.T, h *Hub, fid string) {
	t.Helper()
	got, err := h.Usage(fid)
	if err != nil {
		return // папку убрали в тесте
	}
	m.mu.Lock()
	sum := m.used[fid]
	m.mu.Unlock()
	if sum != got {
		t.Fatalf("учёт квоты разошёлся: по событиям %d, по индексу %d", sum, got)
	}
}

func TestНаборЗеркалаСКвотой(t *testing.T) {
	cases := map[string]func(*testing.T){
		"Basic": TestBasicSync, "Concurrent": TestConcurrentEdit, "DeleteVsEdit": TestDeleteVsEdit,
		"Rename": TestRename, "Interrupted": TestInterruptedTransfer, "ServerDown": TestServerDownForADay,
		"Modes": TestModes, "Ignore": TestIgnore, "Versions": TestVersionsAndRetention,
		"ServerLocalEdit": TestServerLocalEdit, "CrashMidReplace": TestCrashMidReplace,
		"BadPaths": TestBadPaths, "EmptyDirs": TestEmptyDirs, "BigFile": TestБольшойФайлДокачка,
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			quotaOn = &quotaMeter{limit: 1 << 30, used: map[string]int64{}}
			t.Cleanup(func() { quotaOn = nil })
			fn(t)
		})
	}
}

// Большой файл (больше куска загрузки) с обрывами в обе стороны.
func TestБольшойФайлДокачка(t *testing.T) {
	e := newEnv(t)
	a, b := e.client("A", ModeBoth), e.client("B", ModeBoth)
	big := strings.Repeat("большой файл ", 1<<20) // 24 МБ: много кусков загрузки
	os.WriteFile(full(a.Path, "big.bin"), []byte(big), 0o644)
	e.written[sum(big)] = "big.bin"
	// Обрыв внутри куска загрузки (кусок — 4 МБ): второй проход продолжает
	// с места обрыва, а не с начала.
	want := int64(0)
	for _, cut := range []int64{3 << 20, 1 << 20} {
		e.trs[a].cutUp = cut
		if err := e.sync(a); err == nil {
			t.Fatal("обрыв загрузки не заметили")
		}
		if exists(e.root, "big.bin") {
			t.Fatal("на сервере появилось недокачанное")
		}
		want += cut
		if have, _ := e.hub.PartSize(e.fid, "A", "big.bin", sum(big)); have != want {
			t.Fatalf("докачка: загружено %d, ждали %d", have, want)
		}
	}
	e.trs[b].cutDown = 9 << 20
	e.settle()
	if read(t, b.Path, "big.bin") != big {
		t.Fatal("у B не то")
	}
	e.converged()
	e.noLoss()
}

// Квота тесная: загрузка сверх неё — отказ с понятным текстом, в папке
// сервера ничего не появилось; копия уже лежащего на сервере содержимого
// (переименование, второй экземпляр) тоже учитывается сразу.
func TestКвотаТочнаяВМоментЗаписи(t *testing.T) {
	m := &quotaMeter{limit: 3 << 20, used: map[string]int64{}}
	quotaOn = m
	t.Cleanup(func() { quotaOn = nil })
	e := newEnv(t)
	a := e.client("A", ModeBoth)

	two := strings.Repeat("x", 2<<20)
	os.WriteFile(full(a.Path, "one.bin"), []byte(two), 0o644)
	if err := e.sync(a); err != nil {
		t.Fatal(err)
	}
	m.check(t, e.hub, e.fid)

	// Второй экземпляр того же содержимого — без загрузки, копией на сервере:
	// 2 + 2 > 3 МБ — отказ прямо при commit.
	os.WriteFile(full(a.Path, "copy.bin"), []byte(two), 0o644)
	err := e.sync(a)
	if err == nil || !strings.Contains(err.Error(), "заполнена") {
		t.Fatalf("копия сверх квоты прошла: %v", err)
	}
	if exists(e.root, "copy.bin") {
		t.Fatal("копия легла в папку сервера")
	}
	m.check(t, e.hub, e.fid)

	// Новое содержимое сверх квоты — отказ при загрузке.
	os.Remove(full(a.Path, "copy.bin"))
	os.WriteFile(full(a.Path, "new.bin"), []byte(strings.Repeat("y", 2<<20)), 0o644)
	if err := e.sync(a); err == nil {
		t.Fatal("загрузка сверх квоты прошла")
	}
	if exists(e.root, "new.bin") {
		t.Fatal("файл попал в папку сервера сверх квоты")
	}
	m.check(t, e.hub, e.fid)

	// Квоту подняли — тот же файл докачивается и ложится.
	m.mu.Lock()
	m.limit = 64 << 20
	m.mu.Unlock()
	if _, err := a.Sync(context.Background(), 0, false); err != nil {
		t.Fatal(err)
	}
	if !exists(e.root, "new.bin") {
		t.Fatal("после увеличения квоты файл не лёг")
	}
	m.check(t, e.hub, e.fid)
}
