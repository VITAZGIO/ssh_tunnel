package tunnel

// Учёт трафика (ТЗ 5, «Расход трафика»): встречное соединение оборачивается
// в traffic.Wrap с программой и путём, который выбрало ядро. Отдельного
// перехвата нет; пока приложение не работает, соединение не трогаем.

import (
	"net"
	"net/netip"

	"sshtunnel/internal/core"
	"sshtunnel/internal/traffic"
)

// selfName — так в учёте зовётся сама программа (календарь, SSH, файлы).
const selfName = "ssh_tunnel"

func meterRoute(r core.Route) string {
	switch r {
	case core.RouteDirect:
		return traffic.RouteDirect
	case core.RouteMesh:
		return traffic.RouteMesh
	}
	return traffic.RouteServer
}

// metered — remote с учётом байтов программы proc по пути route.
func metered(remote net.Conn, proc string, route core.Route) net.Conn {
	if !traffic.On() {
		return remote
	}
	return traffic.Wrap(remote, proc, meterRoute(route))
}

// owner — чьё соединение из стека, если программу не узнали по порту
// (телефон: владельца знает только система, см. Config.OwnerOf). Спрашиваем
// только ради учёта — на решение «куда» это не влияет.
func (t *Tunnel) owner(proc string, conn net.Conn) string {
	if proc != "" || t.cfg.OwnerOf == nil || !traffic.On() {
		return proc
	}
	// Закрытое приложением соединение стек gVisor отдаёт без адресов (nil):
	// .String() у nil падал и ронял всё приложение на телефоне.
	ra, la := conn.RemoteAddr(), conn.LocalAddr()
	if ra == nil || la == nil {
		return ""
	}
	src, err1 := netip.ParseAddrPort(ra.String())
	dst, err2 := netip.ParseAddrPort(la.String())
	if err1 != nil || err2 != nil {
		return ""
	}
	return t.cfg.OwnerOf(6, src, dst)
}

// udpOwner — то же для UDP: у системы владелец ищется по своему адресу
// приложения, куда шло — не важно.
func (t *Tunnel) udpOwner(proc string, src netip.AddrPort) string {
	if proc != "" || t.cfg.OwnerOf == nil || !traffic.On() {
		return proc
	}
	return t.cfg.OwnerOf(17, src, netip.AddrPort{})
}
