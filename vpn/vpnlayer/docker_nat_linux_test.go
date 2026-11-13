package vpnlayer

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func joinCmds(cs [][]string) []string {
	var out []string
	for _, c := range cs {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func TestNATPlan(t *testing.T) {
	a := netip.MustParseAddr("172.17.0.2")
	b := netip.MustParseAddr("172.17.0.3")
	dockerPost := []string{"-P POSTROUTING ACCEPT", "-A POSTROUTING -s 172.17.0.0/16 ! -o docker0 -j MASQUERADE"}

	got := joinCmds(natPlan(nil, dockerPost, []netip.Addr{a}, "sshtun0", false))
	want := []string{
		"-N SSH_TUNNEL_DOCKER",
		"-A SSH_TUNNEL_DOCKER -s 172.17.0.2/32 -j ACCEPT",
		"-I POSTROUTING 1 -o sshtun0 -j SSH_TUNNEL_DOCKER",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("с нуля:\n%q\nждали\n%q", got, want)
	}

	// Стоит как надо — ничего не делать; сменился адрес — добавить новый
	// раньше, чем снять старый.
	chain := []string{"-N SSH_TUNNEL_DOCKER", "-A SSH_TUNNEL_DOCKER -s 172.17.0.2/32 -j ACCEPT"}
	post := append([]string{"-P POSTROUTING ACCEPT", "-A POSTROUTING -o sshtun0 -j SSH_TUNNEL_DOCKER"}, dockerPost[1:]...)
	if c := natPlan(chain, post, []netip.Addr{a}, "sshtun0", false); len(c) != 0 {
		t.Fatalf("лишние команды: %q", joinCmds(c))
	}
	got = joinCmds(natPlan(chain, post, []netip.Addr{b}, "sshtun0", false))
	want = []string{"-A SSH_TUNNEL_DOCKER -s 172.17.0.3/32 -j ACCEPT", "-D SSH_TUNNEL_DOCKER -s 172.17.0.2/32 -j ACCEPT"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("смена адреса: %q", got)
	}

	// Docker после перезапуска встал раньше — переход снова первым.
	late := []string{"-P POSTROUTING ACCEPT", dockerPost[1], "-A POSTROUTING -o sshtun0 -j SSH_TUNNEL_DOCKER"}
	got = joinCmds(natPlan(chain, late, []netip.Addr{a}, "sshtun0", false))
	want = []string{"-D POSTROUTING -o sshtun0 -j SSH_TUNNEL_DOCKER", "-I POSTROUTING 1 -o sshtun0 -j SSH_TUNNEL_DOCKER"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("переход не первым: %q", got)
	}

	// Никого — убрать всё.
	got = joinCmds(natPlan(chain, post, nil, "sshtun0", false))
	want = []string{"-D POSTROUTING -o sshtun0 -j SSH_TUNNEL_DOCKER", "-F SSH_TUNNEL_DOCKER", "-X SSH_TUNNEL_DOCKER"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("снять: %q", got)
	}
	if c := natPlan(nil, dockerPost, nil, "sshtun0", false); len(c) != 0 {
		t.Fatalf("нечего снимать, а команды: %q", joinCmds(c))
	}
	got = joinCmds(natPlan(nil, nil, []netip.Addr{netip.MustParseAddr("fd00::2")}, "sshtun0", true))
	if got[1] != "-A SSH_TUNNEL_DOCKER -s fd00::2/128 -j ACCEPT" {
		t.Fatalf("IPv6: %q", got)
	}
}
