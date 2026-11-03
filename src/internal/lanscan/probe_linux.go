package lanscan

// Linux и Android (GOOS=android — тоже linux): ping без прав — сокет
// ICMP-датаграмм (net.ipv4.ping_group_range; на Android он открыт
// приложениям). Не дали — пробуем TCP. MAC — из /proc/net/arp; Android 10+
// её не даёт, и MAC остаётся «—».

import (
	"bufio"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"time"
)

func probe(_ LAN, a netip.Addr) probeResult {
	if rtt, ok := icmpPing(a, 800*time.Millisecond); ok {
		return probeResult{alive: true, rtt: rtt}
	}
	return tcpProbe(a, 600*time.Millisecond)
}

func icmpPing(a netip.Addr, timeout time.Duration) (time.Duration, bool) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMP)
	if err != nil {
		return 0, false
	}
	defer syscall.Close(fd)
	if dialControl != nil {
		if dialControl("udp4", a.String()+":0", fdConn(fd)) != nil {
			return 0, false
		}
	}
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	// Эхо-запрос: тип 8; id ставит ядро, контрольную сумму — тоже.
	msg := []byte{8, 0, 0, 0, 0, 0, 0, 1, 's', 's', 'h'}
	sa := &syscall.SockaddrInet4{Addr: a.As4()}
	t0 := time.Now()
	if err := syscall.Sendto(fd, msg, 0, sa); err != nil {
		return 0, false
	}
	buf := make([]byte, 512)
	for time.Since(t0) < timeout {
		n, from, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return 0, false
		}
		if f, ok := from.(*syscall.SockaddrInet4); ok && f.Addr == a.As4() && n > 0 && buf[0] == 0 {
			return time.Since(t0), true
		}
	}
	return 0, false
}

// fdConn — syscall.RawConn над готовым сокетом (для пометки «мимо VPN»).
type fdConn int

func (f fdConn) Control(fn func(fd uintptr)) error { fn(uintptr(f)); return nil }
func (f fdConn) Read(func(fd uintptr) bool) error  { return nil }
func (f fdConn) Write(func(fd uintptr) bool) error { return nil }

func neighbors() map[netip.Addr]net.HardwareAddr {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[netip.Addr]net.HardwareAddr{}
	sc := bufio.NewScanner(f)
	sc.Scan() // заголовок
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) < 4 || p[2] != "0x2" { // 0x2 — запись полная
			continue
		}
		a, err1 := netip.ParseAddr(p[0])
		mac, err2 := net.ParseMAC(p[3])
		if err1 == nil && err2 == nil && mac.String() != "00:00:00:00:00:00" {
			out[a] = mac
		}
	}
	return out
}
