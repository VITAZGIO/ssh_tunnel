package lanscan

import (
	"encoding/binary"
	"net"
	"net/netip"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows: ARP-запрос (SendARP) — ответит любое устройство сети, даже с
// закрытым ping, и сразу даст MAC; задержка — по ping (IcmpSendEcho, прав не
// нужно), не ответил на ping — по времени ARP.

var (
	iphlp         = windows.NewLazySystemDLL("iphlpapi.dll")
	procSendARP   = iphlp.NewProc("SendARP")
	procIcmpOpen  = iphlp.NewProc("IcmpCreateFile")
	procIcmpClose = iphlp.NewProc("IcmpCloseHandle")
	procIcmpEcho  = iphlp.NewProc("IcmpSendEcho")
)

func ipv4(a netip.Addr) uint32 {
	b := a.As4()
	return binary.LittleEndian.Uint32(b[:]) // IPAddr — байты в порядке сети
}

func probe(lan LAN, a netip.Addr) probeResult {
	var mac [8]byte
	n := uint32(len(mac))
	t0 := time.Now()
	r, _, _ := procSendARP.Call(uintptr(ipv4(a)), uintptr(ipv4(lan.Prefix.Addr())),
		uintptr(unsafe.Pointer(&mac[0])), uintptr(unsafe.Pointer(&n)))
	arpRTT := time.Since(t0)
	if r != 0 || n < 6 {
		// Все адреса — в своей подсети: не ответил на ARP — его нет.
		return probeResult{}
	}
	res := probeResult{alive: true, rtt: arpRTT, mac: net.HardwareAddr(append([]byte{}, mac[:6]...))}
	if rtt, ok := icmpPing(a, 800*time.Millisecond); ok {
		res.rtt = rtt
	}
	return res
}

func icmpPing(a netip.Addr, timeout time.Duration) (time.Duration, bool) {
	h, _, _ := procIcmpOpen.Call()
	if h == 0 || h == uintptr(windows.InvalidHandle) {
		return 0, false
	}
	defer procIcmpClose.Call(h)
	data := []byte("ssh_tunnel")
	reply := make([]byte, 128)
	t0 := time.Now()
	n, _, _ := procIcmpEcho.Call(h, uintptr(ipv4(a)), uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)),
		0, uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(timeout.Milliseconds()))
	if n == 0 {
		return 0, false
	}
	// ICMP_ECHO_REPLY: Address, Status, RoundTripTime (мс).
	if binary.LittleEndian.Uint32(reply[4:]) != 0 {
		return 0, false
	}
	if rtt := time.Duration(binary.LittleEndian.Uint32(reply[8:])) * time.Millisecond; rtt > 0 {
		return rtt, true
	}
	return time.Since(t0), true
}

// neighbors — MAC уже есть из SendARP; отдельная таблица не нужна.
func neighbors() map[netip.Addr]net.HardwareAddr { return nil }
