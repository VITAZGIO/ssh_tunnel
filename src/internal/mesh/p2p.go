package mesh

// Прямые соединения между устройствами (p2p), минуя сервер.
//
// Как в NetBird и Tailscale: соединение сразу идёт через сервер (meshd), а
// параллельно устройства пробуют найти прямой путь.
//
//  1. У каждого устройства один UDP-сокет для прямых соединений. Через него
//     же оно спрашивает у сервера (STUN), каким адресом видно снаружи, и
//     раз в 20 секунд повторяет — чтобы NAT не закрыл «дырку».
//  2. Устройство A посылает через meshd устройству B предложение: свои
//     адреса-кандидаты (внешний и локальные) и отпечаток ключа. B отвечает
//     тем же. meshd идёт по SSH — подменить предложение по дороге нельзя.
//  3. Оба одновременно шлют друг другу короткие UDP-пакеты («пробивание»):
//     каждый NAT видит исходящий пакет и пропускает встречный.
//  4. Как только путь нашёлся, A открывает по нему QUIC (шифрование TLS 1.3,
//     потоки, контроль скорости). Ключи проверяются по отпечаткам из
//     предложения: чужое устройство не встанет посередине.
//  5. Каждое соединение сети устройств — отдельный поток QUIC. Пропал прямой
//     путь — новые соединения снова идут через сервер.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	p2pALPN = "sshtunnel-mesh-p2p"
	// Пакеты пробивания начинаются с нулевого байта: QUIC так никогда не
	// начинается (у него всегда взведён бит 0x40), и транспорт отдаёт их нам.
	punchPing = 1
	punchPong = 2
	// p2pRetry — не чаще, чем раз в столько, пробовать прямой путь к тому же
	// устройству: неудачное пробивание стоит пакетов и времени.
	p2pRetry = 45 * time.Second
	// punchFor — сколько длится одна попытка пробивания.
	punchFor = 8 * time.Second
	// p2pRetryMax — реже этого сами (без соединения от человека) не пробуем:
	// после каждой неудачи пауза удваивается от p2pRetry до этой.
	p2pRetryMax = 12 * time.Minute
)

var punchMagic = [4]byte{0, 'p', '2', 'p'}

// directWire — строка отчёта p2pstat для meshd.
type directWire struct {
	IP    string  `json:"ip"`
	RTTMs float64 `json:"rttMs,omitempty"`
}

// p2p живёт столько же, сколько Client.Run: управляющее соединение с meshd
// рвётся и поднимается заново, а прямые соединения при этом не трогаются.
type p2p struct {
	c      *Client
	pc     net.PacketConn
	tr     *quic.Transport
	ln     *quic.Listener
	cert   tls.Certificate
	fp     string
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Маяки (beacon.go).
	bkey  []byte
	netID string
	bln   net.PacketConn
	wake  chan struct{}

	stunKick chan struct{}

	mu       sync.Mutex
	stun     *net.UDPAddr // STUN сервера, пока он на связи; nil — сервера нет
	public   netip.AddrPort
	peers    map[string]*p2pPeer
	attempts map[string]*p2pAttempt
	stunWait map[[12]byte]chan netip.AddrPort
	beaconT  map[string]int64
	checkAt  map[string]time.Time
	replyAt  map[string]time.Time
	dialAt   map[string]time.Time
}

// send — строка для meshd по текущему управляющему соединению.
func (p *p2p) send(m wire) error { return p.c.sendCtrl(m) }

// setSTUN — сессия с meshd началась (адрес его STUN) или кончилась (nil).
func (p *p2p) setSTUN(a *net.UDPAddr) {
	p.mu.Lock()
	p.stun = a
	if a == nil {
		p.public = netip.AddrPort{}
	}
	p.mu.Unlock()
	select {
	case p.stunKick <- struct{}{}:
	default:
	}
}

func (p *p2p) stunAddr() *net.UDPAddr {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stun
}

type p2pPeer struct {
	conn    quic.Connection
	rtt     time.Duration
	lastTry time.Time
	fails   int // неудачных попыток подряд (для паузы между ними)
}

// retryAfter — пауза перед следующей попыткой сама по себе.
func (peer *p2pPeer) retryAfter() time.Duration {
	d := p2pRetry
	for i := 0; i < peer.fails && d < p2pRetryMax; i++ {
		d *= 2
	}
	return min(d, p2pRetryMax)
}

type p2pAttempt struct {
	id        [16]byte
	peerIP    string
	initiator bool
	fp        string
	cands     []*net.UDPAddr
	answered  chan struct{} // ответ на предложение пришёл (у инициатора)
	path      chan net.Addr // первый подтверждённый путь
	done      chan struct{}
	once      sync.Once
	finished  sync.Once
}

func (a *p2pAttempt) finish() { a.finished.Do(func() { close(a.done) }) }

var quicConfig = &quic.Config{
	HandshakeIdleTimeout: 5 * time.Second,
	MaxIdleTimeout:       30 * time.Second,
	KeepAlivePeriod:      10 * time.Second,
}

// startP2P открывает сокет прямых соединений и начинает слушать. Ключ —
// постоянный (loadIdentity), если есть где его хранить.
func startP2P(ctx context.Context, c *Client, cert tls.Certificate, fp string) (*p2p, error) {
	// quic-go жалуется в журнал, что не смог увеличить буфер сокета, —
	// на скорость прямых соединений между устройствами это почти не влияет.
	os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	listen := c.cfg.ProbeListen
	if listen == nil {
		listen = func(network string) (net.PacketConn, error) { return net.ListenPacket(network, ":0") }
	}
	pc, err := listen("udp4")
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithCancel(ctx)
	p := &p2p{
		c: c, pc: pc, cert: cert, fp: fp, ctx: pctx, cancel: cancel,
		bkey: beaconKeyFor(c.cfg.Key), netID: netID(c.cfg.Key),
		wake: make(chan struct{}, 1), stunKick: make(chan struct{}, 1),
		peers: map[string]*p2pPeer{}, attempts: map[string]*p2pAttempt{},
		stunWait: map[[12]byte]chan netip.AddrPort{},
		beaconT:  map[string]int64{}, checkAt: map[string]time.Time{},
		replyAt: map[string]time.Time{}, dialAt: map[string]time.Time{},
	}
	p.tr = &quic.Transport{Conn: pc}
	p.ln, err = p.tr.Listen(p.serverTLS(), quicConfig)
	if err != nil {
		cancel()
		pc.Close()
		return nil, err
	}
	p.wg.Add(4)
	go p.readLoop()
	go p.acceptLoop()
	go p.stunLoop()
	go p.healthLoop()
	p.startBeacons()
	return p, nil
}

// close гасит всё и ждёт свои циклы: после Run горутин не остаётся.
func (p *p2p) close() {
	p.cancel()
	p.mu.Lock()
	for _, peer := range p.peers {
		if peer.conn != nil {
			peer.conn.CloseWithError(0, "")
		}
	}
	for _, a := range p.attempts {
		a.finish()
	}
	p.mu.Unlock()
	p.ln.Close()
	p.tr.Close()
	p.pc.Close()
	if p.bln != nil {
		p.bln.Close()
	}
	p.wg.Wait()
}

// ---------- ключи ----------
// Ключ и сертификат — store.go (loadIdentity, p2pCert).

func keyFP(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:])
}

func certFP(raw [][]byte) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("нет сертификата")
	}
	cert, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return "", err
	}
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", errors.New("не тот ключ")
	}
	return keyFP(pub), nil
}

// peerFP — отпечаток ключа собеседника в установленном соединении.
func peerFP(conn quic.Connection) (string, error) {
	certs := conn.ConnectionState().TLS.PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("нет сертификата")
	}
	pub, ok := certs[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", errors.New("не тот ключ")
	}
	return keyFP(pub), nil
}

func (p *p2p) serverTLS() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{p.cert},
		NextProtos:   []string{p2pALPN},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
		// Кто пришёл, проверяем в acceptLoop: отпечаток должен совпасть с
		// тем, что устройство прислало в предложении через meshd.
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			fp, err := certFP(raw)
			if err != nil {
				return err
			}
			// Без сервера (маяки) звонят устройства из последнего списка
			// meshd с запомненным отпечатком.
			if !p.expecting(fp) && p.c.cache.byFP(fp) == "" {
				return errors.New("неизвестное устройство")
			}
			return nil
		},
	}
}

func (p *p2p) clientTLS(expect string) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{p.cert},
		NextProtos:         []string{p2pALPN},
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // цепочки нет — проверяем отпечаток ниже
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			fp, err := certFP(raw)
			if err != nil {
				return err
			}
			if fp != expect {
				return errors.New("ключ собеседника не совпал с присланным через сервер")
			}
			return nil
		},
	}
}

func (p *p2p) expecting(fp string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.attempts {
		if !a.initiator && a.fp == fp {
			return true
		}
	}
	return false
}

// ---------- UDP: STUN и пробивание ----------

func (p *p2p) readLoop() {
	defer p.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, from, err := p.tr.ReadNonQUICPacket(p.ctx, buf)
		if err != nil {
			return
		}
		b := buf[:n]
		switch {
		case n >= 20 && binary.BigEndian.Uint16(b[0:2]) == stunBindResp:
			var id [12]byte
			copy(id[:], b[8:20])
			p.mu.Lock()
			ch := p.stunWait[id]
			p.mu.Unlock()
			if ch != nil {
				if ap, ok := stunParse(b, id); ok {
					select {
					case ch <- ap:
					default:
					}
				}
			}
		case n >= 21 && [4]byte(b[0:4]) == punchMagic:
			var id [16]byte
			copy(id[:], b[5:21])
			p.punchPacket(b[4], id, from)
		case isBeacon(b):
			p.onBeacon(b, from)
		}
	}
}

func (p *p2p) punchPacket(typ byte, id [16]byte, from net.Addr) {
	p.mu.Lock()
	a := p.attempts[string(id[:])]
	p.mu.Unlock()
	if a == nil {
		return // чужое или устаревшее — не отвечаем, чтобы не быть отражателем
	}
	if typ == punchPing {
		p.tr.WriteTo(punchPacket(punchPong, id), from)
	}
	select {
	case a.path <- from:
	default:
	}
}

func punchPacket(typ byte, id [16]byte) []byte {
	b := make([]byte, 21)
	copy(b, punchMagic[:])
	b[4] = typ
	copy(b[5:], id[:])
	return b
}

// stunQuery узнаёт внешний адрес этого сокета.
func (p *p2p) stunQuery() (netip.AddrPort, error) {
	var id [12]byte
	rand.Read(id[:])
	ch := make(chan netip.AddrPort, 1)
	p.mu.Lock()
	p.stunWait[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.stunWait, id)
		p.mu.Unlock()
	}()
	req := stunRequest(id, false)
	stun := p.stunAddr()
	if stun == nil {
		return netip.AddrPort{}, errNoServer
	}
	for try := 0; try < 3; try++ {
		if _, err := p.tr.WriteTo(req, stun); err != nil {
			return netip.AddrPort{}, err
		}
		select {
		case ap := <-ch:
			return ap, nil
		case <-time.After(800 * time.Millisecond):
		case <-p.ctx.Done():
			return netip.AddrPort{}, p.ctx.Err()
		}
	}
	return netip.AddrPort{}, errors.New("сервер не ответил по UDP")
}

// stunLoop держит «дырку» в NAT, пока сервер на связи. Без сервера STUN
// спрашивать не у кого — ждёт следующей сессии (stunKick).
func (p *p2p) stunLoop() {
	defer p.wg.Done()
	first := true
	for {
		var wait <-chan time.Time
		if p.stunAddr() == nil {
			first = true
		} else {
			if ap, err := p.stunQuery(); err == nil {
				p.mu.Lock()
				p.public = ap
				p.mu.Unlock()
				if first {
					// Внешний адрес известен — можно сразу искать прямые пути
					// ко всем устройствам в сети, не дожидаясь, пока к ним
					// пойдут.
					first = false
					go p.autoConnect()
				}
			}
			wait = time.After(20 * time.Second)
		}
		select {
		case <-p.ctx.Done():
			return
		case <-p.stunKick:
		case <-wait:
		}
	}
}

// candidates — адреса, по которым к этому устройству можно попробовать
// достучаться: внешний (через NAT) и локальные (если собеседник в той же сети).
func (p *p2p) candidates() []string {
	var out []string
	p.mu.Lock()
	if p.public.IsValid() {
		out = append(out, p.public.String())
	}
	p.mu.Unlock()
	la, ok := p.pc.LocalAddr().(*net.UDPAddr)
	if !ok {
		return out
	}
	seen := map[netip.Addr]bool{}
	add := func(ip netip.Addr) {
		ip = ip.Unmap()
		if !ip.Is4() || !ip.IsPrivate() || Range.Contains(ip) || seen[ip] || len(out) >= 8 {
			return
		}
		seen[ip] = true
		out = append(out, netip.AddrPortFrom(ip, uint16(la.Port)).String())
	}
	// Сначала адрес, через который устройство ходит в интернет: система
	// выбирает его для «соединения» UDP-сокета с сервером (пакеты при этом не
	// уходят). Это работает и на Android 11+, где список сетевых интерфейсов
	// приложению не отдают.
	if ip, ok := p.routeIP(); ok {
		add(ip)
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				add(ip)
			}
		}
	}
	return out
}

// routeIP — локальный адрес этого устройства в сторону сервера.
func (p *p2p) routeIP() (netip.Addr, bool) {
	dial := p.c.cfg.ProbeDial
	if dial == nil {
		dial = net.Dial
	}
	stun := p.stunAddr()
	if stun == nil {
		return netip.Addr{}, false
	}
	conn, err := dial("udp4", stun.String())
	if err != nil {
		return netip.Addr{}, false
	}
	defer conn.Close()
	ua, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	ip, ok := netip.AddrFromSlice(ua.IP)
	return ip.Unmap(), ok
}

func parseCands(list []string) []*net.UDPAddr {
	var out []*net.UDPAddr
	for _, s := range list {
		if ap, err := netip.ParseAddrPort(s); err == nil && ap.Addr().Is4() {
			out = append(out, net.UDPAddrFromAddrPort(ap))
		}
		if len(out) >= 16 {
			break
		}
	}
	return out
}

// ---------- сигнализация через meshd ----------

// autoConnect пробует прямой путь ко всем устройствам сети, которые сейчас
// на связи: так прямое соединение готово ещё до того, как к устройству
// пойдут, и видно в панели. Неудачи — всё реже (см. retryAfter).
func (p *p2p) autoConnect() {
	st := p.c.Status()
	for _, peer := range st.Peers {
		if peer.Self || !peer.Online || serverNodes.Contains(peer.IP) {
			continue
		}
		p.connect(peer.IP.String(), true)
	}
}

// connect начинает попытку прямого пути к устройству, если её не было
// недавно. Не ждёт: соединение, ради которого вызвали, уже идёт через сервер.
// auto — попытка сама по себе (а не ради соединения человека): после
// неудач реже.
func (p *p2p) connect(peerIP string, auto bool) {
	p.mu.Lock()
	if p.stun == nil {
		// Сервера нет (или он старый, без прямых): сводить некому — без
		// него пути ищут маяки.
		p.mu.Unlock()
		return
	}
	peer := p.peers[peerIP]
	if peer == nil {
		peer = &p2pPeer{}
		p.peers[peerIP] = peer
	}
	wait := p2pRetry
	if auto {
		wait = peer.retryAfter()
	}
	busy := peer.conn != nil || (!peer.lastTry.IsZero() && time.Since(peer.lastTry) < wait)
	for _, a := range p.attempts {
		if a.peerIP == peerIP {
			busy = true
		}
	}
	if busy {
		p.mu.Unlock()
		return
	}
	peer.lastTry = time.Now()
	a := p.newAttemptLocked(peerIP, true)
	p.mu.Unlock()

	if err := p.send(wire{Op: "p2p", To: peerIP, Call: hex.EncodeToString(a.id[:]), Cands: p.candidates(), FP: p.fp}); err != nil {
		// Сервер пропал между делом — это не неудача пробивания.
		p.mu.Lock()
		peer.lastTry = time.Time{}
		p.mu.Unlock()
		p.dropAttempt(a)
		return
	}
	go func() {
		select {
		case <-a.answered:
			p.punch(a)
		case <-time.After(6 * time.Second):
			// Не ответило: старая версия программы или прямые выключены.
			p.failed(peerIP)
			p.dropAttempt(a)
		case <-p.ctx.Done():
		}
	}()
}

// failed отмечает неудачную попытку; возвращает, какая она по счёту подряд.
func (p *p2p) failed(peerIP string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	peer := p.peers[peerIP]
	if peer == nil || peer.conn != nil {
		return 0
	}
	peer.fails++
	return peer.fails
}

func (p *p2p) newAttemptLocked(peerIP string, initiator bool) *p2pAttempt {
	a := &p2pAttempt{peerIP: peerIP, initiator: initiator,
		answered: make(chan struct{}), path: make(chan net.Addr, 4), done: make(chan struct{})}
	rand.Read(a.id[:])
	p.attempts[string(a.id[:])] = a
	return a
}

func (p *p2p) dropAttempt(a *p2pAttempt) {
	a.finish()
	p.mu.Lock()
	delete(p.attempts, string(a.id[:]))
	p.mu.Unlock()
}

// signal — предложение или ответ от другого устройства (op p2p).
func (p *p2p) signal(m wire) {
	raw, err := hex.DecodeString(m.Call)
	if err != nil || len(raw) != 16 || m.From == "" {
		return
	}
	var id [16]byte
	copy(id[:], raw)
	// Свой адрес — до p.mu: Status сама заглядывает в p2p (порядок замков
	// всегда c.mu → p.mu).
	self := p.c.Status().Self.IP.String()

	p.mu.Lock()
	if a := p.attempts[string(id[:])]; a != nil && a.initiator {
		// Ответ на наше предложение.
		p.mu.Unlock()
		if !m.OK || m.FP == "" {
			p.dropAttempt(a)
			return
		}
		a.once.Do(func() {
			p.mu.Lock()
			a.fp, a.cands = m.FP, parseCands(m.Cands)
			p.mu.Unlock()
			close(a.answered)
		})
		return
	}
	// Встречное предложение, пока мы сами пробуем к нему же: побеждает
	// устройство с меньшим адресом, иначе каждый ждал бы своего.
	for _, a := range p.attempts {
		if a.peerIP == m.From && a.initiator {
			if self < m.From {
				p.mu.Unlock()
				p.send(wire{Op: "p2p", To: m.From, Call: m.Call, OK: false})
				return
			}
		}
	}
	if peer := p.peers[m.From]; peer != nil && peer.conn != nil {
		// Прямой путь уже есть, а собеседник его потерял — старый закроем,
		// строим заново.
		peer.conn.CloseWithError(0, "")
		peer.conn = nil
	}
	a := &p2pAttempt{id: id, peerIP: m.From, fp: m.FP, cands: parseCands(m.Cands),
		answered: make(chan struct{}), path: make(chan net.Addr, 4), done: make(chan struct{})}
	p.attempts[string(id[:])] = a
	p.mu.Unlock()

	if err := p.send(wire{Op: "p2p", To: m.From, Call: m.Call, Cands: p.candidates(), FP: p.fp, OK: true}); err != nil {
		p.dropAttempt(a)
		return
	}
	go p.punch(a)
}

// punch — пробивание: шлём пакеты на все адреса собеседника, пока путь не
// найдётся. Инициатор по найденному пути открывает QUIC, второй ждёт его.
func (p *p2p) punch(a *p2pAttempt) {
	defer p.dropAttempt(a)
	ping := punchPacket(punchPing, a.id)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(punchFor)
	p.mu.Lock()
	cands := append([]*net.UDPAddr(nil), a.cands...)
	p.mu.Unlock()
	for _, c := range cands {
		p.tr.WriteTo(ping, c)
	}
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-a.done:
			return
		case <-deadline:
			if p.failed(a.peerIP) == 1 {
				p.c.log("info", "Сеть устройств: прямой путь к "+a.peerIP+" не нашёлся — соединения идут через сервер")
			}
			return
		case <-tick.C:
			for _, c := range cands {
				p.tr.WriteTo(ping, c)
			}
		case path := <-a.path:
			if !a.initiator {
				continue // ждём QUIC от инициатора (acceptLoop), пинги продолжаются
			}
			ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
			conn, err := p.tr.Dial(ctx, path, p.clientTLS(a.fp), quicConfig)
			cancel()
			if err != nil {
				continue // попробуем по следующему подтверждённому пути
			}
			p.adopt(a.peerIP, conn)
			return
		}
	}
}

func (p *p2p) acceptLoop() {
	defer p.wg.Done()
	for {
		conn, err := p.ln.Accept(p.ctx)
		if err != nil {
			return
		}
		fp, err := peerFP(conn)
		if err != nil {
			conn.CloseWithError(1, "")
			continue
		}
		p.mu.Lock()
		var match *p2pAttempt
		for _, a := range p.attempts {
			if !a.initiator && a.fp == fp {
				match = a
			}
		}
		p.mu.Unlock()
		if match == nil {
			// Без предложения через meshd — звонок по маяку: только от
			// устройства из последнего списка с тем же отпечатком.
			if ip := p.c.cache.byFP(fp); ip != "" {
				p.adopt(ip, conn)
				continue
			}
			conn.CloseWithError(1, "")
			continue
		}
		p.adopt(match.peerIP, conn)
		match.finish()
	}
}

// adopt — прямое соединение установлено: с этого момента новые соединения к
// этому устройству идут по нему.
func (p *p2p) adopt(peerIP string, conn quic.Connection) {
	p.mu.Lock()
	peer := p.peers[peerIP]
	if peer == nil {
		peer = &p2pPeer{}
		p.peers[peerIP] = peer
	}
	if peer.conn != nil && peer.conn != conn {
		peer.conn.CloseWithError(0, "")
	}
	peer.conn, peer.fails = conn, 0
	p.mu.Unlock()
	p.c.log("info", fmt.Sprintf("Сеть устройств: прямое соединение с %s (%s)", peerIP, conn.RemoteAddr()))
	// Запомнить отпечаток (рукопожатие его уже проверило) и рабочий адрес:
	// по ним устройство найдётся без сервера.
	fp, _ := peerFP(conn)
	addr := ""
	if ua, ok := conn.RemoteAddr().(*net.UDPAddr); ok {
		if ap := ua.AddrPort(); ap.Addr().Unmap().Is4() {
			addr = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()).String()
		}
	}
	p.c.cache.worked(peerIP, fp, addr)
	p.c.saveCache()
	go p.serve(peerIP, conn)
	p.measure(peerIP)
	p.report()
}

// forget закрывает прямые пути к устройствам, которых больше нет в списке
// meshd (убраны в панели).
func (p *p2p) forget(ips []string) {
	for _, ip := range ips {
		p.mu.Lock()
		peer := p.peers[ip]
		delete(p.peers, ip)
		p.mu.Unlock()
		if peer != nil && peer.conn != nil {
			peer.conn.CloseWithError(0, "")
			p.c.log("info", "Сеть устройств: "+ip+" убран из сети — прямое соединение закрыто")
		}
	}
}

func (p *p2p) drop(peerIP string, conn quic.Connection, why error) {
	p.mu.Lock()
	peer := p.peers[peerIP]
	gone := peer != nil && peer.conn == conn
	if gone {
		peer.conn = nil
		peer.rtt = 0
	}
	p.mu.Unlock()
	conn.CloseWithError(0, "")
	if gone {
		p.c.log("info", fmt.Sprintf("Сеть устройств: прямое соединение с %s пропало (%v) — дальше через сервер", peerIP, why))
		p.report()
		p.kick() // без сервера — искать путь заново маяками
	}
}

// serve принимает потоки от собеседника: каждый — соединение к службе этого
// устройства (или замер задержки, порт 0).
func (p *p2p) serve(peerIP string, conn quic.Connection) {
	for {
		str, err := conn.AcceptStream(p.ctx)
		if err != nil {
			p.drop(peerIP, conn, err)
			return
		}
		go p.handleStream(peerIP, conn, str)
	}
}

// Заголовок потока: версия (1), порт (2 байта). Ответ — 1 байт.
const (
	streamOK       = 0
	streamRefused  = 1
	streamNoListen = 2
)

func (p *p2p) handleStream(peerIP string, conn quic.Connection, str quic.Stream) {
	var hdr [3]byte
	str.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(str, hdr[:]); err != nil || hdr[0] != 1 {
		str.CancelRead(0)
		str.Close()
		return
	}
	str.SetReadDeadline(time.Time{})
	port := int(binary.BigEndian.Uint16(hdr[1:3]))
	reply := func(code byte) {
		str.Write([]byte{code})
		finishStream(str)
	}
	if port == 0 { // замер задержки
		reply(streamOK)
		return
	}
	h := service(port)
	if h == nil && !p.c.cfg.AllowIncoming {
		reply(streamRefused)
		return
	}
	var local net.Conn
	var err error
	if h != nil {
		local = localService(h, Caller{IP: peerIP, Host: p.c.hostOf(peerIP), ID: p.c.idOf(peerIP)})
	} else {
		ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
		local, err = p.c.LocalDial(ctx, port)
		cancel()
	}
	if err != nil {
		reply(streamNoListen)
		return
	}
	str.Write([]byte{streamOK})
	p.c.log("info", fmt.Sprintf("Сеть устройств: %s напрямую → порт %d этого устройства", peerIP, port))
	pump(streamConn{str, conn}, local)
}

// dialDirect — соединение с портом устройства по прямому пути. nil, nil —
// прямого пути нет (идти через сервер).
func (p *p2p) dialDirect(peerIP string, port int) (net.Conn, error) {
	p.mu.Lock()
	peer := p.peers[peerIP]
	var conn quic.Connection
	if peer != nil {
		conn = peer.conn
	}
	p.mu.Unlock()
	if conn == nil {
		return nil, nil
	}
	str, err := p.openStream(conn, port)
	if err != nil {
		p.drop(peerIP, conn, fmt.Errorf("новый поток: %w", err))
		return nil, nil
	}
	var status [1]byte
	str.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(str, status[:]); err != nil {
		str.CancelRead(0)
		str.Close()
		p.drop(peerIP, conn, fmt.Errorf("устройство не ответило: %w", err))
		return nil, nil
	}
	str.SetReadDeadline(time.Time{})
	switch status[0] {
	case streamRefused:
		finishStream(str)
		return nil, errors.New("устройство не принимает входящие соединения")
	case streamNoListen:
		finishStream(str)
		return nil, fmt.Errorf("на устройстве ничего не слушает порт %d", port)
	}
	return streamConn{str, conn}, nil
}

func (p *p2p) openStream(conn quic.Connection, port int) (quic.Stream, error) {
	ctx, cancel := context.WithTimeout(p.ctx, 3*time.Second)
	defer cancel()
	str, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	hdr := []byte{1, byte(port >> 8), byte(port)}
	if _, err := str.Write(hdr); err != nil {
		str.CancelRead(0)
		str.Close()
		return nil, err
	}
	return str, nil
}

// measure — задержка прямого пути (пустой поток туда и обратно).
func (p *p2p) measure(peerIP string) bool {
	_, ok := p.ping(peerIP)
	return ok
}

// ping — задержка прямого пути сейчас; false — прямого пути нет (или он
// только что пропал).
func (p *p2p) ping(peerIP string) (time.Duration, bool) {
	p.mu.Lock()
	peer := p.peers[peerIP]
	var conn quic.Connection
	if peer != nil {
		conn = peer.conn
	}
	p.mu.Unlock()
	if conn == nil {
		return 0, false
	}
	start := time.Now()
	str, err := p.openStream(conn, 0)
	if err == nil {
		var b [1]byte
		str.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = io.ReadFull(str, b[:])
		finishStream(str)
	}
	if err != nil {
		p.drop(peerIP, conn, fmt.Errorf("замер задержки: %w", err))
		return 0, false
	}
	rtt := time.Since(start)
	p.mu.Lock()
	if peer.conn == conn {
		peer.rtt = rtt
	}
	p.mu.Unlock()
	return rtt, true
}

func (p *p2p) healthLoop() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
		p.mu.Lock()
		var ips []string
		for ip, peer := range p.peers {
			if peer.conn != nil {
				ips = append(ips, ip)
			}
		}
		p.mu.Unlock()
		for _, ip := range ips {
			p.measure(ip)
		}
		p.report()
		p.autoConnect()
		// Не записалось с первого раза — запишется сейчас (без изменений
		// файл не трогается).
		p.c.saveCache()
	}
}

// report — сервер узнаёт, с кем у устройства прямые соединения (для панели).
func (p *p2p) report() {
	p.mu.Lock()
	list := []directWire{}
	for ip, peer := range p.peers {
		if peer.conn != nil {
			list = append(list, directWire{IP: ip, RTTMs: float64(peer.rtt.Microseconds()) / 1000})
		}
	}
	p.mu.Unlock()
	p.send(wire{Op: "p2pstat", Direct: list})
}

// direct — прямой ли путь к устройству и его задержка.
func (p *p2p) direct(peerIP string) (bool, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if peer := p.peers[peerIP]; peer != nil && peer.conn != nil {
		return true, peer.rtt
	}
	return false, 0
}

// finishStream закрывает поток до конца: своё направление — закрытием,
// встречное — дочитав до конца (FIN) от собеседника. Недочитанный поток QUIC
// считает живым, а живых потоков у соединения не больше 100: раньше замеры
// задержки раз в 15 секунд за ~25 минут выбирали лимит, и прямое соединение
// обрывалось.
func finishStream(str quic.Stream) {
	str.Close()
	str.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, io.LimitReader(str, 1<<16)); err != nil {
		str.CancelRead(0)
	}
}

// streamConn — поток QUIC как net.Conn.
type streamConn struct {
	quic.Stream
	conn quic.Connection
}

func (s streamConn) LocalAddr() net.Addr  { return s.conn.LocalAddr() }
func (s streamConn) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }
func (s streamConn) CloseWrite() error    { return s.Stream.Close() }
func (s streamConn) Close() error {
	s.Stream.CancelRead(0)
	return s.Stream.Close()
}
