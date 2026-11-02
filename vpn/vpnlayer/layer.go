//go:build windows || linux

package vpnlayer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"sshtunnel/android/core"
	"sshtunnel/internal/config"
	core2 "sshtunnel/internal/core"
	"sshtunnel/internal/events"
	"sshtunnel/internal/mesh"
	"sshtunnel/internal/routing"
	"sshtunnel/internal/tunnel"
	"sshtunnel/internal/udprelay"
)

// Layer — режим VPN, подключаемый к app.App (реализует app.NetLayer).
//
// Адаптер поднимается один раз на подключение и переживает переход на
// запасной сервер: меняется только туннель, в который он ведёт.
type Layer struct {
	bus *events.Bus
	sys *sys

	// current — туннель, куда сейчас идут соединения из адаптера.
	current atomic.Pointer[tunnel.Tunnel]

	mu      sync.Mutex
	direct  *routing.DirectList
	engine  *core.Engine
	up      bool
	unwatch func()
	// stopHotspot останавливает слежку за точкой доступа (watchHotspot).
	stopHotspot chan struct{}
	servers     []netip.Addr
	sshPort     int
	// modes — режим, применённый к адаптеру (см. Apply).
	modes core2.Modes
	// pool — таблица подставных адресов поднятого адаптера (сбрасывается при
	// смене режима, см. core.Hygiene).
	pool *core.FakePool
	// onNetChange — сообщить ядру, что сменилась сеть: оно применит режим
	// заново, целиком (см. SetNetworkHook).
	onNetChange func()

	// block — блокировка рекламы (см. adblock.go). Один и тот же объект на
	// всё время жизни: настройки меняют его содержимое, а не указатель.
	block *core.BlockList
	// stats — счётчики стека поднятого адаптера; из них берётся число
	// заблокированных имён. Атомарно, без mu: запрос состояния не должен
	// ждать, пока адаптер поднимается или снимается.
	stats atomic.Pointer[core.Stats]
}

// ApplyConfig принимает общие настройки программы (реализует app.ConfigLayer):
// раздавать ли VPN на мобильную точку доступа Windows.
func (l *Layer) ApplyConfig(cfg config.Config) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sys.setShare(!cfg.HotspotDirect)
}

// PrepareSelfCheck пускает проверочное соединение самопроверки мимо адаптера
// (реализует app.SelfCheckLayer).
func (l *Layer) PrepareSelfCheck(cfg *tunnel.Config) {
	cfg.ProtectSocket = l.sys.protect
	cfg.Resolver = l.resolver()
}

// SetNetworkHook — куда сообщать о смене настоящей сети (реализует
// app.NetworkNotifier): ядро в ответ применяет режим заново, целиком.
func (l *Layer) SetNetworkHook(f func()) {
	l.mu.Lock()
	l.onNetChange = f
	l.mu.Unlock()
}

// New готовит слой. Адаптер появится только при подключении.
func New(bus *events.Bus) *Layer {
	return &Layer{bus: bus, sys: newSys(bus), block: core.NewBlockList(nil, nil)}
}

// Prepare вызывается перед созданием каждого туннеля.
func (l *Layer) Prepare(cfg *tunnel.Config) {
	// Локальные прокси в режиме VPN не нужны: всё приходит из адаптера. И
	// не занимать порты 1080/1081 полезно — обычная версия может быть
	// запущена рядом.
	cfg.SocksAddr, cfg.HTTPAddr = "", ""
	cfg.ProtectSocket = l.sys.protect
	cfg.PinDirect = l.sys.pinner()
	cfg.Resolver = l.resolver()
	cfg.TunProcessRules = true

	l.mu.Lock()
	l.direct = cfg.Direct
	l.mu.Unlock()
}

// Apply приводит адаптер к режиму m целиком (реализует app.PlanLayer): план
// считает ядро (core.PlanVPN), здесь он только исполняется. Не поднят адаптер
// — поднимается; поднят — меняются маршруты и DNS, сам адаптер не
// пересоздаётся, и соединения к устройствам сети (RDP) не рвутся. Тот же
// вызов переводит адаптер на новый туннель (переход на запасной сервер) и
// заново ставит всё, что зависит от настоящей сети, после её смены.
func (l *Layer) Apply(tun *tunnel.Tunnel, p config.Profile, m core2.Modes, h core2.Hygiene) error {
	// До блокировки: поиск адреса может занять секунды, а под блокировкой
	// ждали бы и все соединения из адаптера.
	servers := l.lookupServer(p.Host)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.current.Store(tun)
	l.servers, l.sshPort = servers, p.SSHPort
	wasUp, prev := l.up, l.modes
	l.modes = m
	if !l.up {
		if err := l.bringUp(); err != nil {
			l.bringDown()
			l.current.Store(nil)
			return err
		}
	}
	l.sys.setServers(servers)
	plan := core2.PlanVPN(m, core2.PlanOptions{IPv6: l.sys.hasV6(), ShareWanted: l.sys.wantShare()})
	if err := l.sys.apply(plan); err != nil {
		return err
	}
	if h.ResetFakeIP && l.pool != nil {
		l.pool.Reset()
	}
	if h.FlushDNS {
		l.sys.flushDNS()
	}
	switch {
	case !wasUp && m.Bypass:
		l.bus.Infof("VPN включён: весь трафик системы идёт через адаптер %s", l.sys.name())
	case !wasUp:
		l.bus.Infof("VPN включён: через адаптер %s идёт только сеть устройств", l.sys.name())
	case prev.Bypass && !m.Bypass:
		l.bus.Infof("VPN: через адаптер идёт только сеть устройств, интернет — напрямую")
	case !prev.Bypass && m.Bypass:
		l.bus.Infof("VPN: весь трафик системы снова идёт через адаптер %s", l.sys.name())
	}
	return nil
}

// Detach снимает адаптер: вместе с ним исчезают его маршруты, и система
// сразу возвращается к обычной сети.
func (l *Layer) Detach() {
	l.mu.Lock()
	defer l.mu.Unlock()
	wasUp := l.up
	l.bringDown()
	l.current.Store(nil)
	if wasUp {
		l.bus.Infof("VPN выключен, сеть работает как обычно")
	}
}

func (l *Layer) bringUp() error {
	l.up = true
	l.sys.refresh()
	if !l.sys.hasUplink() {
		return errors.New("не нашёл подключение к интернету — не к чему привязать туннель")
	}

	dev, err := l.sys.open()
	if err != nil {
		return err
	}
	if err := l.startStack(dev); err != nil {
		return err
	}
	// Адреса адаптера; маршруты и DNS режима ставит apply по плану ядра.
	if err := l.sys.configure(false); err != nil {
		return err
	}

	unwatch, err := l.sys.watch(l.networkChanged)
	if err != nil {
		l.bus.Warnf("Не получится следить за сменой сети: %v", err)
	} else {
		l.unwatch = unwatch
	}
	l.stopHotspot = make(chan struct{})
	go l.watchHotspot(l.stopHotspot)
	l.sys.flushDNS()
	return nil
}

// watchHotspot раз в несколько секунд сверяет то, о чём система сообщает
// ненадёжно или никак:
//   - настоящую сеть: пропавший Wi-Fi не всегда убирает свой маршрут по
//     умолчанию, уведомления о смене нет, и прямые соединения так и шли бы
//     в мёртвую карту (NetBird, «всегда напрямую», исключённые программы);
//   - мобильную точку доступа: раздавать VPN надо и тогда, когда её
//     включили после VPN.
func (l *Layer) watchHotspot(stop chan struct{}) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		changed := false
		l.mu.Lock()
		if l.up {
			changed = l.sys.refresh()
			l.sys.syncShare()
		}
		l.mu.Unlock()
		if changed {
			l.networkChanged()
		}
	}
}

func (l *Layer) bringDown() {
	if !l.up {
		return
	}
	l.up = false
	if l.unwatch != nil {
		l.unwatch()
		l.unwatch = nil
	}
	if l.stopHotspot != nil {
		close(l.stopHotspot)
		l.stopHotspot = nil
	}
	// Сначала адаптер, потом всё остальное (запрет DNS, настройки DNS
	// системы): наоборот на долю секунды открылось бы окно, когда DNS ещё
	// смотрит в адаптер, а защиты уже нет.
	l.sys.beforeClose()
	if l.engine != nil {
		l.engine.Close() // закрывает и устройство
		l.engine = nil
	}
	l.sys.close()
	l.sys.flushDNS()
}

func (l *Layer) startStack(dev core.Device) error {
	pool, err := core.NewFakePool(fakeNet)
	if err != nil {
		dev.Close()
		return err
	}
	l.pool = pool
	stats := &core.Stats{}
	l.stats.Store(stats)
	resolver := l.resolver()

	dns := &core.DNS{
		Pool:  pool,
		Stats: stats,
		// Реклама и слежка — отказом в имени, раньше всего остального.
		Block: l.block,
		// Имена сети устройств («ноутбук.mesh») — настоящими адресами
		// устройств: они и так ведут в сеть устройств через туннель.
		Static: func(name string) ([]net.IP, bool) {
			var m *mesh.Client
			if t := l.current.Load(); t != nil {
				m = t.Mesh()
			}
			return mesh.StaticDNS(m, name)
		},
		// Настоящим адресом (мимо туннеля) отвечает то, что решило ядро
		// (core.DecideDNS): локальная сеть, «всегда напрямую», и всё подряд,
		// когда обход выключен.
		Direct: func(name string) bool {
			t := l.current.Load()
			if t == nil {
				return false
			}
			return core2.DecideDNS(t.Rules(), name) == core2.DNSReal
		},
		Local: func(name string) ([]net.IP, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			addrs, err := resolver.LookupIP(ctx, "ip4", name)
			if err != nil {
				return nil, err
			}
			// Приложение придёт уже с адресом, а не с именем, — ядро должно
			// узнать его и не повести через сервер (см. tunnel.LearnDirect).
			if t := l.current.Load(); t != nil {
				t.LearnDirect(name, addrs)
			}
			return addrs, nil
		},
	}

	engine, err := core.StartDevice(dev, &core.Handler{
		Core:    (*coreSwitch)(l),
		Resolve: pool.Resolver(),
		Fake:    pool.Contains,
		DNS:     dns,
		Stats:   stats,
		Log:     func(line string) { l.bus.Infof("%s", line) },
		UDPRelay: func() *udprelay.Client {
			if t := l.current.Load(); t != nil {
				return t.UDPRelay()
			}
			return nil
		},
		// ping к устройствам сети (198.19.x.y) — через саму сеть устройств.
		Ping: func() core.Pinger {
			if t := l.current.Load(); t != nil {
				if m := t.Mesh(); m != nil {
					return m
				}
			}
			return nil
		},
	})
	if err != nil {
		return err // StartDevice уже закрыл устройство
	}
	l.engine = engine
	return nil
}

// resolver — резолвер для своих нужд программы: спрашивает DNS-сервер
// настоящей сети через привязанный к ней сокет. Системный спрашивать нельзя:
// пока VPN включён, он смотрит в наш же адаптер.
func (l *Layer) resolver() *net.Resolver {
	d := &net.Dialer{Timeout: 5 * time.Second, Control: l.sys.protect}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return d.DialContext(ctx, network, pickDNSServer(address, l.sys.dnsServers()))
		},
	}
}

// networkChanged — сменилась настоящая сеть: старые SSH-соединения привязаны
// к прежней карте и почти наверняка мертвы, пересобираем пул сразу, а ядро
// применяет режим заново (маршрут к серверу и прямые маршруты — через новый
// роутер).
func (l *Layer) networkChanged() {
	l.bus.Infof("Сеть сменилась — переподключаюсь к серверу")
	l.mu.Lock()
	hook := l.onNetChange
	if l.up && hook == nil {
		l.sys.setServers(l.servers)
	}
	l.mu.Unlock()
	if t := l.current.Load(); t != nil {
		t.Kick()
	}
	if hook != nil {
		hook()
	}
}

// lookupServer узнаёт адреса сервера — чтобы распознать петлю (см.
// isServerTarget). Спрашивает настоящий DNS, как и сам туннель.
func (l *Layer) lookupServer(host string) []netip.Addr {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a.Unmap()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := l.resolver().LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil
	}
	for i := range ips {
		ips[i] = ips[i].Unmap()
	}
	return ips
}

// coreSwitch отдаёт соединения из адаптера текущему туннелю.
type coreSwitch Layer

// RouteUDP — UDP-поток из адаптера: куда его вести, решает ядро через
// текущий туннель (см. tunnel.RouteUDP).
func (c *coreSwitch) RouteUDP(src netip.AddrPort, target string, byIP bool) func(net.Conn) {
	t := (*Layer)(c).current.Load()
	if t == nil {
		return nil
	}
	return t.RouteUDP(src, target, byIP)
}

func (c *coreSwitch) ServeConn(conn net.Conn, target string, byIP bool) {
	l := (*Layer)(c)
	t := l.current.Load()
	if t == nil {
		conn.Close()
		return
	}
	l.mu.Lock()
	servers, port := l.servers, l.sshPort
	l.mu.Unlock()
	if byIP && isServerTarget(target, servers, port) {
		conn.Close()
		return
	}
	t.ServeConn(conn, target, byIP)
}
