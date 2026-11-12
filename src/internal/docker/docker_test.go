package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

func ctr(name, project, service, mode, addr string, labels map[string]string) Container {
	c := Container{ID: "id-" + name, Name: name, State: "running", Project: project, Service: service, NetMode: mode, Labels: labels}
	if strings.HasPrefix(mode, "container:") {
		c.NetMode, c.Parent = "container", strings.TrimPrefix(mode, "container:")
	}
	if addr != "" {
		c.Networks = []Network{{Name: "bridge", IP: ip(addr)}}
	}
	return c
}

func TestResolve(t *testing.T) {
	cs := []Container{
		ctr("web", "", "", "bridge", "172.17.0.2", nil),
		ctr("db", "shop", "db", "bridge", "172.18.0.2", nil),
		ctr("api", "shop", "api", "bridge", "172.18.0.3", nil),
		ctr("lbl", "shop", "lbl", "bridge", "172.18.0.4", map[string]string{Label: "direct"}),
		ctr("hostnet", "", "", "host", "", nil),
		ctr("side", "", "", "container:id-web", "", nil),
		ctr("nonet", "", "", "none", "", nil),
	}
	ch := Choices{
		"name:web":         ChoiceTunnel,
		"project:shop":     ChoiceTunnel,
		"service:shop/api": ChoiceHost, // явное «как компьютер» сильнее проекта
		"service:shop/lbl": ChoiceTunnel,
		"name:hostnet":     ChoiceTunnel, // не действует: сеть хоста
	}
	want := map[string]struct {
		eff        Choice
		from       string
		switchable bool
	}{
		"web":     {ChoiceTunnel, "container", true},
		"db":      {ChoiceTunnel, "project", true},
		"api":     {ChoiceHost, "container", true},
		"lbl":     {ChoiceDirect, "label", true},
		"hostnet": {ChoiceHost, "fixed", false},
		"side":    {ChoiceTunnel, "parent", false},
		"nonet":   {ChoiceHost, "fixed", false},
	}
	rows := Resolve(cs, ch)
	if len(rows) != len(want) {
		t.Fatalf("строк %d", len(rows))
	}
	for _, r := range rows {
		w := want[r.Name]
		if r.Effective != w.eff || r.From != w.from || r.Switchable != w.switchable {
			t.Errorf("%s: %s/%s/%v, ждали %s/%s/%v", r.Name, r.Effective, r.From, r.Switchable, w.eff, w.from, w.switchable)
		}
	}

	ts := Targets(rows)
	got := map[string]Choice{}
	for _, t := range ts {
		got[t.Addr.String()] = t.Route
	}
	wantT := map[string]Choice{"172.17.0.2": ChoiceTunnel, "172.18.0.2": ChoiceTunnel, "172.18.0.4": ChoiceDirect}
	if len(got) != len(wantT) {
		t.Fatalf("адреса: %v", got)
	}
	for a, c := range wantT {
		if got[a] != c {
			t.Errorf("%s: %s, ждали %s", a, got[a], c)
		}
	}

	ps := Projects(rows, ch)
	if len(ps) != 1 || ps[0].Name != "shop" || ps[0].Count != 3 || ps[0].Choice != ChoiceTunnel {
		t.Errorf("проекты: %+v", ps)
	}
}

func TestTargetsStopped(t *testing.T) {
	c := ctr("web", "", "", "bridge", "172.17.0.2", nil)
	c.State = "exited"
	if ts := Targets(Resolve([]Container{c}, Choices{"name:web": ChoiceTunnel})); len(ts) != 0 {
		t.Fatalf("у остановленного правил быть не должно: %v", ts)
	}
}

func has(p Plan, s string) bool {
	for _, r := range p.Rules {
		if r.String() == s {
			return true
		}
	}
	for _, r := range p.Routes {
		if r.String() == s {
			return true
		}
	}
	return false
}

func TestKernel(t *testing.T) {
	ts := []Target{{Addr: ip("172.17.0.2"), Route: ChoiceTunnel}, {Addr: ip("172.17.0.3"), Route: ChoiceDirect}}
	full := Net{Up: true, Full: true, Tun: "sshtun0", Uplink4: "eth0", Gw4: ip("192.168.1.1")}
	cases := []struct {
		name    string
		ts      []Target
		n       Net
		want    []string
		notWant []string
		empty   bool
	}{
		{name: "адаптер выключен — ничего", ts: ts, n: Net{}, empty: true},
		{name: "нет контейнеров — ничего", n: full, empty: true},
		{name: "туннель включён", ts: ts, n: full,
			want: []string{
				"v4 pref 9400 from 172.17.0.2/32 ipproto 17 dport 53 lookup 7095",
				"v4 pref 9400 from 172.17.0.2/32 ipproto 6 dport 53 lookup 7095",
				"v4 pref 9401 from 172.17.0.2/32 lookup 7094",
				"v4 pref 9401 from 172.17.0.3/32 lookup 7096",
				"v4 table 7094 throw 192.168.0.0/16",
				"v4 table 7094 dev 0.0.0.0/0 dev sshtun0",
				"v4 table 7094 unreachable 0.0.0.0/0 metric 4294967040",
				"v4 table 7095 dev 0.0.0.0/0 dev sshtun0",
				"v4 table 7095 unreachable 0.0.0.0/0 metric 4294967040",
				"v4 table 7096 throw 198.18.0.0/15",
				"v4 table 7096 via 0.0.0.0/0 via 192.168.1.1 dev eth0",
			},
			// DNS-таблица без throw: запрос к DNS роутера не уходит мимо.
			notWant: []string{"v4 table 7095 throw 192.168.0.0/16"}},
		{name: "туннель выключен — через сервер нет интернета", ts: ts,
			n: Net{Up: true, Tun: "sshtun0", Uplink4: "eth0", Gw4: ip("192.168.1.1")},
			want: []string{
				"v4 table 7094 dev 198.18.0.0/15 dev sshtun0",
				"v4 table 7094 unreachable 0.0.0.0/0 metric 4294967040",
				"v4 table 7095 unreachable 0.0.0.0/0 metric 4294967040",
			},
			notWant: []string{"v4 table 7094 dev 0.0.0.0/0 dev sshtun0", "v4 table 7095 dev 0.0.0.0/0 dev sshtun0"}},
		{name: "без настоящей сети «напрямую» уходит в main", ts: ts[1:], n: Net{Up: true, Full: true, Tun: "sshtun0"},
			want:    []string{"v4 table 7096 throw 10.0.0.0/8"},
			notWant: []string{"v4 table 7096 via 0.0.0.0/0 via 192.168.1.1 dev eth0"}},
		{name: "IPv6 без IPv6 у адаптера — только запрет",
			ts: []Target{{Addr: ip("fd00::2"), Route: ChoiceTunnel}}, n: full,
			want: []string{
				"v6 pref 9401 from fd00::2/128 lookup 7094",
				"v6 table 7094 unreachable ::/0 metric 4294967040",
				"v6 table 7094 throw fe80::/10",
			},
			notWant: []string{"v6 table 7094 dev ::/0 dev sshtun0"}},
	}
	for _, c := range cases {
		p := KernelPlan(c.ts, c.n)
		if c.empty {
			if !p.Empty() {
				t.Errorf("%s: ждали пустой план, а там %v", c.name, p)
			}
			continue
		}
		for _, w := range c.want {
			if !has(p, w) {
				t.Errorf("%s: нет %q\nплан: %v", c.name, w, p)
			}
		}
		for _, w := range c.notWant {
			if has(p, w) {
				t.Errorf("%s: лишнее %q", c.name, w)
			}
		}
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := OpenStore(dir)
	api := ctr("api", "shop", "api", "bridge", "", nil)
	web := ctr("web", "", "", "bridge", "", nil)
	if err := s.SetContainer(api, ChoiceDirect); err != nil {
		t.Fatal(err)
	}
	if err := s.SetContainer(web, ChoiceTunnel); err != nil {
		t.Fatal(err)
	}
	// Проект целиком снимает выбор его служб.
	if err := s.SetProject("shop", ChoiceTunnel); err != nil {
		t.Fatal(err)
	}
	got := OpenStore(dir).Get()
	if got["service:shop/api"] != "" || got["project:shop"] != ChoiceTunnel || got["name:web"] != ChoiceTunnel {
		t.Fatalf("выбор: %v", got)
	}
	// «Как компьютер» при «через сервер» у проекта хранится явно.
	_ = s.SetContainer(api, ChoiceHost)
	if OpenStore(dir).Get()["service:shop/api"] != ChoiceHost {
		t.Fatal("явное «как компьютер» потерялось")
	}
	// Без выбора у проекта — просто убирается.
	_ = s.SetContainer(web, ChoiceHost)
	if _, ok := OpenStore(dir).Get()["name:web"]; ok {
		t.Fatal("«как компьютер» без проекта не должно храниться")
	}
}

func TestClientList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[
 {"Id":"aaa","Names":["/shop-api-1"],"Image":"api:1","State":"running",
  "Labels":{"com.docker.compose.project":"shop","com.docker.compose.service":"api"},
  "HostConfig":{"NetworkMode":"shop_default"},
  "NetworkSettings":{"Networks":{"shop_default":{"IPAddress":"172.18.0.3","GlobalIPv6Address":""}}}},
 {"Id":"bbb","Names":["/side"],"Image":"x","State":"running","Labels":{},
  "HostConfig":{"NetworkMode":"container:aaa"},"NetworkSettings":{"Networks":{}}},
 {"Id":"ccc","Names":["/h"],"Image":"x","State":"exited","Labels":{},
  "HostConfig":{"NetworkMode":"host"},"NetworkSettings":{"Networks":{"host":{"IPAddress":""}}}}
]`))
	}))
	defer srv.Close()
	cs, err := newHTTPClient(srv.URL).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 {
		t.Fatalf("контейнеров %d", len(cs))
	}
	a := cs[0]
	if a.Name != "shop-api-1" || a.Project != "shop" || a.Service != "api" || a.NetMode != "bridge" ||
		len(a.Networks) != 1 || a.Networks[0].IP != ip("172.18.0.3") || a.Key() != "service:shop/api" {
		t.Errorf("api: %+v", a)
	}
	if cs[1].NetMode != "container" || cs[1].Parent != "aaa" {
		t.Errorf("side: %+v", cs[1])
	}
	if cs[2].NetMode != "host" {
		t.Errorf("h: %+v", cs[2])
	}
	rows := Resolve(cs, Choices{"service:shop/api": ChoiceTunnel})
	for _, r := range rows {
		if r.Name == "side" && r.Effective != ChoiceTunnel {
			t.Errorf("side должен идти как shop-api-1: %+v", r)
		}
	}
}

func TestInteresting(t *testing.T) {
	for _, c := range []struct {
		typ, act string
		want     bool
	}{
		{"container", "start", true}, {"container", "die", true}, {"network", "connect", true},
		{"network", "disconnect", true}, {"container", "exec_start: sh -c x", false}, {"network", "create", false},
	} {
		if interesting(c.typ, c.act) != c.want {
			t.Errorf("%s %s", c.typ, c.act)
		}
	}
}
