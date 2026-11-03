package traffic

import (
	"net"
	"testing"
	"time"
)

func TestStoreDaysTopLimit(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.Local)
	st := openStore(dir)
	st.now = func() time.Time { return now }
	st.add("chrome.exe", RouteServer, 1e6, 9e6)
	st.add("chrome.exe", RouteDirect, 0, 5e6)
	st.add("ssh_tunnel", RouteMesh, 1e6, 1e6)
	now = now.AddDate(0, 0, -1)
	st.add("chrome.exe", RouteServer, 0, 2e6)
	now = now.AddDate(0, 0, 1)

	r := build(st, "week", "")
	if len(r.Days) != 7 || r.Days[6].Date != "2026-11-03" {
		t.Fatalf("дни: %+v", r.Days)
	}
	if d := r.Days[6]; d.Server != 10e6 || d.Direct != 5e6 || d.Mesh != 2e6 {
		t.Fatalf("сегодня: %+v", d)
	}
	if r.Total.Server != 12e6 {
		t.Fatalf("итог: %+v", r.Total)
	}
	// Топ совпадает с суммой по путям.
	if r.Top[0].Name != "chrome.exe" || r.Top[0].Total != r.Top[0].Server+r.Top[0].Direct+r.Top[0].Mesh || r.Top[0].Total != 17e6 {
		t.Fatalf("топ: %+v", r.Top)
	}

	// Лимит 0,02 ГБ на месяц: 19 МБ — 95% → одно предупреждение 80, затем 100.
	st.setLimit(Limit{On: true, GB: 0.02, Period: "month", Scope: "all"})
	if lv, _, _ := st.checkLimit(); lv != 80 {
		t.Fatalf("ждали 80, %d", lv)
	}
	if lv, _, _ := st.checkLimit(); lv != 0 {
		t.Fatalf("повтор: %d", lv)
	}
	st.add("x", RouteServer, 2e6, 0)
	if lv, _, _ := st.checkLimit(); lv != 100 {
		t.Fatalf("ждали 100, %d", lv)
	}
	// Только через сервер: 14 МБ из 20 — меньше 80%.
	st.setLimit(Limit{On: true, GB: 0.02, Period: "month", Scope: "server"})
	if lv, _, _ := st.checkLimit(); lv != 0 {
		t.Fatalf("server: %d", lv)
	}

	// Файл переживает перезапуск, старше 3 месяцев — удаляется.
	now0 := now
	st.now = func() time.Time { return now0.AddDate(0, 0, -100) }
	st.add("old", RouteServer, 1, 1)
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	st2 := openStore(dir)
	st2.now = func() time.Time { return now0 }
	st2.prune()
	if len(st2.recs("2026-11-03")) != 4 || len(st2.recs(dayKey(now0.AddDate(0, 0, -100)))) != 0 {
		t.Fatal("после перезапуска не то")
	}
	if st2.getLimit().Scope != "server" {
		t.Fatal("лимит не сохранился")
	}
}

func TestWrapCounts(t *testing.T) {
	st := openStore(t.TempDir())
	setSink(st)
	defer setSink(nil)
	a, b := net.Pipe()
	c := Wrap(a, "app", RouteDirect)
	go func() {
		buf := make([]byte, 10)
		b.Read(buf)
		b.Write([]byte("hello"))
		b.Close()
	}()
	c.Write([]byte("0123456789"))
	buf := make([]byte, 10)
	c.Read(buf)
	c.Close()
	r := st.recs(dayKey(time.Now()))
	if len(r) != 1 || r[0].Up != 10 || r[0].Down != 5 || r[0].Route != RouteDirect {
		t.Fatalf("%+v", r)
	}
	setSink(nil)
	if Wrap(a, "app", RouteDirect) != a {
		t.Fatal("выключенный учёт должен отдавать соединение как есть")
	}
}
