package lanscan

import (
	"net"
	"net/netip"
	"testing"
)

func TestTargets(t *testing.T) {
	p := netip.MustParsePrefix("192.168.1.50/24")
	l := targets(p)
	if len(l) != 253 || l[0].String() != "192.168.1.1" || l[252].String() != "192.168.1.254" {
		t.Fatalf("%d %v %v", len(l), l[0], l[len(l)-1])
	}
	for _, a := range l {
		if a == p.Addr() {
			t.Fatal("свой адрес в списке")
		}
	}
	// Большая сеть — только своя /24.
	if n := len(targets(netip.MustParsePrefix("10.1.2.3/16"))); n != 253 {
		t.Fatalf("/16: %d", n)
	}
}

func TestVendor(t *testing.T) {
	mac, _ := net.ParseMAC("34:98:7a:cf:76:53")
	if v := Vendor(mac); v != "Espressif" {
		t.Fatalf("ESP: %q", v)
	}
	rnd, _ := net.ParseMAC("32:6b:1d:9d:2b:b1")
	if !RandomMAC(rnd) || Vendor(rnd) != "" {
		t.Fatal("случайный MAC")
	}
}

func TestLanAddr(t *testing.T) {
	for s, want := range map[string]bool{"192.168.1.5/24": true, "10.0.0.2/8": false, "198.19.0.5/16": false,
		"100.64.1.1/10": false, "172.16.5.4/20": true, "8.8.8.8/24": false} {
		if got := lanAddr(netip.MustParsePrefix(s)); got != want {
			t.Errorf("%s: %v", s, got)
		}
	}
}

func TestPTR(t *testing.T) {
	a := netip.MustParseAddr("192.168.1.105")
	q := ptrQuery(7, a)
	// Ответ: тот же вопрос + ответ PTR со сжатым владельцем.
	m := append([]byte{0, 7, 0x84, 0, 0, 1, 0, 1, 0, 0, 0, 0}, q[12:]...)
	m = append(m, 0xc0, 12, 0, 12, 0, 1, 0, 0, 0, 120)
	name := []byte{7, 'k', 'i', 't', 'c', 'h', 'e', 'n', 5, 'l', 'o', 'c', 'a', 'l', 0}
	m = append(m, 0, byte(len(name)))
	m = append(m, name...)
	ip, n := parsePTR(m)
	if ip != a || n != "kitchen.local" {
		t.Fatalf("%v %q", ip, n)
	}
}
