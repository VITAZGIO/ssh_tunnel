package vpnlayer

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"sshtunnel/internal/docker"
)

// Правила ядра для контейнеров Docker (плагин «Docker», internal/docker).
// План считает docker.KernelPlan — чистая функция; здесь только приводим
// ядро к плану: ставим недостающее и снимаем лишнее в наших таблицах
// (7094–7096) и правилах, которые на них смотрят. Почему адрес источника, а
// не fwmark, — в internal/docker/kernel.go (строгий rp_filter).
//
// Жизнь правил:
//   - адаптер поднят (apply) — правила по плану, переставляются при каждом
//     apply (смена сети, кнопки) и при каждом изменении в Docker;
//   - адаптер выключен (close) — всё снято, компьютер как без программы;
//   - программа упала — правила остаются, а маршрут через исчезнувший
//     адаптер ядро снимает само: в таблицах остаётся unreachable, мимо
//     туннеля контейнеры «через сервер» не выходят;
//   - следующий запуск — оставшееся не трогаем, пока плагин не сообщил
//     адреса контейнеров (иначе открылось бы окно обхода, пока программа
//     подключается); если адаптер так и не поднялся за staleGrace — снимаем
//     обычные правила, как было до программы;
//   - галочка «без VPN — без интернета» (приоритеты 9402/9403): правила
//     этих контейнеров остаются и без адаптера, и после выхода программы —
//     в таблицах тогда только throw локальных сетей и unreachable.
//
// Строгий «через сервер» (ТЗ 14): пакеты таких контейнеров идут в адаптер со
// своим адресом источника — Docker не подменяет его (MASQUERADE) благодаря
// нашей цепочке в таблице nat (natChain). По адресу стек узнаёт, что
// соединение обязано идти только через сервер (core.Conn.Forced): к нему не
// применяются «всегда напрямую», слив после «Выключить» и DNS настоящей
// сети. Ответы стек шлёт на адрес контейнера; ядро пересылает их в мост
// Docker (conntrack: это ответ на соединение, начатое из моста).

// staleGrace — сколько ждать первого подъёма адаптера, прежде чем снять
// правила, оставшиеся от упавшего запуска.
const staleGrace = 2 * time.Minute

type dockerKernel struct {
	s *sys

	mu      sync.Mutex
	targets []docker.Target
	known   bool // плагин уже сообщил адреса (SetTargets)
	up      bool
	full    bool
	touched bool // после запуска уже был apply или close
	rules   int
	lastErr string

	// forced — адреса контейнеров «через сервер», идущие в адаптер со своим
	// адресом (Plan.Forced); читается стеком на каждое соединение.
	forced atomic.Pointer[map[netip.Addr]bool]
}

// isForced — источник обязан идти только через сервер.
func (k *dockerKernel) isForced(a netip.Addr) bool {
	m := k.forced.Load()
	return m != nil && (*m)[a.Unmap()]
}

// forcedSource — для стека (tunnel.Config.ForcedSource).
func (s *sys) forcedSource(a netip.Addr) bool { return s.dk != nil && s.dk.isForced(a) }

func newDockerKernel(s *sys) *dockerKernel {
	k := &dockerKernel{s: s}
	if n := countOurRules(); n > 0 {
		s.bus.Warnf("Docker: нашёл %d правил контейнеров от аварийно закрытого прошлого запуска — держу их (без туннеля у этих контейнеров нет интернета), пока VPN не поднимется", n)
		time.AfterFunc(staleGrace, func() {
			k.mu.Lock()
			defer k.mu.Unlock()
			if k.up {
				return
			}
			if k.known {
				k.reconcileLocked()
			} else if err := dropNonStrict(); err != nil {
				s.bus.Warnf("Docker: снять правила прошлого запуска не вышло: %v", err)
			}
			s.bus.Infof("Docker: VPN так и не поднялся — правила контейнеров от прошлого запуска сняты (кроме «без VPN — без интернета»)")
		})
	}
	return k
}

// SetTargets — адреса контейнеров от плагина (реализует docker.Kernel).
func (k *dockerKernel) SetTargets(ts []docker.Target) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.targets, k.known = ts, true
	if k.touched {
		k.reconcileLocked()
	}
}

// Status — для страницы плагина (реализует docker.Kernel).
func (k *dockerKernel) Status() docker.KernelStatus {
	k.mu.Lock()
	defer k.mu.Unlock()
	return docker.KernelStatus{Up: k.up, Full: k.full, Rules: k.rules, Error: k.lastErr}
}

// onApply — адаптер приведён к плану ядра (sys.apply).
func (k *dockerKernel) onApply(full bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.up, k.full, k.touched = true, full, true
	if k.known {
		k.reconcileLocked()
	}
}

// onClose — адаптер выключается: снять всё, кроме «без VPN — без
// интернета».
func (k *dockerKernel) onClose() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.up, k.full, k.touched = false, false, true
	k.forced.Store(nil)
	if k.known {
		k.reconcileLocked()
	} else {
		k.clearLocked()
	}
}

func (k *dockerKernel) net() docker.Net {
	s := k.s
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := docker.Net{Up: s.link != nil, Full: k.full, V6: s.v6, Uplink4: s.up4, Uplink6: s.up6}
	if s.link != nil {
		n.Tun = linuxIfName
	}
	if a, ok := netip.AddrFromSlice(s.gw4); ok {
		n.Gw4 = a.Unmap()
	}
	if a, ok := netip.AddrFromSlice(s.gw6); ok {
		n.Gw6 = a
	}
	return n
}

func (k *dockerKernel) reconcileLocked() {
	n := k.net()
	if !k.up {
		n = docker.Net{}
	}
	p := docker.KernelPlan(k.targets, n)
	// Сначала адреса для стека и исключение из подмены, потом маршруты:
	// контейнер, пришедший в адаптер, уже узнаётся.
	m := map[netip.Addr]bool{}
	for _, a := range p.Forced {
		m[a] = true
	}
	k.forced.Store(&m)
	err := errors.Join(applyNAT(p.Forced, linuxIfName), applyDockerPlan(p))
	k.rules = len(p.Rules)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != "" && msg != k.lastErr {
		k.s.bus.Warnf("Docker: правила контейнеров встали не все: %s", msg)
	}
	k.lastErr = msg
}

func (k *dockerKernel) clearLocked() {
	k.forced.Store(nil)
	if err := errors.Join(applyNAT(nil, linuxIfName), applyDockerPlan(docker.Plan{})); err != nil {
		k.s.bus.Warnf("Docker: снять правила контейнеров не вышло: %v", err)
	}
	k.rules = 0
}

func family(f int) int {
	if f == 6 {
		return netlink.FAMILY_V6
	}
	return netlink.FAMILY_V4
}

func ours(table int) bool {
	for _, t := range docker.OurTables {
		if t == table {
			return true
		}
	}
	return false
}

func countOurRules() int {
	n := 0
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rs, _ := netlink.RuleList(fam)
		for _, r := range rs {
			if ours(r.Table) {
				n++
			}
		}
	}
	return n
}

// ruleKey — правило ядра в виде строки плана (для сравнения).
func ruleKey(fam int, r netlink.Rule) string {
	f := 4
	if fam == netlink.FAMILY_V6 {
		f = 6
	}
	dr := docker.Rule{Family: f, Priority: r.Priority, Table: r.Table, IPProto: r.IPProto}
	if r.Src != nil {
		if a, ok := netip.AddrFromSlice(r.Src.IP); ok {
			ones, _ := r.Src.Mask.Size()
			if f == 4 {
				a = a.Unmap()
			}
			dr.Src = netip.PrefixFrom(a, ones)
		}
	}
	if r.Dport != nil {
		dr.DPort = r.Dport.Start
	}
	return dr.String()
}

func toNetlinkRule(r docker.Rule) *netlink.Rule {
	nr := netlink.NewRule()
	nr.Family = family(r.Family)
	nr.Priority = r.Priority
	nr.Table = r.Table
	nr.Src = prefixNet(r.Src)
	nr.IPProto = r.IPProto
	if r.DPort != 0 {
		nr.Dport = netlink.NewRulePortRange(r.DPort, r.DPort)
	}
	return nr
}

// toNetlinkRoute — маршрут плана для netlink; ifindex — номер карты по имени.
func toNetlinkRoute(r docker.Route, ifindex func(string) int) (*netlink.Route, error) {
	nr := &netlink.Route{Family: family(r.Family), Table: r.Table, Dst: prefixNet(r.Dst), Protocol: rtProtoOurs, Priority: int(r.Metric)}
	switch r.Kind {
	case docker.KindThrow:
		nr.Type = unix.RTN_THROW
	case docker.KindUnreachable:
		nr.Type = unix.RTN_UNREACHABLE
	case docker.KindDev, docker.KindVia:
		idx := ifindex(r.Dev)
		if idx == 0 {
			return nil, fmt.Errorf("нет карты %s", r.Dev)
		}
		nr.LinkIndex = idx
		if r.Kind == docker.KindVia {
			nr.Gw = net.IP(r.Via.AsSlice())
		} else {
			nr.Scope = netlink.SCOPE_LINK
		}
	default:
		return nil, fmt.Errorf("вид маршрута %q", r.Kind)
	}
	return nr, nil
}

// routeKey — маршрут для сравнения: таблица, вид, адрес, карта, роутер,
// метрика.
func routeKey(r netlink.Route) string {
	dst := "default"
	if r.Dst != nil {
		dst = r.Dst.String()
	}
	if r.Dst != nil {
		if ones, _ := r.Dst.Mask.Size(); ones == 0 {
			dst = "default"
		}
	}
	return fmt.Sprintf("%d/%d/%d/%s/%d/%s/%d", r.Family, r.Table, r.Type, dst, r.LinkIndex, r.Gw, r.Priority)
}

func ifindexByName(name string) int {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return 0
	}
	return l.Attrs().Index
}

// applyDockerPlan приводит наши таблицы и правила к плану. Порядок —
// чтобы и посреди перестановки не было обхода: сначала новые маршруты, потом
// новые правила, потом снять лишние правила, в конце — лишние маршруты.
func applyDockerPlan(p docker.Plan) error {
	var errs []error

	// Желаемые маршруты по ключу.
	wantRoutes := map[string]*netlink.Route{}
	for _, r := range p.Routes {
		nr, err := toNetlinkRoute(r, ifindexByName)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r, err))
			continue
		}
		wantRoutes[routeKey(*nr)] = nr
	}
	haveRoutes := map[string]netlink.Route{}
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		for _, t := range docker.OurTables {
			rs, err := netlink.RouteListFiltered(fam, &netlink.Route{Table: t}, netlink.RT_FILTER_TABLE)
			if err != nil {
				continue
			}
			for _, r := range rs {
				r.Family = fam
				haveRoutes[routeKey(r)] = r
			}
		}
	}
	for k, r := range wantRoutes {
		if _, ok := haveRoutes[k]; ok {
			continue
		}
		if err := netlink.RouteAdd(r); err != nil && !errors.Is(err, syscall.EEXIST) {
			errs = append(errs, fmt.Errorf("маршрут table %d %s: %w", r.Table, r.Dst, err))
		}
	}

	wantRules := map[string]docker.Rule{}
	for _, r := range p.Rules {
		wantRules[r.String()] = r
	}
	haveRules := map[string]netlink.Rule{}
	haveFam := map[string]int{}
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rs, err := netlink.RuleList(fam)
		if err != nil {
			continue
		}
		for _, r := range rs {
			if !ours(r.Table) {
				continue
			}
			k := ruleKey(fam, r)
			haveRules[k], haveFam[k] = r, fam
		}
	}
	for k, r := range wantRules {
		if _, ok := haveRules[k]; ok {
			continue
		}
		if err := netlink.RuleAdd(toNetlinkRule(r)); err != nil && !errors.Is(err, syscall.EEXIST) {
			errs = append(errs, fmt.Errorf("правило %s: %w", r, err))
		}
	}
	for k, r := range haveRules {
		if _, ok := wantRules[k]; ok {
			continue
		}
		r := r
		r.Family = haveFam[k]
		if err := netlink.RuleDel(&r); err != nil && !errors.Is(err, syscall.ENOENT) {
			errs = append(errs, fmt.Errorf("снять правило %s: %w", k, err))
		}
	}
	for k, r := range haveRoutes {
		if _, ok := wantRoutes[k]; ok {
			continue
		}
		r := r
		if err := netlink.RouteDel(&r); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("снять маршрут table %d %s: %w", r.Table, r.Dst, err))
		}
	}
	return errors.Join(errs...)
}

// dropNonStrict — снять правила прошлого запуска, кроме «без VPN — без
// интернета»; если таких не осталось — и маршруты наших таблиц.
func dropNonStrict() error {
	var errs []error
	strict := 0
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rs, _ := netlink.RuleList(fam)
		for _, r := range rs {
			if !ours(r.Table) {
				continue
			}
			if docker.StrictPrio(r.Priority) {
				strict++
				continue
			}
			r := r
			r.Family = fam
			if err := netlink.RuleDel(&r); err != nil && !errors.Is(err, syscall.ENOENT) {
				errs = append(errs, err)
			}
		}
	}
	if strict == 0 {
		errs = append(errs, applyDockerPlan(docker.Plan{}))
	}
	errs = append(errs, applyNAT(nil, linuxIfName))
	return errors.Join(errs...)
}
