// Package lanscan — плагин «Сканер сети» (ТЗ 5, п. 11):
// кто сейчас в локальной сети (Wi-Fi или провод), без запоминания.
//
// Подсеть — у активного Wi-Fi/Ethernet (адаптер VPN и сеть устройств не в
// счёт). Каждый адрес опрашивается: на Windows — ARP-запросом (SendARP:
// отвечает даже тот, кто молчит на ping) и ping, на Linux и Android — ping
// без прав (ICMP-датаграмма) или попыткой TCP; MAC — из ответа ARP или
// таблицы соседей (/proc/net/arp; Android 10+ её не даёт — MAC «—»). Затем у
// найденных — имя (DNS, mDNS, NetBIOS), производитель по базе OUI и краткая
// проверка портов 22/80/443. Полного скана портов нет.
package lanscan

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Host — найденное устройство.
type Host struct {
	IP     string  `json:"ip"`
	MAC    string  `json:"mac,omitempty"` // "" — не узнали (на телефоне — запрет системы)
	Random bool    `json:"randomMac,omitempty"`
	Name   string  `json:"name,omitempty"`
	Vendor string  `json:"vendor,omitempty"`
	RTTMs  float64 `json:"rttMs,omitempty"`
	Ports  []int   `json:"ports"` // открытые из 22, 80, 443
	Self   bool    `json:"self,omitempty"`

	mac net.HardwareAddr
}

// Result — состояние сканирования (только в памяти).
type Result struct {
	Running bool   `json:"running"`
	Subnet  string `json:"subnet,omitempty"`
	Iface   string `json:"iface,omitempty"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Error   string `json:"error,omitempty"`
	Started string `json:"started,omitempty"`
	Took    int64  `json:"tookMs,omitempty"`
	Hosts   []Host `json:"hosts"`
}

// Сколько адресов опрашивать разом: быстро, но без потопа.
const parallel = 48

// maxHosts — больше /22 не сканируем: берём /24 вокруг своего адреса.
const maxHosts = 1022

var ports = []int{22, 80, 443}

// LAN — своя локальная сеть.
type LAN struct {
	Prefix netip.Prefix // адрес устройства и маска
	Iface  string
	MAC    net.HardwareAddr
}

// lanSource — откуда взять сеть, если net.Interfaces не работает (Android
// 11+ не даёт netlink): строка «192.168.1.50/24» или "".
var (
	srcMu     sync.Mutex
	lanSource func() string
)

// SetLANSource — сеть спросит платформа (телефон).
func SetLANSource(f func() string) {
	srcMu.Lock()
	lanSource = f
	srcMu.Unlock()
}

var errNoLAN = errors.New("нет подключения к Wi-Fi или проводной сети")

// skipIface — адаптеры, которые не локальная сеть: VPN (свой и чужие),
// виртуальные.
func skipIface(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"ssh_tunnel", "wintun", "tun", "tap", "wg", "vpn", "zt", "tailscale", "netbird", "docker", "veth", "br-", "virbr", "vethernet", "vmnet", "virtualbox", "loopback", "radmin", "hamachi"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// lanAddr — подходит ли адрес как своя локальная сеть.
func lanAddr(p netip.Prefix) bool {
	a := p.Addr()
	if !a.Is4() || !a.IsPrivate() {
		return false
	}
	// 198.18/15 (подставные адреса, сеть устройств) IsPrivate не считает,
	// но и CGNAT 100.64/10 — не своя сеть.
	return p.Bits() >= 16 && p.Bits() <= 30
}

// FindLAN — активная локальная сеть.
func FindLAN() (LAN, error) {
	srcMu.Lock()
	f := lanSource
	srcMu.Unlock()
	if f != nil {
		if s := f(); s != "" {
			if p, err := netip.ParsePrefix(s); err == nil && lanAddr(p) {
				return LAN{Prefix: p, Iface: "Wi-Fi"}, nil
			}
		}
		return LAN{}, errNoLAN
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return LAN{}, err
	}
	var found []LAN
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || skipIface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP.To4())
			if !ok {
				continue
			}
			bits, _ := ipn.Mask.Size()
			p := netip.PrefixFrom(ip, bits)
			if lanAddr(p) {
				found = append(found, LAN{Prefix: p, Iface: ifc.Name, MAC: ifc.HardwareAddr})
			}
		}
	}
	if len(found) == 0 {
		return LAN{}, errNoLAN
	}
	// Несколько — берём ту, через которую идёт путь в интернет мимо VPN
	// (её адрес выбирает система для внешнего адреса), иначе первую.
	if c, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		la, _ := netip.ParseAddrPort(c.LocalAddr().String())
		c.Close()
		for _, l := range found {
			if l.Prefix.Addr() == la.Addr() {
				return l, nil
			}
		}
	}
	return found[0], nil
}

// targets — адреса подсети без своего, сети и широковещательного.
func targets(p netip.Prefix) []netip.Addr {
	if p.Bits() < 32-10 { // больше /22 — только своя /24
		p = netip.PrefixFrom(p.Addr(), 24)
	}
	base := p.Masked().Addr().As4()
	n := uint32(1) << (32 - p.Bits())
	start := binary.BigEndian.Uint32(base[:])
	var out []netip.Addr
	for i := uint32(1); i < n-1 && len(out) < maxHosts; i++ {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], start+i)
		a := netip.AddrFrom4(b)
		if a != p.Addr() {
			out = append(out, a)
		}
	}
	return out
}

// probe — ответил ли адрес; mac — если узнали сразу (ARP на Windows).
type probeResult struct {
	alive bool
	rtt   time.Duration
	mac   net.HardwareAddr
}

// tcpProbe — «жив ли» без ping: соединение или отказ — оба значат, что
// устройство есть. Заодно система узнаёт его MAC (ARP).
func tcpProbe(a netip.Addr, timeout time.Duration) probeResult {
	for _, port := range []int{80, 443, 22, 6053, 8080} {
		t0 := time.Now()
		c, err := dialer(timeout).Dial("tcp", netip.AddrPortFrom(a, uint16(port)).String())
		if err == nil {
			c.Close()
			return probeResult{alive: true, rtt: time.Since(t0)}
		}
		if isRefused(err) {
			return probeResult{alive: true, rtt: time.Since(t0)}
		}
		var ne net.Error
		if !(errors.As(err, &ne) && ne.Timeout()) {
			// «нет маршрута», «хост недоступен» — дальше пробовать незачем.
			return probeResult{}
		}
	}
	return probeResult{}
}

func isRefused(err error) bool {
	s := err.Error()
	return strings.Contains(s, "refused") || strings.Contains(s, "actively refused") ||
		strings.Contains(s, "reset by peer")
}

// dialControl — пометка сокетов на телефоне (мимо VPN); nil — не нужно.
var dialControl func(network, address string, c syscall.RawConn) error

// SetDialControl — как помечать сокеты сканера (телефон: мимо VPN).
func SetDialControl(f func(network, address string, c syscall.RawConn) error) { dialControl = f }

func dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: dialControl}
}

// Scanner — одно сканирование за раз; результат — в памяти до следующего.
type Scanner struct {
	mu   sync.Mutex
	res  Result
	stop context.CancelFunc
}

var scanner = &Scanner{res: Result{Hosts: []Host{}}}

// Get — текущее состояние.
func (s *Scanner) Get() Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.res
	r.Hosts = append([]Host{}, s.res.Hosts...)
	return r
}

// Start — начать сканирование (идёт — ничего не делает).
func (s *Scanner) Start() Result {
	s.mu.Lock()
	if s.res.Running {
		s.mu.Unlock()
		return s.Get()
	}
	lan, err := FindLAN()
	if err != nil {
		s.res = Result{Error: err.Error(), Hosts: []Host{}}
		s.mu.Unlock()
		return s.Get()
	}
	list := targets(lan.Prefix)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	s.stop = cancel
	s.res = Result{Running: true, Subnet: lan.Prefix.Masked().String(), Iface: lan.Iface,
		Total: len(list), Started: time.Now().Format(time.RFC3339), Hosts: []Host{}}
	s.mu.Unlock()
	go s.run(ctx, lan, list)
	return s.Get()
}

// Stop — прервать и забыть (приложение убрали).
func (s *Scanner) Stop() {
	s.mu.Lock()
	if s.stop != nil {
		s.stop()
	}
	s.res = Result{Hosts: []Host{}}
	s.mu.Unlock()
}

func (s *Scanner) run(ctx context.Context, lan LAN, list []netip.Addr) {
	t0 := time.Now()
	defer func() {
		s.mu.Lock()
		s.res.Running = false
		s.res.Took = time.Since(t0).Milliseconds()
		s.mu.Unlock()
	}()

	found := map[netip.Addr]*Host{}
	var fmu sync.Mutex
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for _, a := range list {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(a netip.Addr) {
			defer func() { <-sem; wg.Done() }()
			r := probe(lan, a)
			fmu.Lock()
			if r.alive {
				h := &Host{IP: a.String(), mac: r.mac, Ports: []int{}}
				if r.rtt > 0 {
					h.RTTMs = ms(r.rtt)
				}
				found[a] = h
			}
			fmu.Unlock()
			s.mu.Lock()
			s.res.Done++
			s.mu.Unlock()
		}(a)
	}
	wg.Wait()

	// Таблица соседей: и MAC, и те, кто ответил только на ARP.
	for a, mac := range neighbors() {
		if !lan.Prefix.Masked().Contains(a) || a == lan.Prefix.Addr() {
			continue
		}
		if h, ok := found[a]; ok {
			if h.mac == nil {
				h.mac = mac
			}
		} else {
			found[a] = &Host{IP: a.String(), mac: mac, Ports: []int{}}
		}
	}
	self := &Host{IP: lan.Prefix.Addr().String(), mac: lan.MAC, Self: true, Ports: []int{}}
	found[lan.Prefix.Addr()] = self
	s.publish(found)

	// Имена и порты — у найденных, параллельно.
	var addrs []netip.Addr
	for a := range found {
		addrs = append(addrs, a)
	}
	names := resolveNames(ctx, lan, addrs)
	var pwg sync.WaitGroup
	for a, h := range found {
		if h.Self {
			continue
		}
		pwg.Add(1)
		go func(a netip.Addr, h *Host) {
			defer pwg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			for _, p := range ports {
				c, err := dialer(700*time.Millisecond).Dial("tcp", netip.AddrPortFrom(a, uint16(p)).String())
				if err == nil {
					c.Close()
					fmu.Lock()
					h.Ports = append(h.Ports, p)
					fmu.Unlock()
				}
			}
		}(a, h)
	}
	pwg.Wait()
	fmu.Lock()
	for a, n := range names {
		if h, ok := found[a]; ok {
			h.Name = n
		}
	}
	if self.Name == "" {
		self.Name = selfName()
	}
	fmu.Unlock()
	s.publish(found)
}

func selfName() string {
	if n, err := os.Hostname(); err == nil {
		return n
	}
	return ""
}

func (s *Scanner) publish(found map[netip.Addr]*Host) {
	hosts := make([]Host, 0, len(found))
	for _, h := range found {
		c := *h
		if len(c.mac) >= 6 {
			c.MAC = strings.ToUpper(c.mac.String())
			c.Random = RandomMAC(c.mac)
			c.Vendor = Vendor(c.mac)
		}
		c.Ports = append([]int{}, h.Ports...)
		sort.Ints(c.Ports)
		hosts = append(hosts, c)
	}
	sort.Slice(hosts, func(i, j int) bool {
		a, _ := netip.ParseAddr(hosts[i].IP)
		b, _ := netip.ParseAddr(hosts[j].IP)
		return a.Less(b)
	})
	s.mu.Lock()
	s.res.Hosts = hosts
	s.mu.Unlock()
}

func ms(d time.Duration) float64 {
	v := float64(d.Microseconds()) / 1000
	if v < 0.1 {
		v = 0.1
	}
	return float64(int(v*10)) / 10
}
