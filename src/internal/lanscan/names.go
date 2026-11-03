package lanscan

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// resolveNames — имена найденных устройств: обратный DNS (роутер знает
// имена из DHCP), mDNS (ESPHome, Apple, принтеры, Linux с avahi), NetBIOS
// (Windows, Samba). Каждое — с коротким тайм-аутом, всё параллельно.
func resolveNames(ctx context.Context, lan LAN, addrs []netip.Addr) map[netip.Addr]string {
	out := map[netip.Addr]string{}
	var mu sync.Mutex
	set := func(a netip.Addr, n string, strong bool) {
		n = strings.TrimSuffix(strings.TrimSpace(n), ".")
		if n == "" {
			return
		}
		mu.Lock()
		if _, ok := out[a]; !ok || strong {
			out[a] = n
		}
		mu.Unlock()
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for a, n := range mdnsNames(lan, addrs, 1500*time.Millisecond) {
			set(a, strings.TrimSuffix(n, ".local"), false)
		}
	}()
	sem := make(chan struct{}, 16)
	for _, a := range addrs {
		wg.Add(1)
		go func(a netip.Addr) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if n := netbiosName(a, 900*time.Millisecond); n != "" {
				set(a, n, false)
			}
			c, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
			defer cancel()
			if names, err := net.DefaultResolver.LookupAddr(c, a.String()); err == nil && len(names) > 0 {
				// Имя от роутера — главное: его человек сам и задавал.
				n := strings.TrimSuffix(names[0], ".")
				for _, suf := range []string{".lan", ".local", ".home", ".localdomain"} {
					n = strings.TrimSuffix(n, suf)
				}
				set(a, n, true)
			}
		}(a)
	}
	wg.Wait()
	return out
}

// netbiosName — имя компьютера по NetBIOS (запрос NBSTAT на порт 137).
func netbiosName(a netip.Addr, timeout time.Duration) string {
	c, err := dialer(timeout).Dial("udp4", netip.AddrPortFrom(a, 137).String())
	if err != nil {
		return ""
	}
	defer c.Close()
	q := []byte{0x13, 0x37, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0x20, 'C', 'K'}
	for i := 0; i < 15; i++ {
		q = append(q, 'A', 'A')
	}
	q = append(q, 0, 0, 0x21, 0, 1)
	c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write(q); err != nil {
		return ""
	}
	b := make([]byte, 1500)
	n, err := c.Read(b)
	if err != nil || n < 57 {
		return ""
	}
	cnt := int(b[56])
	for i := 0; i < cnt; i++ {
		off := 57 + i*18
		if off+18 > n {
			break
		}
		name := strings.TrimRight(string(b[off:off+15]), " \x00")
		suffix := b[off+15]
		group := binary.BigEndian.Uint16(b[off+16:])&0x8000 != 0
		if suffix == 0 && !group && name != "" {
			return name
		}
	}
	return ""
}

// mdnsNames — обратный запрос PTR «d.c.b.a.in-addr.arpa» по mDNS: на
// групповой адрес (ответ придёт нам напрямую, мы не на порту 5353) и каждому
// адресу напрямую — кто-то отвечает только так.
func mdnsNames(lan LAN, addrs []netip.Addr, wait time.Duration) map[netip.Addr]string {
	out := map[netip.Addr]string{}
	lc := net.ListenConfig{Control: dialControl}
	pc, err := lc.ListenPacket(context.Background(), "udp4", net.JoinHostPort(lan.Prefix.Addr().String(), "0"))
	if err != nil {
		pc, err = lc.ListenPacket(context.Background(), "udp4", ":0")
		if err != nil {
			return out
		}
	}
	defer pc.Close()
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	for i, a := range addrs {
		q := ptrQuery(uint16(i+1), a)
		pc.WriteTo(q, group)
		pc.WriteTo(q, &net.UDPAddr{IP: a.AsSlice(), Port: 5353})
	}
	pc.SetReadDeadline(time.Now().Add(wait))
	b := make([]byte, 9000)
	for {
		n, from, err := pc.ReadFrom(b)
		if err != nil {
			return out
		}
		ua, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		src, _ := netip.AddrFromSlice(ua.IP.To4())
		if ip, name := parsePTR(b[:n]); name != "" {
			if ip.IsValid() {
				src = ip
			}
			if _, have := out[src]; !have {
				out[src] = name
			}
		}
	}
}

func reverseName(a netip.Addr) string {
	b := a.As4()
	return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa", b[3], b[2], b[1], b[0])
}

func ptrQuery(id uint16, a netip.Addr) []byte {
	q := []byte{byte(id >> 8), byte(id), 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(reverseName(a), ".") {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	// PTR, класс IN с битом «ответить напрямую» (QU).
	return append(q, 0, 0, 0x0c, 0x80, 0x01)
}

// readName — имя DNS со сжатием; возвращает имя и смещение после него.
func readName(m []byte, off int) (string, int, bool) {
	var parts []string
	end := -1
	for hops := 0; hops < 20; hops++ {
		if off >= len(m) {
			return "", 0, false
		}
		l := int(m[off])
		switch {
		case l == 0:
			if end < 0 {
				end = off + 1
			}
			return strings.Join(parts, "."), end, true
		case l&0xc0 == 0xc0:
			if off+1 >= len(m) {
				return "", 0, false
			}
			if end < 0 {
				end = off + 2
			}
			off = int(binary.BigEndian.Uint16(m[off:])) & 0x3fff
		default:
			if off+1+l > len(m) {
				return "", 0, false
			}
			parts = append(parts, string(m[off+1:off+1+l]))
			off += 1 + l
		}
	}
	return "", 0, false
}

// parsePTR — первый ответ PTR: чей адрес (из имени запроса) и имя.
func parsePTR(m []byte) (netip.Addr, string) {
	if len(m) < 12 || m[2]&0x80 == 0 {
		return netip.Addr{}, ""
	}
	qd := int(binary.BigEndian.Uint16(m[4:]))
	an := int(binary.BigEndian.Uint16(m[6:])) + int(binary.BigEndian.Uint16(m[8:])) + int(binary.BigEndian.Uint16(m[10:]))
	off := 12
	for i := 0; i < qd; i++ {
		_, o, ok := readName(m, off)
		if !ok {
			return netip.Addr{}, ""
		}
		off = o + 4
	}
	for i := 0; i < an; i++ {
		owner, o, ok := readName(m, off)
		if !ok || o+10 > len(m) {
			return netip.Addr{}, ""
		}
		typ := binary.BigEndian.Uint16(m[o:])
		rdl := int(binary.BigEndian.Uint16(m[o+8:]))
		rd := o + 10
		if rd+rdl > len(m) {
			return netip.Addr{}, ""
		}
		if typ == 12 && strings.HasSuffix(owner, ".in-addr.arpa") {
			name, _, ok := readName(m, rd)
			if ok {
				return addrOfReverse(owner), name
			}
		}
		off = rd + rdl
	}
	return netip.Addr{}, ""
}

func addrOfReverse(owner string) netip.Addr {
	p := strings.Split(strings.TrimSuffix(owner, ".in-addr.arpa"), ".")
	if len(p) != 4 {
		return netip.Addr{}
	}
	a, err := netip.ParseAddr(p[3] + "." + p[2] + "." + p[1] + "." + p[0])
	if err != nil {
		return netip.Addr{}
	}
	return a
}
