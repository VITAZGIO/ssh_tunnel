package tunnel

// Здесь туннель спрашивает ядро, куда вести соединение (core.Decide), и
// исполняет ответ. Само решение в туннеле не принимается: правило пишется
// один раз в core и одинаково для прокси, для стека режима VPN и для
// Android.
//
// Кроме того, туннель помнит все живые соединения вместе с принятым
// решением — чтобы при смене режима закрыть открытые по старым правилам
// (core.Hygiene): состояние после переключения должно совпадать с чистым
// запуском в новом режиме.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"sshtunnel/internal/core"
	"sshtunnel/internal/procinfo"
)

// rules — правила решения прямо сейчас: режим кнопок и настройки.
func (t *Tunnel) rules() core.Rules {
	t.mu.RLock()
	policy, direct, tunneled := t.cfg.Policy, t.cfg.Direct, t.cfg.Tunneled
	t.mu.RUnlock()
	return core.Rules{
		Bypass:         !t.bypassOff.Load(),
		Mesh:           t.cfg.Mesh != nil && !t.meshPaused.Load(),
		Draining:       t.draining.Load(),
		LocalViaTunnel: t.localViaTunnel.Load(),
		Policy:         policy,
		Direct:         direct,
		Tunneled:       tunneled,
	}
}

// Rules — те же правила для DNS стека (core.DecideDNS): чем отвечать на имя.
func (t *Tunnel) Rules() core.Rules { return t.rules() }

// decide спрашивает ядро, куда вести соединение программы process к target.
// Адрес, выданный нашим DNS «настоящим» под имя из «всегда напрямую»,
// сверяется со списками по этому имени (см. LearnDirect).
func (t *Tunnel) decide(process, target string) (core.Route, core.Conn) {
	c := core.Conn{Process: process, Target: target}
	if name, ok := t.learnedName(target); ok {
		c.Name = name
	}
	return core.Decide(t.rules(), c), c
}

// dialRoute открывает TCP-соединение тем путём, который выбрало ядро.
func (t *Tunnel) dialRoute(route core.Route, target string) (net.Conn, error) {
	switch route {
	case core.RouteServer:
		return t.Dial("tcp", target)
	case core.RouteDirect:
		return t.dialDirect("tcp", target, 15*time.Second)
	case core.RouteMesh:
		m := t.mesh.Load()
		if m == nil {
			return nil, errMeshNotReady
		}
		return m.DialPeer(target)
	}
	return nil, errBlocked
}

var (
	errMeshNotReady = errors.New("сеть устройств ещё не подключилась")
	errBlocked      = errors.New("сеть устройств выключена")
)

// entry — откуда пришло соединение приложения.
type entry int

const (
	entryProxy entry = iota // локальный SOCKS/HTTP-прокси
	entryStack              // адаптер режима VPN или Android
)

// liveConn — живое соединение и решение, по которому оно открыто.
type liveConn struct {
	conn  core.Conn
	route core.Route
	from  entry
	close func()
}

type liveConns struct {
	mu sync.Mutex
	m  map[*liveConn]struct{}
}

// track запоминает соединение до его закрытия (вызвать возвращённую
// функцию).
func (t *Tunnel) track(c core.Conn, route core.Route, from entry, closeFn func()) func() {
	lc := &liveConn{conn: c, route: route, from: from, close: closeFn}
	t.live.mu.Lock()
	if t.live.m == nil {
		t.live.m = make(map[*liveConn]struct{})
	}
	t.live.m[lc] = struct{}{}
	t.live.mu.Unlock()
	return func() {
		t.live.mu.Lock()
		delete(t.live.m, lc)
		t.live.mu.Unlock()
	}
}

// ApplyHygiene закрывает соединения, открытые по правилам старого режима
// (см. core.Transition). Возвращает, сколько закрыто.
//
// Без этого браузер держал бы открытое в «Обходе» соединение (HTTP/2,
// keep-alive) и после выключения обхода: страницы продолжали бы ходить через
// сервер, а сайты проверки адреса показывали бы адрес сервера.
func (t *Tunnel) ApplyHygiene(h core.Hygiene) int {
	rules := t.rules()
	var victims []*liveConn
	t.live.mu.Lock()
	for lc := range t.live.m {
		if (h.CloseMesh && lc.route == core.RouteMesh) ||
			(h.CloseStackInternet && lc.from == entryStack && lc.route != core.RouteMesh) ||
			(h.Reevaluate && core.Decide(rules, lc.conn) != lc.route) {
			victims = append(victims, lc)
		}
	}
	t.live.mu.Unlock()
	for _, v := range victims {
		v.close()
	}
	if len(victims) > 0 {
		t.bus.Infof("Закрыто соединений, открытых по прежним правилам: %d — приложения переподключатся по новым", len(victims))
	}
	return len(victims)
}

// LiveConns — сколько соединений сейчас открыто по каждому пути (для
// диагностики и тестов).
func (t *Tunnel) LiveConns() map[core.Route]int {
	out := map[core.Route]int{}
	t.live.mu.Lock()
	for lc := range t.live.m {
		out[lc.route]++
	}
	t.live.mu.Unlock()
	return out
}

// dialDirect — соединение мимо сервера. Сокет привязан к настоящей сети
// (ProtectSocket: IP_UNICAST_IF на Windows, SO_BINDTODEVICE на Linux,
// VpnService.protect на Android). Если платформа умеет (PinDirect), к адресу
// на время соединения ставится маршрут через настоящую сеть: на Windows одной
// привязки сокета бывает мало — с включённой точкой доступа система сверяет
// ответы с таблицей маршрутов и выкидывает их, соединение висит в SYN_SENT.
func (t *Tunnel) dialDirect(network, target string, timeout time.Duration) (net.Conn, error) {
	d := t.directDialer(timeout)
	pin := t.cfg.PinDirect
	if pin == nil {
		return d.Dial(network, target)
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a.Unmap()}
	} else {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		ips, err := t.resolver().LookupNetIP(ctx, "ip", host)
		cancel()
		if err != nil {
			return nil, err
		}
		addrs = preferIPv4(ips)
	}
	var lastErr error
	for i, a := range addrs {
		if i == 4 || time.Now().After(deadline) {
			break
		}
		release := pin(a)
		d.Deadline = deadline
		c, err := d.Dial(network, netip.AddrPortFrom(a, uint16(port)).String())
		if err != nil {
			release()
			lastErr = err
			continue
		}
		return &pinnedConn{Conn: c, release: release}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("нет адресов для " + host)
	}
	return nil, lastErr
}

func (t *Tunnel) resolver() *net.Resolver {
	if t.cfg.Resolver != nil {
		return t.cfg.Resolver
	}
	return net.DefaultResolver
}

// preferIPv4 ставит адреса IPv4 первыми: прямой путь по IPv6 чаще сломан
// (у сети его нет, а у адаптера есть), и ждать его тайм-аута незачем.
func preferIPv4(ips []netip.Addr) []netip.Addr {
	out := make([]netip.Addr, 0, len(ips))
	for _, a := range ips {
		if a = a.Unmap(); a.Is4() {
			out = append(out, a)
		}
	}
	for _, a := range ips {
		if a = a.Unmap(); !a.Is4() {
			out = append(out, a)
		}
	}
	return out
}

// pinnedConn снимает маршрут к адресу, когда соединение закрыто.
type pinnedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *pinnedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// CloseWrite — полузакрытие, как у самого TCP-соединения (см. closeWrite).
func (c *pinnedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.SetReadDeadline(time.Now().Add(30 * time.Second))
}

// RouteUDP решает, что делать с UDP-потоком приложения из стека: src —
// откуда (по нему находится программа), target — куда (имя, если адрес был
// подставным). nil — не брать: стек ответит «порт недоступен», и браузер
// сразу перейдёт с QUIC на TCP.
//
// Напрямую — для исключённых программ и целей «всегда напрямую»: им UDP
// нужен как без программы (NetBird и WireGuard, звонки, игры). Через сервер
// — только через ретранслятор UDP, если он есть. В сеть устройств UDP не
// ходит.
func (t *Tunnel) RouteUDP(src netip.AddrPort, target string, byIP bool) func(app net.Conn) {
	var proc string
	var pid int
	if t.cfg.TunProcessRules {
		proc, pid = procinfo.LookupUDP(src.Port())
	}
	route, c := t.decide(proc, target)
	switch route {
	case core.RouteDirect:
		return func(app net.Conn) { t.serveUDPDirect(app, c, proc, pid, byIP) }
	case core.RouteServer:
		relay := t.UDPRelay()
		if relay == nil {
			return nil
		}
		return func(app net.Conn) { t.serveUDPRelay(app, c, relay) }
	}
	return nil
}

// udpIdle — сколько поток UDP живёт без единой датаграммы в любую сторону.
const udpIdle = 60 * time.Second

// serveUDPDirect ведёт UDP-поток приложения напрямую мимо сервера.
func (t *Tunnel) serveUDPDirect(app net.Conn, c core.Conn, proc string, pid int, byIP bool) {
	defer app.Close()
	remote, err := t.dialDirect("udp", c.Target, 10*time.Second)
	if err != nil {
		t.bus.Publish(eventConn(proc, pid, c.Target, "udp", byIP, true, err))
		return
	}
	defer remote.Close()
	untrack := t.track(c, core.RouteDirect, entryStack, func() { app.Close(); remote.Close() })
	defer untrack()
	t.bus.Publish(eventConn(proc, pid, c.Target, "udp", byIP, true, nil))
	pumpDatagrams(app, remote)
}

// pumpDatagrams перекладывает датаграммы в обе стороны, пока поток не
// замолчит на udpIdle (в обе стороны сразу: звонок, где говорит один,
// живёт) или одна из сторон не закроется.
func pumpDatagrams(a, b net.Conn) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	copyLoop := func(dst, src net.Conn, done chan<- struct{}) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 65535)
		for {
			src.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := src.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() &&
					time.Since(time.Unix(0, last.Load())) < udpIdle {
					continue
				}
				return
			}
			last.Store(time.Now().UnixNano())
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
	}
	done := make(chan struct{}, 2)
	go copyLoop(b, a, done)
	go copyLoop(a, b, done)
	<-done
	a.Close()
	b.Close()
	<-done
}

// serveUDPRelay ведёт UDP-поток через ретранслятор на сервере.
func (t *Tunnel) serveUDPRelay(app net.Conn, c core.Conn, relay udpRelay) {
	defer app.Close()
	host, portStr, err := net.SplitHostPort(c.Target)
	if err != nil {
		return
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return
	}
	sessionID := relay.Open(func(data []byte, _ string, _ uint16) {
		app.Write(data)
	})
	defer relay.Close(sessionID)
	untrack := t.track(c, core.RouteServer, entryStack, func() { app.Close() })
	defer untrack()

	buf := make([]byte, 65535)
	for {
		app.SetReadDeadline(time.Now().Add(udpIdle))
		n, err := app.Read(buf)
		if err != nil {
			return
		}
		if err := relay.Send(sessionID, host, uint16(port), buf[:n]); err != nil {
			return
		}
	}
}

// udpRelay — то, что нужно от клиента ретранслятора (udprelay.Client).
type udpRelay interface {
	Open(func(data []byte, fromHost string, fromPort uint16)) uint32
	Close(sessionID uint32)
	Send(sessionID uint32, host string, port uint16, data []byte) error
}
