// Пакет mesh — клиент «сети устройств»: твои устройства (ноутбук, телефон,
// домашний сервер) видят друг друга по постоянным адресам 198.19.x.y и
// именам «имя.mesh» — как в NetBird или Tailscale, только через собственный
// сервер. Серверная сторона — sshtunnel/cmd/meshd, протокол описан там.
//
// Клиент ходит к meshd через уже поднятый SSH-туннель (Dial ядра до
// 127.0.0.1:47831 на сервере), поэтому ему не нужны ни белый адрес, ни проброс
// портов на роутере: достаточно того, что устройство видит сервер.
package mesh

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAddr — где на сервере слушает meshd (его собственный localhost).
const DefaultAddr = "127.0.0.1:47831"

// Suffix — окончание имён устройств: «ноутбук.mesh».
const Suffix = ".mesh"

// Range — адреса устройств. Из диапазона тестов сетевого оборудования
// (198.18.0.0/15): с настоящими сетями он не пересекается, а его первую
// половину уже занимают подставные адреса DNS.
var Range = netip.MustParsePrefix("198.19.0.0/16")

// Config — настройки сети устройств для одного сервера.
type Config struct {
	// Key — ключ сети: устройства с одинаковым ключом видят друг друга.
	Key string
	// DeviceID — постоянный id этого устройства (см. NewDeviceID): по нему
	// сервер узнаёт устройство и выдаёт ему тот же адрес.
	DeviceID string
	// Name — имя устройства, из него получается имя «.mesh».
	Name string
	// AllowIncoming — пускать ли соединения от других устройств к своим
	// службам (на 127.0.0.1 этого устройства).
	AllowIncoming bool
	// Addr — адрес meshd на сервере. Пусто — DefaultAddr.
	Addr string

	// То, что устройство рассказывает о себе: видно в панели на сервере.
	// Всё необязательное.
	Platform   string // windows, linux, android
	OS         string // версия системы, если известна
	AppVersion string // версия ssh_tunnel
	Mode       string // proxy или vpn
	Via        string // через какой сервер подключено устройство
	Hostname   string // имя компьютера

	// Проверка NAT (см. natprobe.go) идёт на сервер Via мимо туннеля.
	// ProbeListen открывает такой UDP-сокет (на Android и в режиме VPN — с
	// пометкой «мимо VPN»); nil — обычный. ProbeResolver ищет адрес сервера
	// мимо туннеля; nil — системный. NoNATProbe — не проверять (тесты).
	ProbeListen   func(network string) (net.PacketConn, error)
	ProbeResolver *net.Resolver
	NoNATProbe    bool
	// ProbeDial — такой же сокет мимо туннеля, но «соединённый»: по нему
	// узнаётся свой локальный адрес (см. p2p.routeIP); nil — обычный.
	ProbeDial func(network, addr string) (net.Conn, error)
	// NoDirect — не пробовать прямые соединения между устройствами: всё
	// только через сервер.
	NoDirect bool
	// StateDir — папка, где сеть устройств хранит ключ прямых соединений и
	// последний список устройств (store.go), чтобы работать без сервера.
	// Права — как у остальных настроек программы (0600/0700). Пусто — всё
	// только в памяти: без сервера живут лишь уже открытые прямые соединения.
	StateDir string
}

// NewKey придумывает ключ новой сети.
func NewKey() string { return randomString(24) }

// NewDeviceID придумывает id устройства.
func NewDeviceID() string { return randomString(16) }

func randomString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Peer — устройство сети.
type Peer struct {
	// ID — постоянный id устройства (ТЗ 18, PeerID): адрес и имя меняются,
	// он — нет. Пусто — старый meshd.
	ID       string     `json:"id,omitempty"`
	Name     string     `json:"name"`
	Host     string     `json:"host"`
	IP       netip.Addr `json:"ip"`
	Platform string     `json:"platform,omitempty"`
	OS       string     `json:"os,omitempty"` // версия системы (новый meshd)
	Via      string     `json:"via,omitempty"`
	Online   bool       `json:"online"`
	LastSeen int64      `json:"lastSeen,omitempty"`
	Self     bool       `json:"self,omitempty"`
	// Direct — к устройству есть прямое соединение (минуя сервер),
	// DirectRTTMs — его задержка.
	Direct      bool    `json:"direct,omitempty"`
	DirectRTTMs float64 `json:"directRttMs,omitempty"`
	// Servers — через какие из серверов этого устройства оно сейчас на
	// связи (имена). Elsewhere — серверы, с которыми оно на связи по своему
	// последнему отчёту, а это устройство к ним не подключено; NoCommon —
	// общего живого сервера нет, а прямого пути тоже: соединиться нельзя.
	Servers   []string `json:"servers,omitempty"`
	Elsewhere []string `json:"elsewhere,omitempty"`
	NoCommon  bool     `json:"noCommon,omitempty"`
}

// ServerStatus — одно управляющее соединение с сервером сети.
type ServerStatus struct {
	Key string `json:"key"` // как его назвал хозяин (адрес из профиля)
	// ID и Name — что сказал о себе meshd (у старого meshd id нет).
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	IP    string `json:"ip,omitempty"`
	State string `json:"state"` // connecting, online, error, duplicate
	Error string `json:"error,omitempty"`
	// Primary — сервер туннеля; остальные — лёгкие соединения только для
	// сети устройств.
	Primary bool `json:"primary,omitempty"`
}

// Status — то, что показывается человеку.
type Status struct {
	// State: "connecting", "online", "error" или "local" — сервера сети нет
	// (Error — почему), но с частью устройств есть прямые соединения: они
	// Online и Direct, работают без сервера. "online" — на связи хоть с
	// одним сервером.
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	Self  Peer   `json:"self"`
	Peers []Peer `json:"peers"`
	// NAT — результат проверки NAT этого устройства, если она прошла.
	NAT *NATInfo `json:"nat,omitempty"`
	// Servers — все серверы сети, с которыми устройство держит связь.
	Servers []ServerStatus `json:"servers,omitempty"`
	// Advertised — серверы, которые советуют панели серверов (может, к ним
	// стоит добавить вход), без тех, что уже есть.
	Advertised []AdvertisedServer `json:"advertised,omitempty"`
}

// AdvertisedServer — сервер сети, который советует панель другого сервера.
type AdvertisedServer struct {
	Name string `json:"name,omitempty"`
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
	From string `json:"from,omitempty"` // какой сервер посоветовал
}

// Dialer открывает соединение через туннель (tunnel.Tunnel.Dial).
type Dialer func(network, addr string) (net.Conn, error)

// Client держит управляющие соединения с meshd на серверах сети и
// обслуживает вызовы в обе стороны.
//
// Серверов может быть несколько (ТЗ 12): с каждым — своё управляющее
// соединение (srvLink), главного нет. Устройство одно и то же на всех:
// свой адрес и имя оно хранит само и называет каждому серверу (см.
// «адрес и имя устройства» в cmd/meshd). Список устройств — объединение
// списков всех серверов по адресу: устройство, которое на связи с двумя
// серверами, в нём одно.
type Client struct {
	cfg Config
	log func(level, text string)

	// LocalDial соединяется со службой на этом устройстве для входящего
	// вызова. Можно подменить в тестах.
	LocalDial func(ctx context.Context, port int) (net.Conn, error)

	// cache — последний список устройств (store.go); свой замок.
	cache *peerCache

	// Для тестов: приёмник широковещательных маяков и куда их слать (nil —
	// порт defaultBeaconPort и broadcast локальных сетей).
	beaconListen  func() (net.PacketConn, error)
	beaconTargets func() []*net.UDPAddr

	mu     sync.Mutex
	status Status
	p2p    *p2p       // прямые соединения, пока идёт Run; nil — нет
	links  []*srvLink // первый — сервер туннеля
	runCtx context.Context
	wg     sync.WaitGroup
	echo   map[string]chan struct{}
	// Что устройство держит о себе (кроме адреса и имени — они в
	// status.Self): с какого времени адрес, имя из панели и когда его дали.
	since    int64
	alias    string
	aliasT   int64
	stunLink *srvLink // чей STUN сейчас у прямых соединений
	// helloMu — по одному входу за раз: «кто ты» и hello двух соединений
	// не должны перемешаться, иначе два пути к одному meshd выбьют друг
	// друга раньше, чем станет понятно, что это один сервер.
	helloMu sync.Mutex
}

// srvLink — управляющее соединение с одним сервером сети.
type srvLink struct {
	key     string
	via     string // адрес сервера снаружи: проверка NAT и STUN
	dial    Dialer
	primary bool
	cancel  context.CancelFunc

	// Дальше — под c.mu.
	ctrl  func(wire) error
	info  wireServer
	state string
	err   string
	// peers — последний список этого сервера; живёт и после обрыва, чтобы
	// устройства, которых знает только он, не забылись при его падении.
	peers    []wirePeer
	peersSet bool
	ads      []wireAd
	since    time.Time // когда соединение поднялось
	sentOn   string    // последний отчёт op on этому серверу
}

// New готовит клиента. dial — путь к серверу туннеля (основной); другие
// серверы сети добавляет AddServer. log получает строки для журнала
// ("info", "warn").
func New(cfg Config, dial Dialer, log func(level, text string)) *Client {
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if log == nil {
		log = func(string, string) {}
	}
	c := &Client{cfg: cfg, log: log, status: Status{State: "connecting"}, cache: loadCache("")}
	c.links = []*srvLink{{key: cfg.Via, via: cfg.Via, dial: dial, primary: true, state: "connecting"}}
	c.LocalDial = func(ctx context.Context, port int) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}
	return c
}

// AddServer добавляет ещё один сервер сети (или меняет путь к уже
// добавленному). key — как его называет хозяин (адрес из профиля), via —
// адрес сервера снаружи. Соединение с ним лёгкое: реже пинги, без проверки
// NAT. Можно звать и до Run, и во время.
func (c *Client) AddServer(key, via string, dial Dialer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.links {
		if l.key == key {
			l.dial = dial
			return
		}
	}
	l := &srvLink{key: key, via: via, dial: dial, state: "connecting"}
	c.links = append(c.links, l)
	if c.runCtx != nil && c.runCtx.Err() == nil {
		c.startLinkLocked(l)
	}
}

// RemoveServer убирает сервер, добавленный AddServer (основной не убирается).
func (c *Client) RemoveServer(key string) {
	c.mu.Lock()
	var gone *srvLink
	for i, l := range c.links {
		if l.key == key && !l.primary {
			gone = l
			c.links = append(c.links[:i:i], c.links[i+1:]...)
			break
		}
	}
	if gone != nil && gone.cancel != nil {
		gone.cancel()
	}
	c.rebuildLocked()
	c.mu.Unlock()
	if gone != nil {
		c.reportOn()
	}
}

// wire — строка протокола; зеркало msg из cmd/meshd.
type wire struct {
	Op       string     `json:"op"`
	V        int        `json:"v,omitempty"`
	Net      string     `json:"net,omitempty"`
	Device   string     `json:"device,omitempty"`
	Name     string     `json:"name,omitempty"`
	To       string     `json:"to,omitempty"`
	Port     int        `json:"port,omitempty"`
	Call     string     `json:"call,omitempty"`
	OK       bool       `json:"ok,omitempty"`
	Error    string     `json:"error,omitempty"`
	IP       string     `json:"ip,omitempty"`
	Host     string     `json:"host,omitempty"`
	From     string     `json:"from,omitempty"`
	FromHost string     `json:"fromHost,omitempty"`
	FromID   string     `json:"fromId,omitempty"` // ТЗ 18, новый meshd
	Peers    []wirePeer `json:"peers,omitempty"`

	Platform string `json:"platform,omitempty"`
	OS       string `json:"os,omitempty"`
	App      string `json:"app,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Via      string `json:"via,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	T        int64  `json:"t,omitempty"`

	Stun []int    `json:"stun,omitempty"`
	NAT  *NATInfo `json:"nat,omitempty"`

	// Прямые соединения (см. p2p.go).
	Cands  []string     `json:"cands,omitempty"`
	FP     string       `json:"fp,omitempty"`
	Direct []directWire `json:"direct,omitempty"`

	// Несколько серверов (ТЗ 12), см. cmd/meshd.
	Since   int64        `json:"since,omitempty"`
	Alias   string       `json:"alias,omitempty"`
	AliasT  int64        `json:"aliasT,omitempty"`
	Quiet   bool         `json:"quiet,omitempty"`
	Server  *wireServer  `json:"server,omitempty"`
	Servers []wireAd     `json:"servers,omitempty"`
	On      []wireServer `json:"on,omitempty"`
}

type wirePeer struct {
	ID       string       `json:"id,omitempty"`
	Name     string       `json:"name"`
	Host     string       `json:"host"`
	IP       string       `json:"ip"`
	Platform string       `json:"platform,omitempty"`
	OS       string       `json:"os,omitempty"`
	Via      string       `json:"via,omitempty"`
	Online   bool         `json:"online"`
	LastSeen int64        `json:"lastSeen,omitempty"`
	On       []wireServer `json:"on,omitempty"`
}

// wireServer — сервер в welcome и в отчёте op on.
type wireServer struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Host string `json:"host,omitempty"`
	IP   string `json:"ip,omitempty"`
	Pol  string `json:"pol,omitempty"`
}

// wireAd — сервер, который советует панель.
type wireAd struct {
	Name string `json:"name,omitempty"`
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
}

const protoVersion = 2

func send(conn net.Conn, m wire) error {
	m.V = protoVersion
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetWriteDeadline(time.Time{})
	return json.NewEncoder(conn).Encode(m)
}

// errDuplicate — это соединение пришло в тот же meshd, что и другое
// (старый «побочный» сервер — проброс к главному, или один сервер в двух
// профилях): второе соединение одного устройства с одним meshd выбивало бы
// первое.
var errDuplicate = errors.New("тот же сервер сети, что и другой")

// Run держит управляющие соединения со всеми серверами сети, пока жив ctx,
// и переподключается при обрывах с нарастающей паузой.
//
// Прямые соединения живут всё время Run, а не одного подключения: сервер
// пропал — они продолжают работать, сервер вернулся — не пересобираются.
func (c *Client) Run(ctx context.Context) {
	c.loadState()
	if !c.cfg.NoDirect && c.cfg.Via != "" {
		dir := c.stateDir()
		cert, fp, err := loadIdentity(dir)
		if err != nil && dir != "" {
			c.log("warn", "Сеть устройств: ключ прямых соединений не сохранился: "+err.Error())
			cert, fp, err = p2pCert()
		}
		var p *p2p
		if err == nil {
			p, err = startP2P(ctx, c, cert, fp)
		}
		if err != nil {
			c.log("warn", "Сеть устройств: прямые соединения не запустились: "+err.Error())
		} else {
			c.mu.Lock()
			c.p2p = p
			c.mu.Unlock()
			defer func() {
				c.mu.Lock()
				c.p2p = nil
				c.mu.Unlock()
				p.close()
			}()
		}
	}
	c.mu.Lock()
	c.runCtx = ctx
	for _, l := range c.links {
		c.startLinkLocked(l)
	}
	c.mu.Unlock()
	<-ctx.Done()
	c.wg.Wait()
	c.mu.Lock()
	c.runCtx = nil
	c.status = Status{State: "connecting"}
	for _, l := range c.links {
		l.ctrl, l.state, l.err = nil, "connecting", ""
	}
	c.mu.Unlock()
}

// startLinkLocked запускает цикл соединения с сервером l. Под c.mu.
func (c *Client) startLinkLocked(l *srvLink) {
	ctx, cancel := context.WithCancel(c.runCtx)
	l.cancel = cancel
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.linkLoop(ctx, l)
	}()
}

// linkLoop — переподключения к одному серверу.
func (c *Client) linkLoop(ctx context.Context, l *srvLink) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx, l)
		if ctx.Err() != nil {
			return
		}
		c.setError(l, err)
		pause := backoff
		if errors.Is(err, errDuplicate) {
			// Тот же meshd уже на связи по другому пути: пробуем изредка —
			// вдруг тот путь упадёт.
			pause = 30 * time.Second
		} else {
			if time.Since(start) > time.Minute {
				backoff = time.Second
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
}

func (c *Client) setError(l *srvLink, err error) {
	text := "связь с сервером сети потеряна"
	if err != nil {
		text = err.Error()
	}
	c.mu.Lock()
	wasOnline := l.state == "online"
	l.ctrl = nil
	l.state, l.err = "error", text
	if errors.Is(err, errDuplicate) {
		l.state = "duplicate"
	}
	anyOnline := c.onlineLocked()
	c.rebuildLocked()
	p := c.p2p
	stunGone := c.stunLink == l
	if stunGone {
		c.stunLink = nil
	}
	c.mu.Unlock()
	if wasOnline {
		name := l.key
		if l.info.Name != "" {
			name = l.info.Name
		}
		c.log("warn", "Сеть устройств, сервер "+name+": "+text)
	}
	if p != nil && stunGone {
		p.setSTUN(nil)
	}
	if p != nil && !anyOnline {
		p.kick()
	}
	c.reportOn()
}

// loadState — список устройств с диска: имена «.mesh» и адреса известны ещё
// до сервера (и без него).
func (c *Client) loadState() {
	c.cache = loadCache(c.stateDir())
	f := c.cache.snapshot()
	self, _ := netip.ParseAddr(f.Self.IP)
	c.mu.Lock()
	defer c.mu.Unlock()
	if self.IsValid() {
		c.status.Self = Peer{Name: f.Self.Name, Host: f.Self.Host, IP: self, Self: true}
		c.since = f.Self.Since
		if c.since == 0 {
			// Кэш от версии до ТЗ 12: адрес держим с тех пор, как он попал
			// в кэш (точнее не знаем).
			c.since = f.Self.LastSeen
		}
	}
	c.alias, c.aliasT = f.Self.Alias, f.Self.AliasT
	c.status.Peers = nil
	for _, p := range f.Peers {
		ip, err := netip.ParseAddr(p.IP)
		if err != nil {
			continue
		}
		c.status.Peers = append(c.status.Peers, Peer{ID: p.ID, Name: p.Name, Host: p.Host, IP: ip, Platform: p.Platform,
			OS: p.OS, Via: p.Via, LastSeen: p.LastSeen, Self: ip == self})
	}
}

func (c *Client) saveCache() {
	if err := c.cache.save(); err != nil {
		c.log("warn", "Сеть устройств: список устройств не сохранился: "+err.Error())
	}
}

// online — управляющее соединение хоть с одним meshd сейчас есть.
func (c *Client) online() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.onlineLocked()
}

func (c *Client) onlineLocked() bool {
	for _, l := range c.links {
		if l.ctrl != nil {
			return true
		}
	}
	return false
}

func (c *Client) selfIP() netip.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status.Self.IP
}

// peerOnLocked — живые серверы, на которых устройство ip сейчас на связи.
func (c *Client) peerOnLocked(ip string) []*srvLink {
	var out []*srvLink
	for _, l := range c.links {
		if l.ctrl == nil {
			continue
		}
		if l.info.IP == ip && ip != "" {
			out = append(out, l) // сам сервер
			continue
		}
		for _, p := range l.peers {
			if p.IP == ip && p.Online {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

// sendCtrl — строка для meshd; нет ни одного соединения — errNoServer. Не
// звать под p.mu (порядок замков c.mu → p.mu).
//
// Кому: сообщение для другого устройства (To) — серверу, с которым оно на
// связи; отчёт о прямых соединениях — всем; остальное — первому живому.
func (c *Client) sendCtrl(m wire) error {
	c.mu.Lock()
	var targets []func(wire) error
	switch {
	case m.Op == "p2pstat":
		for _, l := range c.links {
			if l.ctrl != nil {
				targets = append(targets, l.ctrl)
			}
		}
	case m.To != "":
		for _, l := range c.peerOnLocked(m.To) {
			targets = append(targets, l.ctrl)
			break
		}
	}
	if len(targets) == 0 {
		for _, l := range c.links {
			if l.ctrl != nil {
				targets = append(targets, l.ctrl)
				break
			}
		}
	}
	c.mu.Unlock()
	if len(targets) == 0 {
		return errNoServer
	}
	var err error
	for _, f := range targets {
		if e := f(m); e != nil {
			err = e
		}
	}
	return err
}

// selfWireLocked — что устройство говорит о себе серверу.
func (c *Client) selfWireLocked(op string) wire {
	m := wire{Op: op, Since: c.since, Alias: c.alias, AliasT: c.aliasT}
	if c.status.Self.IP.IsValid() {
		m.IP, m.Host = c.status.Self.IP.String(), c.status.Self.Host
	}
	if m.Since == 0 {
		// Новый клиент всегда шлёт since: по нему meshd узнаёт, что
		// устройство само держит свой адрес.
		m.Since = time.Now().Unix()
	}
	return m
}

// adoptLocked — сервер сказал устройству его адрес и имя (welcome, self,
// readdress). Изменилось — true: надо сказать остальным серверам.
func (c *Client) adoptLocked(ip, host string, since int64) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	changed := a != c.status.Self.IP || (host != "" && host != c.status.Self.Host)
	if a != c.status.Self.IP && since > 0 {
		c.since = since
	} else if c.since == 0 && since > 0 {
		c.since = since
	}
	c.status.Self.IP, c.status.Self.Self = a, true
	if host != "" {
		c.status.Self.Host = host
	}
	return changed
}

// adoptAliasLocked — имя из панели: позднее побеждает.
func (c *Client) adoptAliasLocked(alias string, t int64) bool {
	if t <= c.aliasT {
		return false
	}
	c.alias, c.aliasT = alias, t
	return true
}

// pushSelf рассказывает остальным серверам (кроме from) новый адрес, имя и
// переименование — op self. Старый meshd её пропускает.
func (c *Client) pushSelf(from *srvLink) {
	c.mu.Lock()
	m := c.selfWireLocked("self")
	var targets []func(wire) error
	for _, l := range c.links {
		if l != from && l.ctrl != nil {
			targets = append(targets, l.ctrl)
		}
	}
	c.mu.Unlock()
	for _, f := range targets {
		f(m)
	}
	c.saveSelf()
}

// reportOn рассказывает каждому серверу, с какими ещё серверами устройство
// на связи и чьи там правила (op on): так его панель видит остальные
// серверы и замечает, что правила доступа на них другие.
//
// Каждому серверу — только остальные серверы и только когда список для
// него изменился: на op on meshd рассылает списки устройств, а лишняя
// рассылка заставляет устройства разом искать друг к другу прямой путь.
func (c *Client) reportOn() {
	type job struct {
		f  func(wire) error
		on []wireServer
	}
	var jobs []job
	c.mu.Lock()
	for _, l := range c.links {
		if l.ctrl == nil {
			continue
		}
		var on []wireServer
		for _, o := range c.links {
			if o != l && o.ctrl != nil && o.info.ID != "" && o.info.ID != l.info.ID {
				on = append(on, wireServer{ID: o.info.ID, Name: o.info.Name, Pol: o.info.Pol})
			}
		}
		key := ""
		if len(on) > 0 {
			b, _ := json.Marshal(on)
			key = string(b)
		}
		if key == l.sentOn {
			continue
		}
		l.sentOn = key
		jobs = append(jobs, job{l.ctrl, on})
	}
	c.mu.Unlock()
	for _, j := range jobs {
		j.f(wire{Op: "on", On: j.on})
	}
}

// saveSelf — адрес, давность и имя из панели в кэш на диске.
func (c *Client) saveSelf() {
	c.mu.Lock()
	self := cachedPeer{IP: c.status.Self.IP.String(), Host: c.status.Self.Host, Name: c.status.Self.Name,
		Since: c.since, Alias: c.alias, AliasT: c.aliasT}
	valid := c.status.Self.IP.IsValid()
	c.mu.Unlock()
	if !valid {
		return
	}
	c.cache.setSelf(self)
	c.saveCache()
}

// session — одно управляющее соединение с сервером l от подключения до обрыва.
func (c *Client) session(ctx context.Context, l *srvLink) error {
	c.mu.Lock()
	dial := l.dial
	hello := c.selfWireLocked("hello")
	c.mu.Unlock()
	if dial == nil {
		return errNoServer
	}
	c.helloMu.Lock()
	entered := false
	defer func() {
		if !entered {
			c.helloMu.Unlock()
		}
	}()
	if err := c.probe(l, dial); err != nil {
		return err
	}
	conn, err := dial("tcp", c.cfg.Addr)
	if err != nil {
		return fmt.Errorf("сервис сети устройств на сервере недоступен (не установлен?): %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	hello.Net, hello.Device, hello.Name = c.cfg.Key, c.cfg.DeviceID, c.cfg.Name
	hello.Platform, hello.OS, hello.App = c.cfg.Platform, c.cfg.OS, c.cfg.AppVersion
	hello.Mode, hello.Via, hello.Hostname = c.cfg.Mode, l.via, c.cfg.Hostname
	hello.Quiet = !l.primary
	if err := send(conn, hello); err != nil {
		return err
	}
	r := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var welcome wire
	if err := readLine(r, &welcome); err != nil {
		return fmt.Errorf("сервис сети устройств не ответил: %w", err)
	}
	if welcome.Op == "error" {
		return errors.New(welcome.Error)
	}
	if welcome.Op != "welcome" {
		return fmt.Errorf("неожиданный ответ сервера: %q", welcome.Op)
	}
	info := wireServer{}
	if welcome.Server != nil {
		info = *welcome.Server
	}

	var wmu sync.Mutex
	ctrl := func(m wire) error {
		wmu.Lock()
		defer wmu.Unlock()
		return send(conn, m)
	}
	c.mu.Lock()
	changed := c.adoptLocked(welcome.IP, welcome.Host, welcome.Since)
	if c.adoptAliasLocked(welcome.Alias, welcome.AliasT) {
		changed = true
	}
	if c.status.Self.Name == "" {
		c.status.Self.Name = c.cfg.Name
		if c.alias != "" {
			c.status.Self.Name = c.alias
		}
	}
	c.status.Self.Online = true
	l.ctrl, l.info, l.state, l.err, l.since = ctrl, info, "online", "", time.Now()
	l.sentOn = "" // meshd забывает прежний отчёт при каждом hello
	l.ads = welcome.Servers
	firstOnline := true
	for _, o := range c.links {
		if o != l && o.ctrl != nil {
			firstOnline = false
		}
	}
	useSTUN := len(welcome.Stun) > 0 && l.via != "" && (l.primary || c.stunLink == nil)
	if useSTUN {
		c.stunLink = l
	}
	c.mu.Unlock()
	entered = true
	c.helloMu.Unlock()
	defer func() {
		c.mu.Lock()
		if l.ctrl != nil {
			l.ctrl = nil
		}
		c.mu.Unlock()
	}()
	c.setPeers(l, welcome.Peers)
	name := info.Name
	if name == "" {
		name = l.key
	}
	if firstOnline || l.primary {
		c.log("info", fmt.Sprintf("Сеть устройств: это устройство — %s%s (%s), сервер %s", welcome.Host, Suffix, welcome.IP, name))
	} else {
		c.log("info", "Сеть устройств: на связи и с сервером "+name)
	}
	if changed {
		c.pushSelf(l)
	} else {
		c.saveSelf()
	}
	c.reportOn()

	// Проверка NAT — один раз на подключение основного сервера: сеть
	// сменилась — подключение пересобирается, и проверка идёт заново.
	if l.primary && len(welcome.Stun) > 0 && l.via != "" && !c.cfg.NoNATProbe {
		go func() {
			info := ProbeNAT(ctx, ProbeEnv{Server: l.via, Ports: welcome.Stun,
				Listen: c.cfg.ProbeListen, Resolver: c.cfg.ProbeResolver})
			if ctx.Err() != nil {
				return
			}
			c.mu.Lock()
			c.status.NAT = &info
			c.mu.Unlock()
			c.log("info", "Сеть устройств, проверка прямых соединений: "+natSummary(info))
			ctrl(wire{Op: "nat", NAT: &info})
		}()
	}

	// Прямые соединения: сокет уже открыт (Run), сервер сообщает свой STUN —
	// по нему узнаём адрес снаружи и сводим устройства через meshd.
	if p := c.direct(); p != nil {
		if useSTUN {
			if ip, err := resolve4(ctx, ProbeEnv{Server: l.via, Resolver: c.cfg.ProbeResolver}); err != nil {
				c.log("warn", "Сеть устройств: адрес сервера для прямых соединений не нашёлся: "+err.Error())
			} else {
				p.setSTUN(&net.UDPAddr{IP: ip, Port: welcome.Stun[0]})
			}
		}
		go p.report() // сервер мог не знать о прямых путях, что жили без него
	}
	// Пинг держит соединение живым через NAT и даёт серверу знать, что мы
	// на месте. Дополнительному серверу хватает его собственных пингов
	// (hello с quiet): так у телефона с двумя серверами пробуждений почти
	// не прибавляется.
	if l.primary {
		go func() {
			t := time.NewTicker(25 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := ctrl(wire{Op: "ping"}); err != nil {
						conn.Close()
						return
					}
				}
			}
		}()
	}
	idle := 90 * time.Second
	if !l.primary {
		idle = 200 * time.Second // сервер пингует раз в 60 с (quiet)
	}
	for {
		conn.SetReadDeadline(time.Now().Add(idle))
		var m wire
		if err := readLine(r, &m); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("связь с сервером сети потеряна")
		}
		switch m.Op {
		case "peers":
			c.setPeers(l, m.Peers)
			if p := c.direct(); p != nil {
				go p.autoConnect()
			}
		case "incoming":
			go c.incoming(l, m)
		case "p2p":
			if p := c.direct(); p != nil {
				go p.signal(m)
			}
		case "echo":
			// Кто-то пингует это устройство (ping 198.19.x.y) — ответить.
			go ctrl(wire{Op: "echoreply", To: m.From, Call: m.Call})
		case "echoreply":
			c.mu.Lock()
			if ch := c.echo[m.Call]; ch != nil {
				close(ch)
				delete(c.echo, m.Call)
			}
			c.mu.Unlock()
		case "readdress", "self":
			// Сервер решил спор за адрес или имя (см. cmd/meshd) — новый
			// адрес у устройства один на все серверы.
			c.mu.Lock()
			changed := c.adoptLocked(m.IP, m.Host, m.Since)
			c.mu.Unlock()
			if changed {
				c.log("info", fmt.Sprintf("Сеть устройств: адрес этого устройства теперь %s (%s%s)", m.IP, m.Host, Suffix))
				c.pushSelf(l)
			}
		case "alias":
			c.mu.Lock()
			changed := c.adoptAliasLocked(m.Alias, m.AliasT)
			if m.Host != "" {
				c.status.Self.Host = m.Host
			}
			c.mu.Unlock()
			if changed {
				c.pushSelf(l)
			}
		case "ping":
			// Сервер меряет задержку: вернуть его метку времени как есть.
			if err := ctrl(wire{Op: "pong", T: m.T}); err != nil {
				return errors.New("связь с сервером сети потеряна")
			}
		}
	}
}

// probe спрашивает сервер «кто ты» (op id) до hello: если это тот же
// meshd, с которым уже есть соединение, — errDuplicate, и hello не идёт
// вовсе (второй hello того же устройства выбил бы первое соединение). Старый
// meshd отвечает ошибкой: у него id нет, а все старые meshd с одним ключом
// сети — это один и тот же главный (побочные — проброс к нему). Под helloMu.
func (c *Client) probe(l *srvLink, dial Dialer) error {
	c.mu.Lock()
	others := 0
	for _, o := range c.links {
		if o != l && o.ctrl != nil {
			others++
		}
	}
	c.mu.Unlock()
	if others == 0 {
		return nil // спорить не с кем — лишний запрос не нужен
	}
	conn, err := dial("tcp", c.cfg.Addr)
	if err != nil {
		return fmt.Errorf("сервис сети устройств на сервере недоступен (не установлен?): %w", err)
	}
	defer conn.Close()
	if err := send(conn, wire{Op: "id"}); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var m wire
	if err := readLine(bufio.NewReader(conn), &m); err != nil {
		return fmt.Errorf("сервис сети устройств не ответил: %w", err)
	}
	id := ""
	if m.Op == "id" && m.Server != nil {
		id = m.Server.ID
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, o := range c.links {
		if o != l && o.ctrl != nil && o.info.ID == id {
			return errDuplicate
		}
	}
	return nil
}

func readLine(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// setPeers — новый список устройств от сервера l.
func (c *Client) setPeers(l *srvLink, list []wirePeer) {
	c.mu.Lock()
	l.peers, l.peersSet = list, true
	c.rebuildLocked()
	st, p := c.status, c.p2p
	c.mu.Unlock()
	// Объединённый список — он же решает, к кому можно без сервера.
	self := cachedPeer{IP: st.Self.IP.String(), Host: st.Self.Host, Name: st.Self.Name}
	c.mu.Lock()
	self.Since, self.Alias, self.AliasT = c.since, c.alias, c.aliasT
	c.mu.Unlock()
	cached := make([]cachedPeer, 0, len(st.Peers))
	now := time.Now().Unix()
	for _, pr := range st.Peers {
		cp := cachedPeer{ID: pr.ID, IP: pr.IP.String(), Host: pr.Host, Name: pr.Name, Platform: pr.Platform,
			OS: pr.OS, Via: pr.Via, LastSeen: pr.LastSeen}
		if pr.Online {
			cp.LastSeen = now
		}
		cached = append(cached, cp)
	}
	removed := c.cache.setList(self, cached)
	c.saveCache()
	if p != nil && len(removed) > 0 {
		p.forget(removed)
	}
}

// rebuildLocked собирает общий список устройств из списков серверов: по
// адресу — устройство с двумя серверами одно. Сведения берутся у сервера,
// где оно на связи. Пока ни один сервер списка не прислал, остаётся список
// с диска. Под c.mu.
func (c *Client) rebuildLocked() {
	type entry struct {
		p      Peer
		live   bool // сведения от живого сервера, где оно на связи
		names  []string
		others map[string]string // серверы из его отчёта: id → имя
	}
	defer c.updateStateLocked()
	got := false
	by := map[string]*entry{}
	var order []string
	mine := map[string]bool{}
	for _, l := range c.links {
		if l.ctrl != nil && l.info.ID != "" {
			mine[l.info.ID] = true
		}
	}
	for _, l := range c.links {
		if !l.peersSet {
			continue
		}
		got = true
		lname := l.info.Name
		if lname == "" {
			lname = l.key
		}
		for _, wp := range l.peers {
			ip, err := netip.ParseAddr(wp.IP)
			if err != nil {
				continue
			}
			live := l.ctrl != nil && wp.Online
			e := by[wp.IP]
			if e == nil {
				e = &entry{others: map[string]string{}}
				by[wp.IP] = e
				order = append(order, wp.IP)
			}
			if e.p.Host == "" || (live && !e.live) {
				last, id := e.p.LastSeen, e.p.ID
				e.p = Peer{ID: wp.ID, Name: wp.Name, Host: wp.Host, IP: ip, LastSeen: wp.LastSeen,
					Platform: wp.Platform, OS: wp.OS, Via: wp.Via}
				if last > e.p.LastSeen {
					e.p.LastSeen = last
				}
				if e.p.ID == "" {
					e.p.ID = id // этот сервер — старый meshd, id знает другой
				}
			} else if wp.LastSeen > e.p.LastSeen {
				e.p.LastSeen = wp.LastSeen
			}
			if e.p.ID == "" {
				e.p.ID = wp.ID
			}
			if live {
				e.live = true
				e.names = append(e.names, lname)
			}
			for _, o := range wp.On {
				e.others[o.ID] = o.Name
			}
			if ip == c.status.Self.IP && l.ctrl != nil && l.info.ID == "" && wp.Host != "" {
				// Старый meshd сам решает имя (и переименование в панели).
				c.status.Self.Host = wp.Host
			}
		}
	}
	if !got {
		return
	}
	peers := make([]Peer, 0, len(by))
	for _, ip := range order {
		e := by[ip]
		p := e.p
		p.Online = e.live
		p.Servers = e.names
		p.Self = p.IP == c.status.Self.IP
		if p.Self {
			// Устройство могли переименовать в панели — имя берём из списка.
			c.status.Self.Name = p.Name
			if c.status.Self.Host == "" {
				c.status.Self.Host = p.Host
			}
		}
		if !e.live && !p.Self {
			for id, name := range e.others {
				if !mine[id] {
					if name == "" {
						name = id
					}
					p.Elsewhere = append(p.Elsewhere, name)
				}
			}
			sort.Strings(p.Elsewhere)
		}
		peers = append(peers, p)
	}
	sort.SliceStable(peers, func(i, j int) bool { return peers[i].Host < peers[j].Host })
	c.status.Peers = peers
}

// updateStateLocked — общее состояние по серверам. Под c.mu.
func (c *Client) updateStateLocked() {
	errText := ""
	connecting := false
	for _, l := range c.links {
		switch l.state {
		case "online":
			c.status.State, c.status.Error = "online", ""
			return
		case "connecting":
			connecting = true
		case "error":
			if errText == "" || l.primary {
				errText = l.err
			}
		}
	}
	if connecting && errText == "" {
		c.status.State = "connecting"
		return
	}
	c.status.State, c.status.Error = "error", errText
	c.status.Self.Online = false
}

// hostOf — имя устройства с адресом ip (без «.mesh»), "" — не знаем.
// idOf — постоянный id устройства с адресом ip (из списка от meshd) или "".
func (c *Client) idOf(ip string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.status.Peers {
		if p.IP.String() == ip {
			return p.ID
		}
	}
	return ""
}

func (c *Client) hostOf(ip string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.status.Peers {
		if p.IP.String() == ip {
			return p.Host
		}
	}
	return ""
}

// Status — копия текущего состояния.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updateStateLocked()
	st := c.status
	st.Self.ID = PeerID(c.cfg.DeviceID)
	st.Peers = make([]Peer, len(c.status.Peers))
	copy(st.Peers, c.status.Peers)
	seen := map[string]bool{}
	for _, l := range c.links {
		ss := ServerStatus{Key: l.key, ID: l.info.ID, Name: l.info.Name, IP: l.info.IP,
			State: l.state, Error: l.err, Primary: l.primary}
		st.Servers = append(st.Servers, ss)
		seen[strings.ToLower(l.key)], seen[strings.ToLower(l.via)] = true, true
	}
	for _, l := range c.links {
		from := l.info.Name
		if from == "" {
			from = l.key
		}
		for _, a := range l.ads {
			if seen[strings.ToLower(a.Host)] {
				continue
			}
			seen[strings.ToLower(a.Host)] = true
			st.Advertised = append(st.Advertised, AdvertisedServer{Name: a.Name, Host: a.Host, Port: a.Port, From: from})
		}
	}
	direct := 0
	online := c.onlineLocked()
	for i := range st.Peers {
		pr := &st.Peers[i]
		if c.p2p != nil {
			if ok, rtt := c.p2p.direct(pr.IP.String()); ok {
				pr.Direct = true
				pr.DirectRTTMs = float64(rtt.Microseconds()) / 1000
				// Без сервера на связи те, до кого есть прямой путь.
				pr.Online = true
				direct++
			}
		}
		pr.NoCommon = online && !pr.Online && !pr.Self && len(pr.Elsewhere) > 0
	}
	if st.State != "online" && direct > 0 {
		st.State = "local"
	}
	return st
}

// PingMatch — отвечает ли за ping на этот адрес сеть устройств.
func (c *Client) PingMatch(dst netip.Addr) bool { return Range.Contains(dst.Unmap()) }

// Ping — «ping» до устройства или сервера сети: настоящая задержка туда и
// обратно. По прямому соединению, если оно есть, иначе через meshd.
// Ошибка — устройство не ответило (не на связи или старая версия программы).
func (c *Client) Ping(dst netip.Addr) (time.Duration, error) {
	dst = dst.Unmap()
	c.mu.Lock()
	self, p := c.status.Self.IP, c.p2p
	online := c.onlineLocked()
	c.mu.Unlock()
	if dst == self && self.IsValid() {
		return 0, nil
	}
	if p != nil && !serverNodes.Contains(dst) {
		if rtt, ok := p.ping(dst.String()); ok {
			return rtt, nil
		}
	}
	if !online {
		return 0, errors.New("сеть устройств не подключена")
	}
	id := randomString(9)
	ch := make(chan struct{})
	c.mu.Lock()
	if c.echo == nil {
		c.echo = map[string]chan struct{}{}
	}
	c.echo[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.echo, id)
		c.mu.Unlock()
	}()
	start := time.Now()
	if err := c.sendCtrl(wire{Op: "echo", To: dst.String(), Call: id}); err != nil {
		return 0, err
	}
	select {
	case <-ch:
		return time.Since(start), nil
	case <-time.After(3 * time.Second):
		return 0, errors.New("нет ответа")
	}
}

func (c *Client) direct() *p2p {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.p2p
}

// serverNodes — адреса серверов в сети устройств (198.19.255.x): к ним
// прямых соединений не бывает, только через meshd.
var serverNodes = netip.MustParsePrefix("198.19.255.0/24")

// Match — ведёт ли цель (host:port) в сеть устройств.
func (c *Client) Match(target string) bool { return IsTarget(target) }

// IsTarget — цель (host:port, имя или адрес) из сети устройств: имя «.mesh»
// или адрес 198.19.x.y. Не зависит от того, включена ли сеть, — это решает
// core.Decide.
func IsTarget(target string) bool {
	host := hostOf(target)
	if strings.HasSuffix(strings.ToLower(host), Suffix) {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && Range.Contains(a.Unmap())
}

// Resolve — адрес устройства по имени («ноутбук.mesh» или «ноутбук»).
func (c *Client) Resolve(name string) (netip.Addr, bool) {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSuffix(name, ".")), Suffix)
	c.mu.Lock()
	defer c.mu.Unlock()
	if name == c.status.Self.Host && c.status.Self.IP.IsValid() {
		return c.status.Self.IP, true
	}
	for _, p := range c.status.Peers {
		if p.Host == name {
			return p.IP, true
		}
	}
	return netip.Addr{}, false
}

// StaticDNS — ответ DNS на имя сети устройств для стека режима VPN (см.
// core.DNS.Static). ours=false — имя не из сети устройств. c может быть nil
// (сеть выключена или не подключена): тогда имена .mesh — «нет такого».
func StaticDNS(c *Client, name string) (ips []net.IP, ours bool) {
	if !strings.HasSuffix(strings.ToLower(strings.TrimSuffix(name, ".")), Suffix) {
		return nil, false
	}
	if c == nil {
		return nil, true
	}
	if ip, ok := c.Resolve(name); ok {
		return []net.IP{net.IP(ip.AsSlice())}, true
	}
	return nil, true
}

func hostOf(target string) string {
	if h, _, err := net.SplitHostPort(target); err == nil {
		return h
	}
	return target
}

// DialPeer соединяется с устройством сети. target — «имя.mesh:порт» или
// «198.19.x.y:порт».
func (c *Client) DialPeer(target string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("неверный порт %q", portStr)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		var ok bool
		if ip, ok = c.Resolve(host); !ok {
			return nil, fmt.Errorf("в сети устройств нет %q", host)
		}
	}
	ip = ip.Unmap()

	c.mu.Lock()
	self := c.status.Self.IP
	c.mu.Unlock()
	if ip == self {
		// Сам к себе — незачем гонять через сервер.
		if h := service(port); h != nil {
			c.mu.Lock()
			me := Caller{IP: self.String(), Host: c.status.Self.Host, ID: PeerID(c.cfg.DeviceID)}
			c.mu.Unlock()
			return localService(h, me), nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return c.LocalDial(ctx, port)
	}

	online := c.online()
	if p := c.direct(); p != nil && !serverNodes.Contains(ip) {
		// Есть прямой путь — по нему; нет — через сервер, а прямой ищем в
		// фоне к следующим соединениям.
		conn, err := p.dialDirect(ip.String(), port)
		if conn != nil || err != nil {
			return conn, err
		}
		if online {
			go p.connect(ip.String(), false)
		} else if p.reach(ip.String(), 3*time.Second) {
			// Без сервера: путь нашёлся по запомненным адресам или маяку.
			if conn, err := p.dialDirect(ip.String(), port); conn != nil || err != nil {
				return conn, err
			}
		}
	}
	name := c.hostOf(ip.String())
	if name == "" {
		name = ip.String()
	} else {
		name += Suffix
	}
	c.mu.Lock()
	down := !c.onlineLocked() && c.status.State == "error"
	via := c.peerOnLocked(ip.String())
	var fallback *srvLink
	if len(via) == 0 {
		for _, l := range c.links {
			if l.ctrl != nil {
				fallback = l
				break
			}
		}
	}
	var elsewhere []string
	for _, pr := range c.status.Peers {
		if pr.IP == ip && !pr.Online {
			elsewhere = pr.Elsewhere
		}
	}
	c.mu.Unlock()
	if down {
		// Сервер уже не отвечал — не ждать его 20 секунд. (Пока идёт первое
		// подключение, «connecting», — пробуем через сервер, как раньше.)
		return nil, fmt.Errorf("нет связи с сервером сети, а прямого пути к %s нет (устройство не в этой локальной сети или ни разу не соединялось напрямую)", name)
	}
	if len(via) == 0 && len(elsewhere) > 0 {
		return nil, fmt.Errorf("нет общего сервера с %s: оно на связи через %s, а это устройство к нему не подключено", name, strings.Join(elsewhere, ", "))
	}
	if len(via) == 0 {
		// Ни один сервер не видит устройство на связи — спросим первый
		// живой (или основной, пока идёт подключение): он объяснит сам.
		if fallback == nil {
			c.mu.Lock()
			fallback = c.links[0]
			c.mu.Unlock()
		}
		via = []*srvLink{fallback}
	}
	var lastErr error
	for _, l := range via {
		conn, err, retry := c.dialVia(l, ip.String(), port)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return nil, lastErr
}

// dialVia — соединение с устройством через сервер l. retry — сервер не
// ответил вовсе (упал, нет пути): стоит попробовать другой общий сервер.
// Отказ самого meshd (нет доступа, порт закрыт) — окончательный.
func (c *Client) dialVia(l *srvLink, ip string, port int) (net.Conn, error, bool) {
	c.mu.Lock()
	dial := l.dial
	c.mu.Unlock()
	if dial == nil {
		return nil, errNoServer, true
	}
	conn, err := dial("tcp", c.cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("сервис сети устройств недоступен: %w", err), true
	}
	if err := send(conn, wire{Op: "dial", Net: c.cfg.Key, Device: c.cfg.DeviceID, To: ip, Port: port}); err != nil {
		conn.Close()
		return nil, err, true
	}
	conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	line, err := readRawLine(conn)
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("сервис сети устройств не ответил: %w", err), true
	}
	var resp wire
	if err := json.Unmarshal(line, &resp); err != nil {
		conn.Close()
		return nil, err, true
	}
	if resp.Op != "connected" {
		conn.Close()
		if resp.Error == "" {
			resp.Error = "соединение не состоялось"
		}
		return nil, errors.New(resp.Error), false
	}
	return conn, nil, false
}

// readRawLine читает строку по байту: после неё идут сырые данные, и забрать
// их в буфер значило бы потерять.
func readRawLine(conn net.Conn) ([]byte, error) {
	var line []byte
	var b [1]byte
	for len(line) < 4096 {
		if _, err := conn.Read(b[:]); err != nil {
			return nil, err
		}
		if b[0] == '\n' {
			return line, nil
		}
		line = append(line, b[0])
	}
	return nil, errors.New("слишком длинная строка")
}

// incoming — другое устройство хочет соединиться с нашей службой через
// сервер l; встречное соединение — к тому же серверу.
func (c *Client) incoming(l *srvLink, m wire) {
	c.mu.Lock()
	dial := l.dial
	c.mu.Unlock()
	if dial == nil {
		return
	}
	reject := func(text string) {
		conn, err := dial("tcp", c.cfg.Addr)
		if err != nil {
			return
		}
		send(conn, wire{Op: "accept", Net: c.cfg.Key, Device: c.cfg.DeviceID, Call: m.Call, OK: false, Error: text})
		conn.Close()
	}
	h := service(m.Port)
	if h == nil && !c.cfg.AllowIncoming {
		reject("устройство не принимает входящие соединения")
		return
	}
	var local net.Conn
	var err error
	if h != nil {
		// id звонящего ставит новый meshd; старый (через этот сервер) — нет,
		// тогда по адресу из общего списка (его мог прислать другой сервер).
		id := m.FromID
		if id == "" {
			id = c.idOf(m.From)
		}
		local = localService(h, Caller{IP: m.From, Host: m.FromHost, ID: id})
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		local, err = c.LocalDial(ctx, m.Port)
		cancel()
	}
	if err != nil {
		reject(fmt.Sprintf("на устройстве ничего не слушает порт %d", m.Port))
		return
	}
	conn, err := dial("tcp", c.cfg.Addr)
	if err != nil {
		local.Close()
		return
	}
	if err := send(conn, wire{Op: "accept", Net: c.cfg.Key, Device: c.cfg.DeviceID, Call: m.Call, OK: true}); err != nil {
		conn.Close()
		local.Close()
		return
	}
	c.log("info", fmt.Sprintf("Сеть устройств: %s%s → порт %d этого устройства", m.FromHost, Suffix, m.Port))
	pump(conn, local)
}

func pump(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}
