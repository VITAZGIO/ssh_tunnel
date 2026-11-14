package mesh

// Поиск устройств без сервера: маяки в локальной сети и по запомненным
// адресам.
//
// Пока meshd на связи, маяков нет вовсе: прямые пути сводит он. Сервер
// пропал (или устройство запустилось без него), а к кому-то из запомненных
// устройств прямого пути нет — устройство шлёт маяк:
//   - широковещательно в свою сеть (255.255.255.255 и broadcast каждой
//     локальной сети) на порт beaconPort;
//   - и адресно — на последние рабочие адреса каждого такого устройства
//     (так находится и уже пробитый путь через NAT, и соседи, до которых
//     широковещание не доходит).
//
// Маяк уходит с сокета прямых соединений — того же, что мимо туннеля (на
// Android и в режиме VPN он помечен), поэтому его адрес отправителя и есть
// адрес для QUIC. В маяке: id сети (хэш ключа, не сам ключ), публичный id
// устройства (PeerID) и его адрес, порт, отпечаток ключа, время; подпись
// HMAC-SHA256 ключом сети.
//
// Настоящего id устройства (Config.DeviceID) в маяке нет (ТЗ 20): у meshd он
// служит устройству паролем, а ключ сети есть у каждого участника — любой из
// них в той же локалке прочитал бы его и вошёл к meshd под чужим именем.
// Маяки до ТЗ 20 несли его в поле "dev"; получатель этого поля никогда не
// читал (узнаёт устройство по адресу и отпечатку из списка meshd), поэтому
// старые и новые версии находят друг друга без условий: старые маяки
// принимаются как прежде (поле "dev" отбрасывается при разборе и нигде не
// хранится), новые старым получателям тоже годятся — "id" они пропускают.
//
// Получатель проверяет подпись, свежесть (окно beaconWindow, время от этого
// устройства только растёт — повтор старого маяка не пройдёт) и что
// устройство с таким адресом и отпечатком было в последнем списке от meshd.
// Дальше — то же рукопожатие QUIC с проверкой отпечатка. Звонит устройство
// с меньшим адресом (как при «ничьей» в signal); большее в ответ шлёт ему
// адресный маяк — так путь находится, даже если широковещание дошло только в
// одну сторону.
//
// Частота: в обычной жизни (сервер на связи или все прямые пути есть) — ни
// одного пакета. Без сервера, пока кого-то не хватает: сразу, через 2 с,
// через 4 с, дальше раз в ~30 с (±30 %). Маяк — один кадр ~300 байт; Wi-Fi
// отдаёт широковещание спящим устройствам по DTIM, на который они и так
// просыпаются, так что раз в полминуты ноутбук не будит. 30 с — сколько
// ждать соединения после того, как сосед пришёл в сеть без сервера.
// IPv6-мультикаст не делаем: сокет прямых соединений только IPv4, а в
// домашней сети IPv4 есть всегда.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"strconv"
	"time"
)

const (
	beaconWindow = 2 * time.Minute
	beaconEvery  = 30 * time.Second
	// beaconCheck — не чаще проверять «живое» соединение с устройством,
	// которое шлёт маяки.
	beaconCheck = 20 * time.Second
)

// defaultBeaconPort — UDP-порт маяков (рядом с портами служб сети
// устройств 47830–47837). 0 — маяков нет (тесты).
var defaultBeaconPort = 47839

var beaconMagic = [4]byte{0, 'b', 'c', 'n'}

type beacon struct {
	V   int    `json:"v"`
	Net string `json:"net"`
	// ID — публичный id устройства (PeerID), только для сведения: ни для
	// входа, ни для решений он не нужен. Поля "dev" (настоящий id у старых
	// версий) в структуре нет намеренно — чтобы его нельзя было ни отправить,
	// ни сохранить.
	ID   string `json:"id,omitempty"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
	FP   string `json:"fp"`
	T    int64  `json:"t"` // unix, мс
}

func beaconKeyFor(netKey string) []byte {
	m := hmac.New(sha256.New, []byte(netKey))
	m.Write([]byte("ssh_tunnel mesh beacon v1"))
	return m.Sum(nil)
}

func encodeBeacon(key []byte, b beacon) []byte {
	body, _ := json.Marshal(b)
	out := append(append([]byte(nil), beaconMagic[:]...), body...)
	m := hmac.New(sha256.New, key)
	m.Write(out)
	return m.Sum(out)
}

func decodeBeacon(key []byte, raw []byte, now time.Time) (beacon, error) {
	if len(raw) < 4+2+sha256.Size || [4]byte(raw[:4]) != beaconMagic {
		return beacon{}, errors.New("не маяк")
	}
	body, sum := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	m := hmac.New(sha256.New, key)
	m.Write(body)
	if !hmac.Equal(m.Sum(nil), sum) {
		return beacon{}, errors.New("подпись не сошлась")
	}
	var b beacon
	if err := json.Unmarshal(body[4:], &b); err != nil {
		return beacon{}, err
	}
	if d := now.Sub(time.UnixMilli(b.T)); d > beaconWindow || d < -beaconWindow {
		return beacon{}, errors.New("маяк устарел")
	}
	return b, nil
}

func isBeacon(b []byte) bool { return len(b) >= 4 && [4]byte(b[:4]) == beaconMagic }

// beaconMsg — свой маяк; nil — своего адреса в сети ещё не знаем.
func (p *p2p) beaconMsg() []byte {
	self := p.c.selfIP()
	la, _ := p.pc.LocalAddr().(*net.UDPAddr)
	if !self.IsValid() || la == nil {
		return nil
	}
	return encodeBeacon(p.bkey, beacon{V: 1, Net: p.netID, ID: PeerID(p.c.cfg.DeviceID), IP: self.String(),
		Port: la.Port, FP: p.fp, T: time.Now().UnixMilli()})
}

// startBeacons — приёмник широковещательных маяков и цикл отправки.
func (p *p2p) startBeacons() {
	listen := p.c.beaconListen
	if listen == nil && defaultBeaconPort > 0 {
		listen = func() (net.PacketConn, error) {
			return net.ListenPacket("udp4", net.JoinHostPort("", strconv.Itoa(defaultBeaconPort)))
		}
	}
	if listen != nil {
		if ln, err := listen(); err != nil {
			// Порт занят (вторая копия программы) — свои маяки всё равно
			// шлём, а чужие придут адресно на сокет прямых соединений.
			p.c.log("info", "Сеть устройств: маяки в локальной сети не слушаются: "+err.Error())
		} else {
			p.bln = ln
			p.wg.Add(1)
			go p.beaconListen(ln)
		}
	}
	p.wg.Add(1)
	go p.beaconLoop()
}

func (p *p2p) beaconListen(ln net.PacketConn) {
	defer p.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, from, err := ln.ReadFrom(buf)
		if err != nil {
			return
		}
		if isBeacon(buf[:n]) {
			p.onBeacon(buf[:n], from)
		}
	}
}

// kick — подтолкнуть цикл маяков (сервер пропал, прямой путь пропал).
func (p *p2p) kick() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *p2p) beaconLoop() {
	defer p.wg.Done()
	burst := 0
	for {
		var wait time.Duration // 0 — ждать толчка
		if !p.c.online() {
			if miss := p.missing(); len(miss) > 0 {
				p.sendBeacons(miss, true)
				switch burst {
				case 0:
					wait = 2 * time.Second
				case 1:
					wait = 4 * time.Second
				default:
					wait = beaconEvery*7/10 + rand.N(beaconEvery*6/10)
				}
				burst++
			}
		} else {
			burst = 0
		}
		var tc <-chan time.Time
		var t *time.Timer
		if wait > 0 {
			t = time.NewTimer(wait)
			tc = t.C
		}
		select {
		case <-p.ctx.Done():
			if t != nil {
				t.Stop()
			}
			return
		case <-p.wake:
		case <-tc:
		}
		if t != nil {
			t.Stop()
		}
	}
}

// missing — устройства из списка, к которым можно пойти без сервера
// (отпечаток известен), но прямого пути сейчас нет.
func (p *p2p) missing() []cachedPeer {
	self := p.c.selfIP().String()
	var out []cachedPeer
	for _, pr := range p.c.cache.snapshot().Peers {
		if pr.FP == "" || pr.IP == self {
			continue
		}
		if a, err := netip.ParseAddr(pr.IP); err != nil || serverNodes.Contains(a) {
			continue
		}
		if ok, _ := p.direct(pr.IP); !ok {
			out = append(out, pr)
		}
	}
	return out
}

// sendBeacons — адресно на запомненные адреса устройств to и (broadcast)
// широковещательно.
func (p *p2p) sendBeacons(to []cachedPeer, broadcast bool) {
	msg := p.beaconMsg()
	if msg == nil {
		return
	}
	if broadcast {
		targets := p.c.beaconTargets
		if targets == nil && defaultBeaconPort > 0 {
			targets = func() []*net.UDPAddr { return broadcastTargets(defaultBeaconPort) }
		}
		if targets != nil {
			for _, a := range targets() {
				p.tr.WriteTo(msg, a)
			}
		}
	}
	for _, pr := range to {
		for _, a := range parseCands(pr.Cands) {
			p.tr.WriteTo(msg, a)
		}
	}
}

// onBeacon — маяк от другого устройства (широковещательный или адресный).
func (p *p2p) onBeacon(raw []byte, from net.Addr) {
	ua, ok := from.(*net.UDPAddr)
	if !ok || ua.IP.To4() == nil {
		return
	}
	b, err := decodeBeacon(p.bkey, raw, time.Now())
	if err != nil || b.Net != p.netID {
		return
	}
	self := p.c.selfIP().String()
	if b.IP == self || b.FP == "" {
		return
	}
	// Только устройства из последнего списка meshd и с тем отпечатком, что
	// пришёл через него: ключ сети есть и у убранных устройств.
	pr, known := p.c.cache.peer(b.IP)
	if !known || pr.FP != b.FP {
		return
	}
	if !p.fresh(b.IP, b.T) {
		return // повтор
	}

	addr := &net.UDPAddr{IP: ua.IP.To4(), Port: ua.Port}
	if ok, _ := p.direct(b.IP); ok {
		// Маяки шлёт тот, кому не хватает прямого пути, — возможно, наш к
		// нему уже мёртвый (устройство перезапустилось), а QUIC узнает об
		// этом только через полминуты. Проверить сейчас.
		if !p.rateOK(p.checkAt, b.IP, beaconCheck) {
			return
		}
		go func() {
			if !p.measure(b.IP) {
				p.beaconAct(self, b, addr)
			}
		}()
		return
	}
	p.beaconAct(self, b, addr)
}

func (p *p2p) beaconAct(self string, b beacon, addr *net.UDPAddr) {
	if self < b.IP {
		go p.beaconDial(b.IP, b.FP, addr)
		return
	}
	// Звонить должно оно: ответить адресным маяком.
	if p.rateOK(p.replyAt, b.IP, 2*time.Second) {
		if msg := p.beaconMsg(); msg != nil {
			p.tr.WriteTo(msg, addr)
		}
	}
}

// fresh — время маяка от устройства ip новее всех прежних (и запомнить):
// записанный и повторенный маяк не пройдёт даже внутри окна свежести.
func (p *p2p) fresh(ip string, t int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t <= p.beaconT[ip] {
		return false
	}
	p.beaconT[ip] = t
	return true
}

// rateOK — прошло ли с прошлого раза для ip не меньше d (и отметить).
func (p *p2p) rateOK(m map[string]time.Time, ip string, d time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := m[ip]; ok && time.Since(t) < d {
		return false
	}
	m[ip] = time.Now()
	return true
}

// beaconDial — QUIC к устройству по адресу из маяка, с проверкой отпечатка.
func (p *p2p) beaconDial(ip, fp string, addr *net.UDPAddr) {
	if !p.rateOK(p.dialAt, ip, 3*time.Second) {
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	conn, err := p.tr.Dial(ctx, addr, p.clientTLS(fp), quicConfig)
	cancel()
	if err != nil {
		return
	}
	if ok, _ := p.direct(ip); ok {
		conn.CloseWithError(0, "") // пока звонили, путь уже нашёлся
		return
	}
	p.adopt(ip, conn)
}

// reach — без сервера попробовать найти прямой путь к устройству ip за d.
func (p *p2p) reach(ip string, d time.Duration) bool {
	pr, ok := p.c.cache.peer(ip)
	if !ok || pr.FP == "" {
		return false
	}
	p.sendBeacons([]cachedPeer{pr}, true)
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok, _ := p.direct(ip); ok {
			return true
		}
		select {
		case <-p.ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return false
}

// broadcastTargets — 255.255.255.255 и broadcast каждой локальной сети
// IPv4 (широковещание «на всех» система шлёт только в один интерфейс).
func broadcastTargets(port int) []*net.UDPAddr {
	out := []*net.UDPAddr{{IP: net.IPv4bcast, Port: port}}
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagBroadcast == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4, mask := n.IP.To4(), n.Mask
			if ip4 == nil || len(mask) != 4 || !ip4.IsPrivate() {
				continue
			}
			bc := make(net.IP, 4)
			for i := range bc {
				bc[i] = ip4[i] | ^mask[i]
			}
			out = append(out, &net.UDPAddr{IP: bc, Port: port})
			if len(out) >= 8 {
				return out
			}
		}
	}
	return out
}
