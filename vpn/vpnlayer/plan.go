// Пакет vpnlayer — режим VPN для Windows и Linux: виртуальный сетевой адаптер
// (Wintun на Windows, TUN на Linux), в который система отдаёт весь трафик, и
// сетевой стек поверх него, который превращает пакеты обратно в соединения и
// отдаёт их в тот же туннель, что и прокси-режим.
//
// Раскладка: layer.go — общая часть (стек, DNS, переключение туннеля),
// sys_windows.go и sys_linux.go — всё, что делается средствами самой ОС:
// адаптер, адреса, маршруты, DNS системы, привязка своих сокетов к настоящей
// сети. В этом файле — решения, не зависящие от ОС: какие адреса и маршруты
// ставить, какой интерфейс считать «настоящим». Они проверяются тестами на
// любой системе.
package vpnlayer

import (
	"net"
	"net/netip"
	"strconv"

	"sshtunnel/internal/core"
)

const (
	// adapterName — как адаптер называется в «Сетевых подключениях» Windows.
	// На Linux имя интерфейса ограничено 15 символами и принято короткое —
	// там свой linuxIfName.
	adapterName = "ssh_tunnel"
	// adapterMTU — пакеты дальше адаптера не уходят (соединения собираются
	// заново у нас), поэтому обычный размер Ethernet — безопасный выбор.
	adapterMTU = 1500
	// fakeNet — подставные адреса для имён (core.FakeNet).
	fakeNet = "198.18.128.0/17"
)

// Адреса адаптера, наш DNS и маршруты всего интернета решает ядро
// (core.PlanVPN) — здесь только короткие имена для кода платформы.
var (
	adapterPrefix4 = core.AdapterPrefix4
	adapterPrefix6 = core.AdapterPrefix6
	dnsAddr        = core.DNSAddr
	routes4        = core.InternetRoutes4
	routes6        = core.InternetRoutes6
)

// pinnable — к этому адресу имеет смысл ставить маршрут через настоящую сеть
// на время прямого соединения: обычный адрес в интернете. Локальные сети,
// служебные адреса и подсеть самого адаптера — нет: их маршруты система и
// так знает лучше, а маршрут «через роутер» к соседу по сети его бы сломал.
func pinnable(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsMulticast() || a.IsUnspecified() ||
		a.IsLinkLocalUnicast() || a.IsPrivate() || adapterPrefix4.Masked().Contains(a) {
		return false
	}
	// 100.64.0.0/10 — mesh-VPN (NetBird, Tailscale): у них свои маршруты.
	return !netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}

// defaultRoute — маршрут по умолчанию одной из сетевых карт.
type defaultRoute struct {
	luid    uint64
	ifIndex uint32
	// metric — метрика маршрута плюс метрика интерфейса: так её считает
	// сама Windows, выбирая, куда слать.
	metric uint32
	// nextHop — роутер этой сети; пустой у соединений «точка-точка».
	nextHop netip.Addr
}

// pickDefault выбирает интерфейс, через который Windows ходила бы в интернет
// без нас: маршрут по умолчанию с наименьшей метрикой, кроме нашего адаптера.
func pickDefault(routes []defaultRoute, ours uint64) (defaultRoute, bool) {
	var best defaultRoute
	found := false
	for _, r := range routes {
		if r.luid == ours || r.ifIndex == 0 {
			continue
		}
		if !found || r.metric < best.metric {
			best, found = r, true
		}
	}
	return best, found
}

// fallbackDNS — на случай, если у настоящей сети не нашлось ни одного
// DNS-сервера (так бывает на странно настроенных сетях).
var fallbackDNS = []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}

// pickDNSServer решает, куда на самом деле слать запрос, который резолвер Go
// собрался отправить на requested. Резолвер берёт список серверов из
// настроек всех адаптеров, включая наш, — а спросить наш адрес значило бы
// спросить самих себя по кругу. Поэтому: сервер настоящей сети — как есть,
// всё остальное — на первый сервер настоящей сети.
func pickDNSServer(requested string, physical []netip.Addr) string {
	if len(physical) == 0 {
		physical = fallbackDNS
	}
	if ap, err := netip.ParseAddrPort(requested); err == nil {
		for _, a := range physical {
			if a == ap.Addr().Unmap() {
				return ap.String()
			}
		}
	}
	return netip.AddrPortFrom(physical[0], 53).String()
}

// isServerTarget — соединение ведёт на сам SSH-сервер. Такое соединение,
// пришедшее из адаптера, означает петлю: наш собственный сокет почему-то не
// удалось привязать к настоящей сети. Вести его в туннель нельзя — он
// поедет сам в себя.
func isServerTarget(target string, serverIPs []netip.Addr, sshPort int) bool {
	host, port, err := net.SplitHostPort(target)
	if err != nil || port != strconv.Itoa(sshPort) {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, s := range serverIPs {
		if s == addr.Unmap() {
			return true
		}
	}
	return false
}
