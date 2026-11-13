package docker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Строгий режим (ТЗ 14): адреса «через сервер» идут в адаптер со своим
// адресом (Forced); галочка «без VPN — без интернета» держит правила и без
// адаптера — со своими приоритетами и без дороги в адаптер.
func TestKernelStrict(t *testing.T) {
	ts := []Target{
		{Addr: ip("172.17.0.2"), Route: ChoiceTunnel},
		{Addr: ip("172.17.0.4"), Route: ChoiceTunnel, Strict: true},
		{Addr: ip("172.17.0.3"), Route: ChoiceDirect},
	}
	full := Net{Up: true, Full: true, Tun: "sshtun0", Uplink4: "eth0", Gw4: ip("192.168.1.1")}

	p := KernelPlan(ts, full)
	for _, w := range []string{
		"v4 pref 9401 from 172.17.0.2/32 lookup 7094",
		"v4 pref 9402 from 172.17.0.4/32 ipproto 17 dport 53 lookup 7095",
		"v4 pref 9403 from 172.17.0.4/32 lookup 7094",
		"v4 table 7094 dev 0.0.0.0/0 dev sshtun0",
	} {
		if !has(p, w) {
			t.Errorf("с адаптером нет %q\n%v", w, p)
		}
	}
	if len(p.Forced) != 2 || p.Forced[0] != ip("172.17.0.2") || p.Forced[1] != ip("172.17.0.4") {
		t.Errorf("Forced = %v, ждали оба «через сервер»", p.Forced)
	}

	down := KernelPlan(ts, Net{})
	if len(down.Forced) != 0 {
		t.Errorf("без адаптера Forced = %v", down.Forced)
	}
	for _, w := range []string{
		"v4 pref 9403 from 172.17.0.4/32 lookup 7094",
		"v4 pref 9402 from 172.17.0.4/32 ipproto 6 dport 53 lookup 7095",
		"v4 table 7094 throw 192.168.0.0/16",
		"v4 table 7094 unreachable 0.0.0.0/0 metric 4294967040",
		"v4 table 7095 unreachable 0.0.0.0/0 metric 4294967040",
	} {
		if !has(down, w) {
			t.Errorf("без адаптера нет %q\n%v", w, down)
		}
	}
	for _, w := range []string{
		"v4 pref 9401 from 172.17.0.2/32 lookup 7094",
		"v4 pref 9401 from 172.17.0.3/32 lookup 7096",
		"v4 table 7094 dev 198.18.0.0/15 dev sshtun0",
	} {
		if has(down, w) {
			t.Errorf("без адаптера лишнее %q", w)
		}
	}
	for _, r := range down.Rules {
		if !StrictPrio(r.Priority) {
			t.Errorf("без адаптера правило не строгого приоритета: %v", r)
		}
	}
	if !KernelPlan(ts[:1], Net{}).Empty() {
		t.Error("без строгих и без адаптера план должен быть пуст")
	}
}

func TestApplyStrictAndTargets(t *testing.T) {
	rows := Resolve([]Container{
		ctr("a", "", "", "bridge", "172.17.0.2", nil),
		ctr("b", "", "", "bridge", "172.17.0.3", nil),
	}, Choices{"name:a": ChoiceTunnel, "name:b": ChoiceDirect})
	ApplyStrict(rows, map[string]bool{"name:a": true, "name:b": true})
	ts := Targets(rows)
	if len(ts) != 2 || !ts[0].Strict || ts[1].Strict {
		t.Fatalf("строгий — только «через сервер»: %v", ts)
	}
	if !rows[1].StrictSet || rows[1].Strict {
		t.Fatalf("у «напрямую» галочка видна, но не действует: %+v", rows[1])
	}
}

func TestStoreStrict(t *testing.T) {
	dir := t.TempDir()
	st := OpenStore(dir)
	c := Container{Name: "a"}
	if err := st.SetStrict(c, true); err != nil {
		t.Fatal(err)
	}
	if !OpenStore(dir).Strict()["name:a"] {
		t.Fatal("галочка не пережила перечитывание")
	}
	st.SetStrict(c, false)
	if len(OpenStore(dir).Strict()) != 0 {
		t.Fatal("галочка не снялась")
	}
}

// API: галочка ставится запросом; Stop оставляет в ядре только строгих.
func TestServiceStrict(t *testing.T) {
	dockerAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/json":
			w.Write([]byte(`[{"Id":"aaa111","Names":["/a"],"Image":"x","State":"running","Labels":{},"Ports":[{"PrivatePort":80,"PublicPort":8080}],
			  "HostConfig":{"NetworkMode":"bridge"},"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}},
			 {"Id":"bbb222","Names":["/b"],"Image":"x","State":"running","Labels":{},
			  "HostConfig":{"NetworkMode":"bridge"},"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.3"}}}}]`))
		case "/events":
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer dockerAPI.Close()
	k := &fakeKernel{}
	SetKernel(k)
	defer SetKernel(nil)
	s := &Service{st: OpenStore(t.TempDir()), cli: newHTTPClient(dockerAPI.URL)}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	post := func(body string) int {
		rec := httptest.NewRecorder()
		handleSet(rec, httptest.NewRequest("POST", "/api/docker/set", strings.NewReader(body)))
		return rec.Code
	}
	if post(`{"container":"a","route":"tunnel","strict":true}`) != 200 || post(`{"container":"b","route":"tunnel"}`) != 200 {
		t.Fatal("set")
	}
	if post(`{"project":"p","strict":true}`) != 400 {
		t.Fatal("strict для проекта должен быть отказом")
	}
	ts := k.get()
	if len(ts) != 2 || !ts[0].Strict || ts[1].Strict {
		t.Fatalf("в ядро: %v", ts)
	}
	s.mu.Lock()
	pub := s.rows[0].Published
	s.mu.Unlock()
	if !pub {
		t.Error("опубликованный порт не замечен")
	}
	s.Stop()
	if ts := k.get(); len(ts) != 1 || ts[0].Addr != ip("172.17.0.2") || !ts[0].Strict {
		t.Fatalf("после Stop должен остаться только строгий: %v", ts)
	}
}
