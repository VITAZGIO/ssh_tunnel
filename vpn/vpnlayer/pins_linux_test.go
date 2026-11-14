package vpnlayer

import (
	"net/netip"
	"reflect"
	"testing"
)

// Правила маршрутов «напрямую»: ставятся по адресам карты, лишние снимаются.
func TestPinRulePlan(t *testing.T) {
	p := func(s string) netip.Prefix { return netip.MustParsePrefix(s) }
	add, del := pinRulePlan(nil, []netip.Prefix{p("192.168.50.2/32"), p("2001:db8::2/128")})
	if !reflect.DeepEqual(add, []netip.Prefix{p("192.168.50.2/32"), p("2001:db8::2/128")}) || del != nil {
		t.Fatalf("с нуля: add %v del %v", add, del)
	}
	// Сеть сменилась — новый адрес до снятия старого.
	add, del = pinRulePlan([]netip.Prefix{p("192.168.50.2/32")}, []netip.Prefix{p("10.0.0.5/32")})
	if !reflect.DeepEqual(add, []netip.Prefix{p("10.0.0.5/32")}) || !reflect.DeepEqual(del, []netip.Prefix{p("192.168.50.2/32")}) {
		t.Fatalf("смена сети: add %v del %v", add, del)
	}
	// Выключено — снять всё; стоит как надо — ничего.
	if add, del = pinRulePlan([]netip.Prefix{p("192.168.50.2/32")}, nil); add != nil || len(del) != 1 {
		t.Fatalf("снять: add %v del %v", add, del)
	}
	if add, del = pinRulePlan([]netip.Prefix{p("192.168.50.2/32")}, []netip.Prefix{p("192.168.50.2/32")}); add != nil || del != nil {
		t.Fatalf("лишнее: add %v del %v", add, del)
	}
	r := pinRule(p("192.168.50.2/32"))
	if r.Priority != pinPrio || r.Table != pinTable || r.Src.String() != "192.168.50.2/32" {
		t.Fatalf("правило: %+v", r)
	}
}
