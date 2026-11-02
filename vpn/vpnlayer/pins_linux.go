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

// uplinkRoute — куда ставить маршрут к a: карта и роутер настоящей сети.
func (s *sys) uplinkRoute(a netip.Addr) *netlink.Route {
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
		d = &linuxPin{route: addRoute(s.uplinkRoute(a))}
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
	for a, d := range p.direct {
		if d.timer != nil {
			d.timer.Stop()
			d.timer = nil
		}
		delRoute(d.route)
		d.route = nil
		if on && d.refs > 0 {
			d.route = addRoute(s.uplinkRoute(a))
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
		if r := addRoute(s.uplinkRoute(a.Unmap())); r != nil {
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
		rs, err := netlink.RouteListFiltered(fam, &netlink.Route{Protocol: rtProtoOurs}, netlink.RT_FILTER_PROTOCOL)
		if err != nil {
			continue
		}
		for i := range rs {
			if err := netlink.RouteDel(&rs[i]); err == nil || errors.Is(err, syscall.ESRCH) {
				n++
			}
		}
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
