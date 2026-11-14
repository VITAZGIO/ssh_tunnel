package docker

import (
	"fmt"
	"net/netip"
	"sort"
)

// План ядра Linux для контейнеров — чистая функция KernelPlan, как
// vpnlayer/plan.go: на входе адреса контейнеров с выбором и состояние слоя
// VPN, на выходе — правила (ip rule) и маршруты наших таблиц. Слой VPN
// приводит ядро к плану целиком.
//
// Почему правила по адресу источника, а не fwmark: проект выбрал маршруты /32
// вместо fwmark + ip rule из-за строгого rp_filter (docs/decisions.md):
// проверка обратного пути идёт без метки (пока src_valid_mark=0). Правило
// «from <адрес контейнера>» ядро применяет и к самому пакету, и к проверке
// обратного пути ответа (там адрес контейнера — источник обратного пути),
// поэтому ответ из TUN на адрес контейнера проходит строгий rp_filter: путь
// туда и проверка сходятся, как у маршрутов /32.

// Номера наших таблиц и приоритеты правил. Таблицы — только наши (по ним
// после аварийного выхода находятся оставшиеся правила и маршруты).
// Приоритеты — раньше main (32766), позже local (0); не заняты известными
// программами (Tailscale — 52xx, wg-quick — 32764/32765).
const (
	TableTunnel    = 7094 // «через сервер»: адаптер или никуда
	TableTunnelDNS = 7095 // DNS контейнера «через сервер»: только адаптер
	TableDirect    = 7096 // «напрямую»: роутер настоящей сети

	PrioDNS  = 9400 // порт 53 контейнеров «через сервер» — раньше всего
	PrioMain = 9401 // весь остальной трафик контейнера

	// То же для «через сервер» с галочкой «без VPN — без интернета»: свои
	// приоритеты, чтобы следующий запуск отличил их правила от обычных
	// (обычные после аварии снимаются через 2 минуты, эти — никогда).
	PrioStrictDNS  = 9402
	PrioStrictMain = 9403

	// metricUnreachable — запасной «нет маршрута»: хуже любого настоящего.
	metricUnreachable = 0xFFFFFF00
)

// OurTables — все наши таблицы.
var OurTables = []int{TableTunnel, TableTunnelDNS, TableDirect}

// StrictPrio — приоритет правила контейнера с галочкой «без VPN — без
// интернета».
func StrictPrio(p int) bool { return p == PrioStrictDNS || p == PrioStrictMain }

// Target — адрес контейнера и как ему ходить (tunnel или direct).
type Target struct {
	Addr  netip.Addr `json:"addr"`
	Route Choice     `json:"route"`
	// Strict — «без VPN — без интернета» (только у tunnel).
	Strict bool `json:"strict,omitempty"`
}

// Net — состояние слоя VPN, от которого зависит план.
type Net struct {
	// Up — адаптер VPN поднят (режим VPN включён). Выключен — правил нет
	// совсем: компьютер такой же, как без программы.
	Up bool
	// Full — интернет через сервер (круглая кнопка включена). Выключена —
	// контейнерам «через сервер» интернета нет.
	Full bool
	// Tun — имя адаптера (sshtun0).
	Tun string
	// V6 — у адаптера есть IPv6.
	V6 bool
	// Настоящая сеть: карта и роутер (роутера может не быть у «точка-точка»).
	Uplink4 string
	Gw4     netip.Addr
	Uplink6 string
	Gw6     netip.Addr
}

// Rule — одно правило ip rule.
type Rule struct {
	Family   int // 4 или 6
	Priority int
	Src      netip.Prefix
	IPProto  int    // 0 — любой, 6 — TCP, 17 — UDP
	DPort    uint16 // 0 — любой
	Table    int
}

func (r Rule) String() string {
	s := fmt.Sprintf("v%d pref %d from %s", r.Family, r.Priority, r.Src)
	if r.IPProto != 0 {
		s += fmt.Sprintf(" ipproto %d", r.IPProto)
	}
	if r.DPort != 0 {
		s += fmt.Sprintf(" dport %d", r.DPort)
	}
	return s + fmt.Sprintf(" lookup %d", r.Table)
}

// RouteKind — вид маршрута.
type RouteKind string

const (
	KindDev         RouteKind = "dev"         // прямо в карту (адаптер)
	KindVia         RouteKind = "via"         // через роутер Via на карте Dev
	KindThrow       RouteKind = "throw"       // «не здесь»: ядро идёт к следующему правилу (в main)
	KindUnreachable RouteKind = "unreachable" // нет маршрута: отказ сразу
)

// Route — один маршрут нашей таблицы.
type Route struct {
	Family int
	Table  int
	Dst    netip.Prefix
	Kind   RouteKind
	Dev    string
	Via    netip.Addr
	Metric uint32
}

func (r Route) String() string {
	s := fmt.Sprintf("v%d table %d %s %s", r.Family, r.Table, r.Kind, r.Dst)
	if r.Via.IsValid() {
		s += " via " + r.Via.String()
	}
	if r.Dev != "" {
		s += " dev " + r.Dev
	}
	if r.Metric != 0 {
		s += fmt.Sprintf(" metric %d", r.Metric)
	}
	return s
}

// Plan — что должно стоять в ядре.
type Plan struct {
	Rules  []Rule
	Routes []Route
	// Forced — адреса контейнеров «через сервер», которые идут в адаптер
	// со своим адресом источника, без подмены Docker (MASQUERADE): по нему
	// стек узнаёт, что соединение обязано идти только через сервер
	// (core.Conn.Forced), и чужие правила «напрямую» к нему не применяет.
	Forced []netip.Addr
}

// Empty — нечего ставить: ядро такое же, как без плагина.
func (p Plan) Empty() bool { return len(p.Rules) == 0 && len(p.Routes) == 0 && len(p.Forced) == 0 }

// Локальные адреса: к ним контейнер ходит как обычно (через main) при любом
// выборе — сам компьютер, соседние контейнеры и сети Docker, домашняя сеть
// (ответы на опубликованные порты `-p`), mesh-VPN вроде NetBird. Совпадает по
// смыслу с «локальная сеть — напрямую» ядра (routing.IsLocalHost).
var (
	local4 = prefixes("10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12",
		"192.168.0.0/16", "224.0.0.0/4", "255.255.255.255/32")
	local6 = prefixes("fc00::/7", "fe80::/10", "ff00::/8")
	// mesh4 — подсеть адаптера: адреса сети устройств и наш DNS.
	mesh4 = netip.MustParsePrefix("198.18.0.0/15")

	any4 = netip.MustParsePrefix("0.0.0.0/0")
	any6 = netip.MustParsePrefix("::/0")
)

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// KernelPlan — план ядра для адресов контейнеров при состоянии слоя n.
//
// Для каждого адреса «через сервер»:
//
//	pref 9400 from <адрес> ipproto udp dport 53 lookup 7095
//	pref 9400 from <адрес> ipproto tcp dport 53 lookup 7095
//	pref 9401 from <адрес> lookup 7094
//
// Для «напрямую»: pref 9401 from <адрес> lookup 7096.
//
// Таблица 7094: throw локальных сетей (→ main, как обычно), default dev
// sshtun0 (только при включённом туннеле; без него — лишь подсеть адаптера,
// для сети устройств) и всегда unreachable default с худшей метрикой.
// Адаптер исчез (программа упала) — ядро само убирает маршруты через него,
// остаётся unreachable: интернета нет, мимо туннеля ничего не уходит.
//
// Таблица 7095 (DNS «через сервер»): default dev sshtun0 при туннеле и
// unreachable — без throw: запрос к DNS домашнего роутера (192.168.x.x) тоже
// уходит в адаптер, а там его перехватывает DNS программы. Иначе он ушёл бы
// напрямую к провайдеру.
//
// Таблица 7096: throw локальных сетей и подсети адаптера, default через
// роутер настоящей сети.
//
// Адаптер выключен (VPN выключил человек, программа вышла) — правила
// остаются только у «через сервер» с галочкой «без VPN — без интернета»
// (приоритеты 9402/9403), в таблицах — лишь throw локальных сетей и
// unreachable: в домашнюю сеть и к соседним контейнерам можно, в интернет —
// нет.
func KernelPlan(ts []Target, n Net) Plan {
	var p Plan
	if len(ts) == 0 {
		return p
	}
	if !n.Up {
		// Без адаптера — только строгие, и никакой дороги в адаптер.
		var strict []Target
		for _, t := range ts {
			if t.Route == ChoiceTunnel && t.Strict {
				strict = append(strict, t)
			}
		}
		if len(strict) == 0 {
			return p
		}
		ts, n = strict, Net{}
	}
	need := map[[2]int]bool{} // {семейство, таблица}
	for _, t := range ts {
		fam, bits := 4, 32
		if t.Addr.Is6() && !t.Addr.Is4In6() {
			fam, bits = 6, 128
		}
		src := netip.PrefixFrom(t.Addr.Unmap(), bits)
		switch t.Route {
		case ChoiceTunnel:
			pd, pm := PrioDNS, PrioMain
			if t.Strict {
				pd, pm = PrioStrictDNS, PrioStrictMain
			}
			p.Rules = append(p.Rules,
				Rule{Family: fam, Priority: pd, Src: src, IPProto: 17, DPort: 53, Table: TableTunnelDNS},
				Rule{Family: fam, Priority: pd, Src: src, IPProto: 6, DPort: 53, Table: TableTunnelDNS},
				Rule{Family: fam, Priority: pm, Src: src, Table: TableTunnel},
			)
			need[[2]int{fam, TableTunnel}] = true
			need[[2]int{fam, TableTunnelDNS}] = true
			if n.Tun != "" {
				p.Forced = append(p.Forced, t.Addr.Unmap())
			}
		case ChoiceDirect:
			p.Rules = append(p.Rules, Rule{Family: fam, Priority: PrioMain, Src: src, Table: TableDirect})
			need[[2]int{fam, TableDirect}] = true
		}
	}
	for _, fam := range []int{4, 6} {
		local, anyNet := local4, any4
		tunOK := true
		upl, gw := n.Uplink4, n.Gw4
		if fam == 6 {
			local, anyNet, tunOK = local6, any6, n.V6
			upl, gw = n.Uplink6, n.Gw6
		}
		unreach := Route{Family: fam, Dst: anyNet, Kind: KindUnreachable, Metric: metricUnreachable}
		if need[[2]int{fam, TableTunnel}] {
			for _, l := range local {
				p.Routes = append(p.Routes, Route{Family: fam, Table: TableTunnel, Dst: l, Kind: KindThrow})
			}
			switch {
			case n.Full && tunOK && n.Tun != "":
				p.Routes = append(p.Routes, Route{Family: fam, Table: TableTunnel, Dst: anyNet, Kind: KindDev, Dev: n.Tun})
			case fam == 4 && n.Tun != "":
				// Без туннеля — только сеть устройств: она в подсети адаптера.
				p.Routes = append(p.Routes, Route{Family: fam, Table: TableTunnel, Dst: mesh4, Kind: KindDev, Dev: n.Tun})
			}
			u := unreach
			u.Table = TableTunnel
			p.Routes = append(p.Routes, u)
		}
		if need[[2]int{fam, TableTunnelDNS}] {
			if n.Full && tunOK && n.Tun != "" {
				p.Routes = append(p.Routes, Route{Family: fam, Table: TableTunnelDNS, Dst: anyNet, Kind: KindDev, Dev: n.Tun})
			}
			u := unreach
			u.Table = TableTunnelDNS
			p.Routes = append(p.Routes, u)
		}
		if need[[2]int{fam, TableDirect}] {
			for _, l := range local {
				p.Routes = append(p.Routes, Route{Family: fam, Table: TableDirect, Dst: l, Kind: KindThrow})
			}
			if fam == 4 {
				p.Routes = append(p.Routes, Route{Family: fam, Table: TableDirect, Dst: mesh4, Kind: KindThrow})
			}
			if upl != "" {
				r := Route{Family: fam, Table: TableDirect, Dst: anyNet, Kind: KindDev, Dev: upl}
				if gw.IsValid() {
					r.Kind, r.Via = KindVia, gw
				}
				p.Routes = append(p.Routes, r)
			}
			// Настоящей сети нет — маршрута по умолчанию в таблице нет, ядро
			// идёт дальше, в main: «напрямую» — не запрет туннеля.
		}
	}
	sort.SliceStable(p.Rules, func(i, j int) bool { return p.Rules[i].String() < p.Rules[j].String() })
	sort.Slice(p.Forced, func(i, j int) bool { return p.Forced[i].Less(p.Forced[j]) })
	return p
}
