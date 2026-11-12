package vpnlayer

import (
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"sshtunnel/internal/docker"
)

// План Docker → netlink и обратно в ключ: без root и без ядра.
func TestDockerPlanToNetlink(t *testing.T) {
	ifs := map[string]int{"sshtun0": 7, "eth0": 2}
	idx := func(n string) int { return ifs[n] }
	p := docker.KernelPlan([]docker.Target{
		{Addr: netip.MustParseAddr("172.17.0.2"), Route: docker.ChoiceTunnel},
		{Addr: netip.MustParseAddr("172.17.0.3"), Route: docker.ChoiceDirect},
	}, docker.Net{Up: true, Full: true, Tun: "sshtun0", Uplink4: "eth0", Gw4: netip.MustParseAddr("192.168.1.1")})

	seen := map[string]bool{}
	for _, r := range p.Routes {
		nr, err := toNetlinkRoute(r, idx)
		if err != nil {
			t.Fatalf("%s: %v", r, err)
		}
		if nr.Protocol != rtProtoOurs || nr.Table != r.Table {
			t.Errorf("%s: метка или таблица: %+v", r, nr)
		}
		switch r.Kind {
		case docker.KindThrow:
			if nr.Type != unix.RTN_THROW || nr.LinkIndex != 0 {
				t.Errorf("%s: %+v", r, nr)
			}
		case docker.KindUnreachable:
			if nr.Type != unix.RTN_UNREACHABLE || nr.Priority == 0 {
				t.Errorf("%s: %+v", r, nr)
			}
		case docker.KindDev:
			if nr.LinkIndex != 7 || nr.Scope != netlink.SCOPE_LINK {
				t.Errorf("%s: %+v", r, nr)
			}
		case docker.KindVia:
			if nr.LinkIndex != 2 || nr.Gw.String() != "192.168.1.1" {
				t.Errorf("%s: %+v", r, nr)
			}
		}
		k := routeKey(*nr)
		if seen[k] {
			t.Errorf("два маршрута с одним ключом %s", k)
		}
		seen[k] = true
	}
	if _, err := toNetlinkRoute(docker.Route{Family: 4, Table: docker.TableTunnel, Kind: docker.KindDev, Dev: "нет", Dst: netip.MustParsePrefix("0.0.0.0/0")}, idx); err == nil {
		t.Error("неизвестная карта должна быть ошибкой")
	}
	for _, r := range p.Rules {
		nr := toNetlinkRule(r)
		if got := ruleKey(nr.Family, *nr); got != r.String() {
			t.Errorf("ключ правила %q, ждали %q", got, r.String())
		}
	}
}
