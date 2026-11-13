package panel

// Выход самого сервера через другой сервер (ТЗ 10): план маршрутов.
//
// Сервер A ставит у себя Linux-клиент ssh_tunnel в режиме VPN, а тот кладёт
// маршруты всего интернета (0/1, 128/1, ::/1, 8000::/1 в sshtun0) не в main,
// а в свою таблицу egressTable (переменная SSH_TUNNEL_ROUTE_TABLE, см.
// vpn/vpnlayer/sys_linux.go). Куда пойдёт пакет, решают правила ip rule
// этого плана — main при этом не трогается вовсе.
//
// Порядок правил (меньше — раньше):
//
//	9300 fwmark 0x7097 lookup main        ответы на соединения, пришедшие снаружи
//	9301 from <адрес карты> lookup main    то же для своих сокетов с этим адресом
//	9302 to <B, главный сервер сети> lookup main   связь с B и с главным meshd
//	9303 oif <карта> lookup main           сокеты, привязанные к карте (сам клиент)
//	9400/9401 — правила контейнеров плагина Docker (docker_linux.go), не наши
//	9500 lookup main suppress_prefixlength 0   локальные сети, мосты, точечные маршруты
//	9501 lookup 7098                       всё остальное — в туннель к B
//	9502 unreachable                       «сервер без интернета», пока B нет
//
// Метка 0x7097 ставится в nftables: пакет пришёл с внешней карты в прямом
// направлении соединения — значит, соединение начал кто-то снаружи (SSH,
// панель, пользователи туннеля, опубликованные порты контейнеров). Метка
// живёт в conntrack и переносится на каждый пакет этого соединения, в том
// числе на ответы контейнера (FORWARD) и на ответы своих сокетов (хук
// output типа route — после него ядро заново выбирает маршрут).
//
// Почему переживает строгий rp_filter: обратная проверка входящего пакета
// ищет маршрут к его отправителю с адресом получателя как источником. Для
// своих адресов срабатывает 9301 (main → та же карта). Для пакетов к
// контейнерам после DNAT адрес получателя уже контейнерный — здесь
// работает 9300, для этого включается net.ipv4.conf.all.src_valid_mark=1
// (иначе метка в обратной проверке не учитывается). Прежнее значение
// сохраняется и возвращается при выключении.

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

const (
	egressMark  = 0x7097
	egressTable = 7098
	egressNFT   = "ssh_tunnel_egress"

	prefEgressReplyMark = 9300
	prefEgressReplyFrom = 9301
	prefEgressDirectTo  = 9302
	prefEgressOif       = 9303
	prefEgressMain      = 9500
	prefEgressTunnel    = 9501
	prefEgressBlock     = 9502
)

// egressPrefs — все приоритеты наших правил: по ним они снимаются целиком,
// без знания плана (откат после перезагрузки или падения).
var egressPrefs = []int{prefEgressReplyMark, prefEgressReplyFrom, prefEgressDirectTo,
	prefEgressOif, prefEgressMain, prefEgressTunnel, prefEgressBlock}

// Каталоги на сервере. Не в папке данных панели: их пишут скрипты от root
// (под systemd-run), а читают службы systemd при загрузке.
const (
	egressVarDir  = "/var/lib/ssh_tunnel_egress"
	egressBinDir  = "/usr/local/lib/ssh_tunnel_egress"
	egressBinPath = egressBinDir + "/ssh_tunnel_vpn_linux"

	egressUnit         = "ssh_tunnel_egress.service"
	egressRulesUnit    = "ssh_tunnel_egress-rules.service"
	egressRollbackUnit = "ssh_tunnel_egress-rollback.service"
	egressRollbackTmr  = "ssh_tunnel_egress-rollback.timer"

	// egressWindow — сколько ждать подтверждения, что сервер доступен.
	egressWindowSec = 60
)

// egressFacts — что известно о сети сервера в момент применения.
type egressFacts struct {
	// Uplinks — карты с маршрутом по умолчанию в main (без sshtun0).
	Uplinks []string
	// Local — адреса этих карт (глобальные, v4 и v6).
	Local []netip.Addr
	// Direct — куда всегда напрямую: сам B и главный сервер сети устройств.
	Direct []netip.Addr
}

// egressRule — одно правило ip rule.
type egressRule struct {
	V6   bool
	Pref int
	Sel  string // всё после «pref N»
}

func (r egressRule) String() string {
	fam := "-4"
	if r.V6 {
		fam = "-6"
	}
	return fmt.Sprintf("ip %s rule add pref %d %s", fam, r.Pref, r.Sel)
}

// egressPlan — что поставить.
type egressPlan struct {
	Rules   []egressRule
	Uplinks []string
	Block   bool
}

var ifNameRe = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,15}$`)

// planEgress — план маршрутов по фактам. block — «сервер без интернета»,
// пока B недоступен; иначе — выход напрямую.
func planEgress(f egressFacts, block bool) (egressPlan, error) {
	if len(f.Uplinks) == 0 {
		return egressPlan{}, errors.New("не нашёл, через какую карту сервер ходит в интернет (нет маршрута по умолчанию)")
	}
	ups := append([]string(nil), f.Uplinks...)
	sort.Strings(ups)
	ups = dedupStrings(ups)
	for _, u := range ups {
		if !ifNameRe.MatchString(u) || strings.HasPrefix(u, "-") {
			return egressPlan{}, fmt.Errorf("странное имя сетевой карты %q", u)
		}
		if u == "sshtun0" {
			return egressPlan{}, errors.New("маршрут по умолчанию уже идёт в sshtun0 — на сервере работает другой клиент ssh_tunnel")
		}
	}
	p := egressPlan{Uplinks: ups, Block: block}
	add := func(v6 bool, pref int, sel string) {
		p.Rules = append(p.Rules, egressRule{V6: v6, Pref: pref, Sel: sel})
	}
	mark := fmt.Sprintf("fwmark 0x%x lookup main", egressMark)
	add(false, prefEgressReplyMark, mark)
	add(true, prefEgressReplyMark, mark)
	for _, a := range sortedAddrs(f.Local) {
		add(a.Is6(), prefEgressReplyFrom, "from "+a.String()+" lookup main")
	}
	for _, a := range sortedAddrs(f.Direct) {
		add(a.Is6(), prefEgressDirectTo, "to "+a.String()+" lookup main")
	}
	for _, u := range ups {
		add(false, prefEgressOif, "oif "+u+" lookup main")
		add(true, prefEgressOif, "oif "+u+" lookup main")
	}
	for _, v6 := range []bool{false, true} {
		add(v6, prefEgressMain, "lookup main suppress_prefixlength 0")
		add(v6, prefEgressTunnel, fmt.Sprintf("lookup %d", egressTable))
		if block {
			add(v6, prefEgressBlock, "unreachable")
		}
	}
	return p, nil
}

func sortedAddrs(in []netip.Addr) []netip.Addr {
	seen := map[netip.Addr]bool{}
	var out []netip.Addr
	for _, a := range in {
		a = a.Unmap()
		if !a.IsValid() || a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}

func dedupStrings(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// nftRuleset — метка соединений, начатых снаружи.
func (p egressPlan) nftRuleset() string {
	q := make([]string, len(p.Uplinks))
	for i, u := range p.Uplinks {
		q[i] = `"` + u + `"`
	}
	return fmt.Sprintf(`table inet %[1]s {
	chain pre {
		type filter hook prerouting priority mangle; policy accept;
		iifname { %[2]s } ct direction original ct mark set 0x%[3]x
		ct mark 0x%[3]x meta mark set ct mark
	}
	chain out {
		type route hook output priority mangle; policy accept;
		ct mark 0x%[3]x meta mark set ct mark
	}
}
`, egressNFT, strings.Join(q, ", "), egressMark)
}

// rulesDownScript — снять наши правила, метки и sysctl. Не зависит от плана:
// по приоритетам. Таблицу egressTable не трогает — её маршруты ставит и
// снимает клиент; чистит её off-скрипт после остановки клиента.
func rulesDownScript() string {
	prefs := make([]string, len(egressPrefs))
	for i, p := range egressPrefs {
		prefs[i] = fmt.Sprint(p)
	}
	return fmt.Sprintf(`for fam in -4 -6; do
  for p in %s; do
    while ip $fam rule del pref $p 2>/dev/null; do :; done
  done
done
nft delete table inet %s 2>/dev/null || true
if [ -f %s/src_valid_mark.saved ]; then
  sysctl -qw net.ipv4.conf.all.src_valid_mark="$(cat %s/src_valid_mark.saved)" || true
  rm -f %s/src_valid_mark.saved
fi
`, strings.Join(prefs, " "), egressNFT, egressVarDir, egressVarDir, egressVarDir)
}

// upScript — поставить план. Сначала снимает прежние свои правила: скрипт
// можно звать повторно (смена режима, перезапуск службы правил).
func (p egressPlan) upScript() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# ssh_tunnel: правила выхода сервера через другой сервер. Пишет панель — руками не править.\nset -eu\n")
	b.WriteString(rulesDownScript())
	fmt.Fprintf(&b, `mkdir -p %[1]s
[ -f %[1]s/src_valid_mark.saved ] || cat /proc/sys/net/ipv4/conf/all/src_valid_mark > %[1]s/src_valid_mark.saved
sysctl -qw net.ipv4.conf.all.src_valid_mark=1
nft -f - <<'EOF_NFT'
%[2]sEOF_NFT
`, egressVarDir, p.nftRuleset())
	for _, r := range p.Rules {
		if r.V6 {
			// IPv6 бывает выключен целиком — без него, но v4 обязателен.
			b.WriteString(r.String() + " 2>/dev/null || true\n")
		} else {
			b.WriteString(r.String() + "\n")
		}
	}
	return b.String()
}
