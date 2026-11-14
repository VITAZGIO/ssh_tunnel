package vpnlayer

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
)

// Маршруты к прямым адресам и к серверу через настоящую сеть — как на
// Windows (sys_windows.go, pinDirect/pinServers), по той же причине.
//
// SO_BINDTODEVICE отправляет пакеты своих сокетов через настоящую карту, но
// ответы ядро проверяет по таблице маршрутов: при строгом rp_filter=1
// (RHEL/Fedora) обратный путь к адресу ведёт в наш TUN (0.0.0.0/1,
// 128.0.0.0/1), а ответ пришёл на карту — и ядро его выкидывает. Соединение
// «напрямую» висит без ответа. Маршрут /32 к адресу через роутер настоящей
// сети делает таблицу честной: и путь туда, и проверка ответа сходятся.
//
// Маршрут к прямому адресу — не в main, а в своей таблице pinTable, и её
// смотрят только пакеты с адреса настоящей карты (pref pinPrio from <адрес
// карты>): это сокеты самой программы (SO_BINDTODEVICE берёт адрес карты), и
// при проверке ответа ядро ищет обратный путь именно «from <адрес карты>».
// В main такой маршрут ломал другое: ответ стека из TUN приложению или
// контейнеру, чьё соединение к этому же адресу идёт через TUN, приходит с
// адресом источника a, а обратный путь к a вёл уже на карту — при rp_filter=1
// ответ выкидывался, и первое соединение «напрямую» (для контейнера —
// до истечения записи conntrack) висело. Нашёл стенд на настоящем ядре
// (ТЗ 19, контейнер «как компьютер» к адресу из «всегда напрямую»).
// Маршруты к серверу остаются в main: к нему и приложения ходят мимо TUN.
const (
	pinTable = 7097
	pinPrio  = 9399
)

// rtProtoOurs — метка наших маршрутов (поле protocol): по ней после
// аварийного выхода находятся и снимаются оставшиеся. 94 не занят ни одним
// известным демоном маршрутизации (см. /etc/iproute2/rt_protos).
const rtProtoOurs = netlink.RouteProtocol(94)

// linuxPin — маршрут к адресу прямого соединения и сколько соединений им
// пользуются.
type linuxPin struct {
	refs  int
	route *netlink.Route // nil — не ставили (уже был чужой или не вышло)
	timer *time.Timer
}

// pins — состояние маршрутов; поле sys.pin.
type pins struct {
	mu      sync.Mutex
	on      bool
	direct  map[netip.Addr]*linuxPin
	servers []*netlink.Route
}

// uplinkRoute — куда ставить маршрут к a: карта и роутер настоящей сети;
// table 0 — main.
func (s *sys) uplinkRoute(a netip.Addr, table int) *netlink.Route {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx, gw, bits := s.idx4, s.gw4, 32
	if a.Is6() {
		idx, gw, bits = s.idx6, s.gw6, 128
	}
	if idx == 0 {
		return nil
	}
	return &netlink.Route{
		LinkIndex: idx,
		Dst:       prefixNet(netip.PrefixFrom(a, bits)),
		Gw:        gw, // nil у соединений «точка-точка» — тогда маршрут прямо в карту
		Protocol:  rtProtoOurs,
		Table:     table,
	}
}

// uplinkAddrs — адреса настоящей карты (без fe80::), по которым правило
// pinPrio отправляет сокеты программы в pinTable.
func (s *sys) uplinkAddrs() []netip.Prefix {
	s.mu.RLock()
	idx4, idx6 := s.idx4, s.idx6
	s.mu.RUnlock()
	var out []netip.Prefix
	for _, c := range []struct{ idx, fam int }{{idx4, netlink.FAMILY_V4}, {idx6, netlink.FAMILY_V6}} {
		if c.idx == 0 {
			continue
		}
		l, err := netlink.LinkByIndex(c.idx)
		if err != nil {
			continue
		}
		as, _ := netlink.AddrList(l, c.fam)
		for _, a := range as {
			ip, ok := netip.AddrFromSlice(a.IP)
			if !ok || ip.IsLinkLocalUnicast() {
				continue
			}
			ip = ip.Unmap()
			out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
		}
	}
	return out
}

// pinRulePlan — какие правила pinPrio поставить и какие снять: have — ключи
// стоящих (префикс источника), want — нужные.
func pinRulePlan(have, want []netip.Prefix) (add, del []netip.Prefix) {
	h, w := map[netip.Prefix]bool{}, map[netip.Prefix]bool{}
	for _, p := range have {
		h[p] = true
	}
	for _, p := range want {
		w[p] = true
		if !h[p] {
			add = append(add, p)
		}
	}
	for _, p := range have {
		if !w[p] {
			del = append(del, p)
		}
	}
	return add, del
}

func pinRule(p netip.Prefix) *netlink.Rule {
	r := netlink.NewRule()
	r.Family = netlink.FAMILY_V4
	if p.Addr().Is6() {
		r.Family = netlink.FAMILY_V6
	}
	r.Priority, r.Table, r.Src = pinPrio, pinTable, prefixNet(p)
	return r
}

// ourPinRules — стоящие правила pinPrio → pinTable.
func ourPinRules() []netlink.Rule {
	var out []netlink.Rule
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rs, _ := netlink.RuleList(fam)
		for _, r := range rs {
			if r.Table == pinTable {
				r.Family = fam
				out = append(out, r)
			}
		}
	}
	return out
}

// syncPinRules приводит правила pinPrio к адресам карты (on) или снимает их.
func (s *sys) syncPinRules(on bool) {
	var want []netip.Prefix
	if on {
		want = s.uplinkAddrs()
	}
	byKey := map[netip.Prefix]netlink.Rule{}
	var have []netip.Prefix
	for _, r := range ourPinRules() {
		if r.Src == nil {
			continue
		}
		ip, ok := netip.AddrFromSlice(r.Src.IP)
		if !ok {
			continue
		}
		ones, _ := r.Src.Mask.Size()
		k := netip.PrefixFrom(ip.Unmap(), ones)
		byKey[k] = r
		have = append(have, k)
	}
	add, del := pinRulePlan(have, want)
	for _, p := range add {
		if err := netlink.RuleAdd(pinRule(p)); err != nil && !errors.Is(err, syscall.EEXIST) {
			s.bus.Warnf("правило маршрутов «напрямую» from %s не встало: %v", p, err)
		}
	}
	for _, p := range del {
		r := byKey[p]
		_ = netlink.RuleDel(&r)
	}
}

// addRoute ставит маршрут; уже стоящий чужой не трогаем (nil).
func addRoute(r *netlink.Route) *netlink.Route {
	if r == nil {
		return nil
	}
	if err := netlink.RouteAdd(r); err != nil {
		return nil // EEXIST — чей-то свой маршрут, остальное — не судьба
	}
	return r
}

func delRoute(r *netlink.Route) {
	if r != nil {
		_ = netlink.RouteDel(r)
	}
}

func (s *sys) pinner() func(netip.Addr) func() { return s.pinDirect }

// pinDirect ставит маршрут к адресу на время прямого соединения
// (tunnel.Config.PinDirect).
func (s *sys) pinDirect(a netip.Addr) func() {
	a = a.Unmap()
	p := &s.pin
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.on || !pinnable(a) || s.isServer(a) {
		return func() {}
	}
	d := p.direct[a]
	if d == nil {
		if len(p.direct) >= maxDirectPins {
			return func() {}
		}
		d = &linuxPin{route: addRoute(s.uplinkRoute(a, pinTable))}
		if p.direct == nil {
			p.direct = map[netip.Addr]*linuxPin{}
		}
		p.direct[a] = d
	}
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.refs++
	var once sync.Once
	return func() { once.Do(func() { s.releasePin(a, d) }) }
}

func (s *sys) releasePin(a netip.Addr, d *linuxPin) {
	p := &s.pin
	p.mu.Lock()
	defer p.mu.Unlock()
	if d.refs--; d.refs > 0 || p.direct[a] != d {
		return
	}
	d.timer = time.AfterFunc(pinLinger, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if d.refs == 0 && p.direct[a] == d {
			delRoute(d.route)
			delete(p.direct, a)
		}
	})
}

// setDirectPins включает или выключает маршруты прямых соединений.
// Включённые ставятся заново: сеть могла смениться, старые ведут через
// прежний роутер.
func (s *sys) setDirectPins(on bool) {
	p := &s.pin
	p.mu.Lock()
	defer p.mu.Unlock()
	p.on = on
	// Правило — до маршрутов и заново при каждом apply: адрес карты мог
	// смениться вместе с сетью.
	s.syncPinRules(on)
	for a, d := range p.direct {
		if d.timer != nil {
			d.timer.Stop()
			d.timer = nil
		}
		delRoute(d.route)
		d.route = nil
		if on && d.refs > 0 {
			d.route = addRoute(s.uplinkRoute(a, pinTable))
			continue
		}
		delete(p.direct, a)
	}
}

func (s *sys) isServer(a netip.Addr) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, srv := range s.servers {
		if srv == a {
			return true
		}
	}
	return false
}

// setServers запоминает адреса сервера; при обходе к ним ставятся маршруты.
func (s *sys) setServers(addrs []netip.Addr) {
	s.mu.Lock()
	s.servers = append([]netip.Addr(nil), addrs...)
	full := s.full
	s.mu.Unlock()
	if full {
		s.pinServers()
	}
}

// pinServers ставит маршруты к серверу через настоящую сеть (заново).
func (s *sys) pinServers() {
	s.unpinServers()
	s.mu.RLock()
	servers := append([]netip.Addr(nil), s.servers...)
	s.mu.RUnlock()
	p := &s.pin
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range servers {
		if r := addRoute(s.uplinkRoute(a.Unmap(), 0)); r != nil {
			p.servers = append(p.servers, r)
		}
	}
}

func (s *sys) unpinServers() {
	p := &s.pin
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.servers {
		delRoute(r)
	}
	p.servers = nil
}

// dropStalePins снимает наши маршруты, оставшиеся от аварийно закрытого
// прошлого запуска.
func dropStalePins() int {
	n := 0
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		// main (без фильтра по таблице netlink отдаёт только её) и pinTable.
		filters := []struct {
			r    *netlink.Route
			mask uint64
		}{
			{&netlink.Route{Protocol: rtProtoOurs}, netlink.RT_FILTER_PROTOCOL},
			{&netlink.Route{Protocol: rtProtoOurs, Table: pinTable}, netlink.RT_FILTER_PROTOCOL | netlink.RT_FILTER_TABLE},
		}
		for _, f := range filters {
			rs, err := netlink.RouteListFiltered(fam, f.r, f.mask)
			if err != nil {
				continue
			}
			for i := range rs {
				if err := netlink.RouteDel(&rs[i]); err == nil || errors.Is(err, syscall.ESRCH) {
					n++
				}
			}
		}
	}
	for _, r := range ourPinRules() {
		r := r
		_ = netlink.RuleDel(&r)
	}
	return n
}

// gatewayOf — роутер маршрута по умолчанию (nil у «точка-точка»).
func gatewayOf(r netlink.Route) net.IP {
	if r.Gw != nil {
		return r.Gw
	}
	for _, nh := range r.MultiPath {
		if nh.Gw != nil {
			return nh.Gw
		}
	}
	return nil
}
