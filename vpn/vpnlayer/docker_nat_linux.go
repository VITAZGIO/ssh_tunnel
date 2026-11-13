package vpnlayer

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// Контейнеры «через сервер» идут в адаптер со своим адресом источника: Docker
// подменяет его на адрес адаптера правилом MASQUERADE в таблице nat
// (POSTROUTING, «-s <сеть> ! -o docker0»). Наша цепочка стоит первой в
// POSTROUTING и для пакетов в адаптер с адресов таких контейнеров говорит
// ACCEPT — дальше по таблице nat пакет не идёт, подмены нет.
//
// Через программу iptables (её ставит сам Docker): Docker пишет свои правила
// ею же, и только так наше правило оказывается в той же цепочке раньше
// MASQUERADE. Docker с «firewall-backend: nftables» (новый, по выбору)
// подменяет адрес в своей таблице nftables — там исключение не действует,
// стек тогда видит адрес адаптера и строгий режим не включается (остаётся
// прежний «через сервер» по правилам ядра).

const natChain = "SSH_TUNNEL_DOCKER"

// natTool — программа для семейства.
func natTool(v6 bool) string {
	if v6 {
		return "ip6tables"
	}
	return "iptables"
}

// natPlan — команды (аргументы после «-w -t nat»), которые приводят цепочку и
// переход на неё к want. chain — вывод «-S natChain» (nil — цепочки нет),
// post — вывод «-S POSTROUTING».
func natPlan(chain, post []string, want []netip.Addr, tun string, v6 bool) [][]string {
	bits := "/32"
	if v6 {
		bits = "/128"
	}
	jump := "-o " + tun + " -j " + natChain
	var cmds [][]string
	if len(want) == 0 {
		for _, l := range post {
			if strings.TrimPrefix(l, "-A POSTROUTING ") == jump {
				cmds = append(cmds, append([]string{"-D", "POSTROUTING"}, strings.Fields(jump)...))
			}
		}
		if chain != nil {
			cmds = append(cmds, []string{"-F", natChain}, []string{"-X", natChain})
		}
		return cmds
	}
	if chain == nil {
		cmds = append(cmds, []string{"-N", natChain})
	}
	have := map[string]bool{}
	for _, l := range chain {
		if r, ok := strings.CutPrefix(l, "-A "+natChain+" "); ok {
			have[r] = true
		}
	}
	wantSet := map[string]bool{}
	for _, a := range want {
		r := "-s " + a.String() + bits + " -j ACCEPT"
		wantSet[r] = true
		if !have[r] {
			cmds = append(cmds, append([]string{"-A", natChain}, strings.Fields(r)...))
		}
	}
	for r := range have {
		if !wantSet[r] {
			cmds = append(cmds, append([]string{"-D", natChain}, strings.Fields(r)...))
		}
	}
	// Переход — первым в POSTROUTING: Docker дописывает свои правила в конец,
	// но после перезапуска мог встать и раньше.
	var rules []string
	for _, l := range post {
		if r, ok := strings.CutPrefix(l, "-A POSTROUTING "); ok {
			rules = append(rules, r)
		}
	}
	first := len(rules) > 0 && rules[0] == jump
	n := 0
	for _, r := range rules {
		if r == jump {
			n++
		}
	}
	if !first || n > 1 {
		for i := 0; i < n; i++ {
			cmds = append(cmds, append([]string{"-D", "POSTROUTING"}, strings.Fields(jump)...))
		}
		cmds = append(cmds, append([]string{"-I", "POSTROUTING", "1"}, strings.Fields(jump)...))
	}
	return cmds
}

func natList(tool, chain string) ([]string, bool) {
	out, err := exec.Command(tool, "-w", "-t", "nat", "-S", chain).Output()
	if err != nil {
		return nil, false
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), true
}

// applyNAT приводит исключения из подмены адреса к forced.
func applyNAT(forced []netip.Addr, tun string) error {
	var errs []error
	for _, v6 := range []bool{false, true} {
		var want []netip.Addr
		for _, a := range forced {
			if a.Is6() == v6 {
				want = append(want, a)
			}
		}
		tool := natTool(v6)
		if _, err := exec.LookPath(tool); err != nil {
			if len(want) > 0 {
				errs = append(errs, fmt.Errorf("нет программы %s: контейнеры «через сервер» идут в адаптер с подменённым адресом — «всегда напрямую» к ним применяется", tool))
			}
			continue
		}
		chain, ok := natList(tool, natChain)
		if !ok {
			chain = nil
		} else if chain == nil {
			chain = []string{}
		}
		post, _ := natList(tool, "POSTROUTING")
		for _, c := range natPlan(chain, post, want, tun, v6) {
			args := append([]string{"-w", "-t", "nat"}, c...)
			if out, err := exec.Command(tool, args...).CombinedOutput(); err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %v %s", tool, strings.Join(c, " "), err, bytes.TrimSpace(out)))
			}
		}
	}
	return errors.Join(errs...)
}
