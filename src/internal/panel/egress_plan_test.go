package panel

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("netip.ParseAddr(%q): %v", s, err)
	}
	return a
}

func TestPlanEgressБезКартыОшибка(t *testing.T) {
	if _, err := planEgress(egressFacts{}, true); err == nil {
		t.Fatal("без карт по умолчанию план не должен строиться")
	}
}

func TestPlanEgressОтказываетНаSshtun0(t *testing.T) {
	_, err := planEgress(egressFacts{Uplinks: []string{"sshtun0"}}, true)
	if err == nil || !strings.Contains(err.Error(), "другой клиент") {
		t.Fatalf("ожидался отказ про другой клиент, получено: %v", err)
	}
}

func TestPlanEgressПорядокИСодержаниеПравил(t *testing.T) {
	f := egressFacts{
		Uplinks: []string{"eth0", "eth0"}, // повтор — должен слипнуться
		Local:   []netip.Addr{mustAddr(t, "203.0.113.5"), mustAddr(t, "127.0.0.1"), mustAddr(t, "203.0.113.5")},
		Direct:  []netip.Addr{mustAddr(t, "198.51.100.9")},
	}
	p, err := planEgress(f, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Uplinks) != 1 || p.Uplinks[0] != "eth0" {
		t.Fatalf("карты не слиплись: %v", p.Uplinks)
	}
	// В каждой из двух таблиц правил ядра (v4 и v6 — у них разные
	// пространства pref) приоритеты должны идти по неубыванию, иначе
	// правила встанут не в том порядке, в котором их читает ядро.
	var lastV4, lastV6 = -1, -1
	for i, r := range p.Rules {
		last := &lastV4
		if r.V6 {
			last = &lastV6
		}
		if r.Pref < *last {
			t.Fatalf("правило %d (pref %d, v6=%v) идёт раньше другого правила той же семьи с большим pref (%d)", i, r.Pref, r.V6, *last)
		}
		*last = r.Pref
	}
	joined := func() string {
		var ss []string
		for _, r := range p.Rules {
			ss = append(ss, r.String())
		}
		return strings.Join(ss, "\n")
	}()
	for _, want := range []string{
		"fwmark 0x7097 lookup main",
		"from 203.0.113.5 lookup main",
		"to 198.51.100.9 lookup main",
		"oif eth0 lookup main",
		"lookup main suppress_prefixlength 0",
		"lookup 7098",
		"unreachable",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("в плане нет правила %q:\n%s", want, joined)
		}
	}
	// Локальный и loopback-адрес не должны попасть в правила from/to.
	if strings.Contains(joined, "127.0.0.1") {
		t.Error("loopback не должен попасть в правила")
	}
}

func TestPlanEgressНаправленииБезBlockНетUnreachable(t *testing.T) {
	f := egressFacts{Uplinks: []string{"eth0"}}
	p, err := planEgress(f, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range p.Rules {
		if strings.Contains(r.Sel, "unreachable") {
			t.Fatalf("в режиме «выход напрямую» не должно быть unreachable: %+v", r)
		}
	}
}

func TestEgressPhase(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		st   egressSysState
		want string
	}{
		{"выключено", egressSysState{}, EgressOff},
		{"применяется", egressSysState{Pending: now.Unix() - 10}, EgressApplying},
		{"окно истекло", egressSysState{Pending: now.Unix() - egressWindowSec - 5}, EgressRollback},
		{"работает", egressSysState{RulesActive: true, RulesEnabled: true, ClientActive: true, RoutesUp: true}, EgressOn},
		{"B недоступен", egressSysState{RulesActive: true, RulesEnabled: true, ClientActive: true, RoutesUp: false}, EgressDown},
		{"клиент не поднялся", egressSysState{RulesActive: true, RulesEnabled: true}, EgressDown},
	}
	for _, c := range cases {
		if got := egressPhase(c.st, now); got != c.want {
			t.Errorf("%s: egressPhase() = %q, ожидалось %q", c.name, got, c.want)
		}
	}
}

func TestCheckEgressLoop(t *testing.T) {
	if err := checkEgressLoop("203.0.113.1", "203.0.113.1", nil); err == nil {
		t.Error("совпадение self/via должно быть петлёй")
	}
	if err := checkEgressLoop("203.0.113.1", "203.0.113.5", []netip.Addr{mustAddr(t, "203.0.113.5")}); err == nil {
		t.Error("via в числе своих адресов должно быть петлёй")
	}
	if err := checkEgressLoop("203.0.113.1", "198.51.100.9", []netip.Addr{mustAddr(t, "203.0.113.5")}); err != nil {
		t.Errorf("петли нет, а ошибка есть: %v", err)
	}
	if err := checkEgressLoop("203.0.113.1", "мусор", nil); err == nil {
		t.Error("битый адрес via должен быть ошибкой")
	}
}
