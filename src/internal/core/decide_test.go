package core

import (
	"testing"

	"sshtunnel/internal/routing"
)

func TestDecide(t *testing.T) {
	except := routing.New(routing.ModeExcept, []string{"netbird.exe", "firefox.exe"})
	only := routing.New(routing.ModeOnly, []string{"chrome.exe"})
	direct := routing.NewDirectList([]string{".netbird.io", "25.*", "example.org"})
	tunneled := routing.NewDirectList([]string{"api.example.org"})

	bypass := Rules{Bypass: true, Mesh: true, Policy: except, Direct: direct, Tunneled: tunneled}
	cases := []struct {
		name string
		r    Rules
		c    Conn
		want Route
	}{
		{"сайт через сервер", bypass, Conn{Target: "youtube.com:443"}, RouteServer},
		{"неизвестная программа через сервер", bypass, Conn{Process: "", Target: "1.2.3.4:443"}, RouteServer},
		{"исключённая программа напрямую", bypass, Conn{Process: "netbird.exe", Target: "api.netbird.cloud:443"}, RouteDirect},
		{"исключённая программа с путём", bypass, Conn{Process: `C:\Program Files\NetBird\NetBird.exe`, Target: "1.2.3.4:443"}, RouteDirect},
		{"«только выбранные»: не выбранная напрямую", Rules{Bypass: true, Policy: only}, Conn{Process: "steam.exe", Target: "1.2.3.4:443"}, RouteDirect},
		{"«только выбранные»: выбранная через сервер", Rules{Bypass: true, Policy: only}, Conn{Process: "chrome.exe", Target: "1.2.3.4:443"}, RouteServer},
		{"локальная сеть напрямую", bypass, Conn{Target: "192.168.1.200:8006"}, RouteDirect},
		{"локальная сеть через сервер по галочке", Rules{Bypass: true, LocalViaTunnel: true}, Conn{Target: "192.168.1.200:8006"}, RouteServer},
		{"«всегда напрямую» по имени", bypass, Conn{Target: "api.netbird.io:443"}, RouteDirect},
		{"«всегда напрямую» по шаблону адреса", bypass, Conn{Target: "25.1.2.3:80"}, RouteDirect},
		{"«всегда напрямую» по выученному имени", bypass, Conn{Target: "93.184.216.34:443", Name: "example.org"}, RouteDirect},
		{"«всегда через сервер» сильнее «напрямую»", bypass, Conn{Process: "netbird.exe", Target: "api.example.org:443"}, RouteServer},
		{"сеть устройств по адресу", bypass, Conn{Target: "198.19.0.5:3389"}, RouteMesh},
		{"сеть устройств по имени", bypass, Conn{Target: "ноутбук.mesh:22"}, RouteMesh},
		{"сеть устройств выключена — отказ", Rules{Bypass: true}, Conn{Target: "198.19.0.5:3389"}, RouteBlock},
		{"сеть устройств раньше «напрямую»", Rules{Bypass: false, Mesh: true}, Conn{Target: "pc.mesh:3389"}, RouteMesh},
		{"обход выключен — всё напрямую", Rules{Bypass: false, Mesh: true, Tunneled: tunneled}, Conn{Target: "api.example.org:443"}, RouteDirect},
		{"слив — напрямую", Rules{Bypass: true, Draining: true}, Conn{Target: "youtube.com:443"}, RouteDirect},
	}
	for _, c := range cases {
		if got := Decide(c.r, c.c); got != c.want {
			t.Errorf("%s: %s, ожидалось %s", c.name, got, c.want)
		}
	}
}

func TestDecideDNS(t *testing.T) {
	r := Rules{Bypass: true, Direct: routing.NewDirectList([]string{".netbird.io"}), Tunneled: routing.NewDirectList([]string{"x.netbird.io"})}
	cases := map[string]DNSAnswer{
		"youtube.com":          DNSFake,
		"api.netbird.io":       DNSReal,
		"x.netbird.io":         DNSFake, // «всегда через сервер» сильнее
		"nas":                  DNSReal, // имя без точки — локальное
		"printer.local":        DNSReal,
		"ноутбук.mesh.":        DNSMesh,
		"Server.MESH":          DNSMesh,
		"homeassistant.ts.net": DNSReal,
	}
	for name, want := range cases {
		if got := DecideDNS(r, name); got != want {
			t.Errorf("%s: %d, ожидалось %d", name, got, want)
		}
	}
	if got := DecideDNS(Rules{Mesh: true}, "youtube.com"); got != DNSReal {
		t.Errorf("без обхода имена должны разрешаться по-настоящему, получено %d", got)
	}
}

func TestPlanVPN(t *testing.T) {
	p := PlanVPN(Modes{Mesh: true}, PlanOptions{IPv6: true, ShareWanted: true})
	if len(p.Routes) != 0 || p.DNSAll || p.PinServer || p.Share || len(p.DNSDomains) != 1 {
		t.Errorf("«только сеть устройств»: %+v", p)
	}
	p = PlanVPN(Modes{Bypass: true}, PlanOptions{IPv6: false, ShareWanted: true})
	if len(p.Routes) != 2 || !p.DNSAll || !p.PinServer || !p.PinDirect || !p.Share {
		t.Errorf("«Обход» без IPv6: %+v", p)
	}
	p = PlanVPN(Modes{Bypass: true, Mesh: true}, PlanOptions{IPv6: true})
	if len(p.Routes) != 4 || p.Share {
		t.Errorf("«Обход + сеть» с IPv6: %+v", p)
	}
	for _, r := range p.Routes {
		if r.Contains(DNSAddr) && r.Bits() > 1 {
			t.Errorf("маршрут %s точнее подсети адаптера", r)
		}
	}
	if !AdapterPrefix4.Contains(DNSAddr) || !AdapterPrefix4.Masked().Contains(FakeNet.Addr()) {
		t.Error("DNS и подставные адреса должны лежать в подсети адаптера")
	}
}
