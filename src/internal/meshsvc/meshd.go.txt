// Команда meshd — «сеть устройств» для ssh_tunnel: сервис на сервере,
// который связывает твои устройства (ноутбук, телефон, домашний сервер) в одну
// сеть, как NetBird или Tailscale, — только через твой собственный сервер.
//
// Как это устроено. Каждое устройство держит к meshd управляющее соединение и
// получает постоянный адрес из 198.19.0.0/16 и имя вида «ноутбук.mesh».
// Когда одно устройство хочет соединиться с другим, оно просит meshd; тот
// через управляющее соединение зовёт второе устройство, оно открывает
// встречное соединение, и meshd сшивает их вместе. Весь трафик идёт через
// сервер — зато не нужны ни белые адреса, ни проброс портов на роутерах: оба
// устройства и так ходят к серверу.
//
// Слушает ТОЛЬКО 127.0.0.1: достучаться до него можно лишь через уже
// прошедший SSH-аутентификацию туннель (direct-tcpip до этого же localhost).
// Устройства разных людей разделены ключом сети: видят друг друга только
// те, у кого ключ одинаковый. Сервер хранит не сам ключ, а его хеш.
//
// Протокол — по строке JSON в начале каждого соединения:
//
//	управление: → {"op":"hello","net":КЛЮЧ,"device":ID,"name":ИМЯ,
//	               "platform":..,"os":..,"app":..,"mode":..,"via":..}
//	            ← {"op":"welcome","ip":..,"host":..,"peers":[..]}
//	            дальше ← peers / incoming / pong / ping, → ping / pong
//	            (сервер шлёт ping с меткой времени и по pong меряет задержку)
//	вызов:      → {"op":"dial","net":..,"device":..,"to":IP,"port":N}
//	            ← {"op":"connected"} и дальше сырые байты — или {"op":"error"}
//	ответ:      → {"op":"accept","net":..,"device":..,"call":ID,"ok":true}
//	            и дальше сырые байты
//	сервер:     → {"op":"server","device":ID,"name":..,"host":..,"addrs":[..]}
//	            ← {"op":"welcome"}, дальше ping / pong, как у устройств
//	кто звонит: → {"op":"who","port":N,"src":M}  (служба на самом сервере)
//	            ← {"op":"whois","ok":true,"net":ID СЕТИ,"device":..,"ip":..,
//	               "host":..,"name":..,"user":..} — или {"op":"error"}
//
// Несколько серверов (с версии 4, ТЗ 12) — без главного: на каждом сервере
// свой meshd, устройство держит управляющее соединение с каждым, который
// знает, и само хранит свой адрес и имя (см. «несколько серверов без
// главного»). Новое в протоколе — только необязательные поля и операции:
//
//	hello:      + "ip","host","since" (что устройство уже держит),
//	            "alias","aliasT" (имя из панели), "quiet" (реже пинговать)
//	welcome:    + "since","alias","aliasT","server":{id,name,host,ip,pol},
//	            "servers":[{name,host,port}] (советы панели)
//	            → {"op":"self","ip","host","since",..}  ← {"op":"self",..}
//	            → {"op":"on","on":[{id,name,pol}]}  (с кем ещё на связи)
//	            ← {"op":"readdress","ip","host","since"} (адрес отдан старшему)
//	            ← {"op":"alias","alias","aliasT","host"} (переименовали в панели)
//	peers[]:    + "on":[{id,name}]
//	кто ты:     → {"op":"id"}  ← {"op":"id","server":{..}} (до hello; старый
//	            meshd отвечает ошибкой — значит, он старый)
//
// Старый способ тоже работает: на «побочном» сервере meshd не запущен, его
// 127.0.0.1:47831 — SSH-проброс (ssh -L) к meshd «главного». Сам побочный
// сервер (его панель) держит здесь управляющее соединение "server" — чтобы
// главный видел его на схеме и знал качество связи с ним.
//
// Для панели на сервере (ssh_tunnel_panel) есть отдельный вход — HTTP по
// unix-сокету (флаг -admin): список устройств с характеристиками и качеством
// связи, переименование, удаление. Сокет доступен только владельцу службы и
// root: через SSH-туннель до него не дотянуться, в отличие от порта.
//
// Клиентская сторона — пакет sshtunnel/internal/mesh. Этот файл нарочно
// самодостаточен (только стандартная библиотека): мастер настройки VPS может
// собрать его прямо на сервере одной командой "go build".
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	protoVersion = 2
	// version — версия самого meshd, её видно в панели.
	// "4" — несколько серверов без главного (ТЗ 12).
	version = "4"

	maxLine          = 4096
	maxNetworks      = 1000
	maxDevicesPerNet = 1024
	maxPendingCalls  = 256
	maxRelays        = 64

	// callTimeout — сколько ждать, пока вызванное устройство откроет встречное
	// соединение.
	callTimeout = 10 * time.Second
	// ctrlIdle — управляющее соединение, по которому дольше этого не пришло ни
	// строчки, считается мёртвым. Сервер шлёт ping раз в 15 секунд, ответ на
	// него — тоже строчка: 45 секунд — это три пропущенных ответа. Меньше
	// нельзя: на мобильном интернете пауза в полминуты — обычное дело.
	// Без этого устройство, ушедшее на другой сервер, долго висело бы «в
	// сети»: sshd на сервере сам не замечает, что клиент пропал.
	ctrlIdle = 45 * time.Second
	// forgetAfter — устройство, которое не появлялось столько времени,
	// забывается, и его адрес может достаться новому.
	forgetAfter = 180 * 24 * time.Hour
	// pingEvery — как часто сервер сам меряет задержку до устройства.
	pingEvery = 15 * time.Second
	// quietPingEvery/quietIdle — то же для дополнительного соединения
	// устройства (hello с quiet): основное уже держит связь живой, а этому
	// хватает замечать смерть за пару минут. Батарея телефона: см. MESH.md.
	quietPingEvery = 60 * time.Second
	quietIdle      = 150 * time.Second
	// maxField — предел для строк, которые устройство рассказывает о себе.
	maxField = 64
	// emptyNetKeep — пустая сеть (без устройств) удаляется сама через столько
	// после того, как из неё убрали последнее устройство.
	emptyNetKeep = 7 * 24 * time.Hour
	// maxEvents — размер журнала событий в памяти.
	maxEvents = 1000
	// maxNetName — предел длины имени сети (в символах).
	maxNetName = 64
)

// meshRange — адреса устройств. Диапазон 198.18.0.0/15 зарезервирован под
// тесты сетевого оборудования: с настоящими сетями он не пересекается, а
// первую его половину (198.18/16) уже занимают подставные адреса DNS.
var meshRange = netip.MustParsePrefix("198.19.0.0/16")

// ---------- сообщения ----------

type msg struct {
	Op       string `json:"op"`
	V        int    `json:"v,omitempty"`
	Net      string `json:"net,omitempty"`
	Device   string `json:"device,omitempty"`
	Name     string `json:"name,omitempty"`
	To       string `json:"to,omitempty"`
	Port     int    `json:"port,omitempty"`
	Call     string `json:"call,omitempty"`
	OK       bool   `json:"ok,omitempty"`
	Error    string `json:"error,omitempty"`
	IP       string `json:"ip,omitempty"`
	Host     string `json:"host,omitempty"`
	From     string `json:"from,omitempty"`
	FromHost string `json:"fromHost,omitempty"`
	// FromID — постоянный id звонящего для служб устройства (ТЗ 18), см.
	// peerID. Старые устройства его не читают.
	FromID string `json:"fromId,omitempty"`
	Peers  []peer `json:"peers,omitempty"`

	// Что устройство рассказывает о себе в hello (всё необязательное).
	Platform string `json:"platform,omitempty"` // windows, linux, android
	OS       string `json:"os,omitempty"`       // версия системы, если известна
	App      string `json:"app,omitempty"`      // версия ssh_tunnel
	Mode     string `json:"mode,omitempty"`     // proxy или vpn
	Via      string `json:"via,omitempty"`      // через какой сервер пришло
	Hostname string `json:"hostname,omitempty"` // имя компьютера

	// Addrs — адреса побочного сервера (op "server"): по ним панель узнаёт,
	// какие устройства пришли через него.
	Addrs []string `json:"addrs,omitempty"`

	// Stun — порты проверки NAT на этом сервере (в welcome устройству).
	Stun []int `json:"stun,omitempty"`
	// NAT — результат проверки NAT, который устройство прислало о себе.
	NAT *natInfo `json:"nat,omitempty"`

	// Прямые соединения (p2p): предложение и ответ пересылаются между
	// устройствами как есть — адреса-кандидаты и отпечаток ключа.
	Cands []string `json:"cands,omitempty"`
	FP    string   `json:"fp,omitempty"`
	// Direct — с кем у устройства сейчас прямое соединение (op p2pstat).
	Direct []directLink `json:"direct,omitempty"`

	// T — метка времени (наносекунды) в ping/pong для замера задержки.
	T int64 `json:"t,omitempty"`

	// Src — порт, с которого meshd подключился к службе на 127.0.0.1 (op
	// who); User — SSH-пользователь устройства в ответе whois.
	Src  int    `json:"src,omitempty"`
	User string `json:"user,omitempty"`

	// Несколько серверов без главного (ТЗ 12). В hello и self устройство
	// называет адрес и имя, которые уже держит (IP, Host), и с какого
	// времени держит адрес (Since, unix): по нему решаются споры за адрес и
	// имя (см. «адрес и имя устройства»). Alias/AliasT — имя из панели
	// какого-нибудь сервера и когда его дали (побеждает последнее). Quiet —
	// дополнительное соединение: пинговать реже (батарея телефона).
	Since  int64  `json:"since,omitempty"`
	Alias  string `json:"alias,omitempty"`
	AliasT int64  `json:"aliasT,omitempty"`
	Quiet  bool   `json:"quiet,omitempty"`
	// Server — кто этот сервер (в welcome); Servers — другие серверы сети,
	// которые советует панель этого сервера; On — с какими серверами
	// устройство сейчас на связи (op on) и чьи правила доступа там действуют.
	Server  *serverInfo  `json:"server,omitempty"`
	Servers []serverAd   `json:"servers,omitempty"`
	On      []serverSeen `json:"on,omitempty"`
}

// serverInfo — сервер глазами устройства: id, имя, адрес в сети устройств и
// отпечаток его правил доступа.
type serverInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Host string `json:"host,omitempty"`
	IP   string `json:"ip,omitempty"`
	Pol  string `json:"pol,omitempty"`
}

// serverAd — сервер сети, который советует панель: куда ещё устройству
// можно подключиться. Только подсказка — входа на тот сервер она не даёт.
type serverAd struct {
	Name string `json:"name,omitempty"`
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
}

// serverSeen — сервер, с которым устройство на связи (по его словам).
type serverSeen struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Pol  string `json:"pol,omitempty"`
}

type peer struct {
	// ID — постоянный id устройства для других устройств (ТЗ 18): не сам
	// id (он служит паролем устройства в dial/accept), а peerID от него.
	// Адрес и имя меняются, этот — нет, пока устройство не начнёт заново.
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	IP       string `json:"ip"`
	Online   bool   `json:"online"`
	LastSeen int64  `json:"lastSeen,omitempty"`
	Platform string `json:"platform,omitempty"`
	OS       string `json:"os,omitempty"`
	Via      string `json:"via,omitempty"`
	// On — с какими серверами устройство на связи (по его последнему
	// отчёту этому серверу): так другое устройство понимает, что общего
	// сервера у них нет.
	On []serverSeen `json:"on,omitempty"`
}

// ---------- состояние ----------

type device struct {
	ID string `json:"id"`
	// Name — как устройство назвало себя само; Alias — как его переименовали
	// в панели. Показывается Alias, если он есть: иначе переименование
	// жило бы до первого переподключения устройства.
	Name  string `json:"name"`
	Alias string `json:"alias,omitempty"`
	Host  string `json:"host"`
	IP    string `json:"ip"`
	// User — SSH-пользователь сервера, через которого пришло последнее
	// управляющее соединение (см. connUser). Его, а не id устройства,
	// проверяют правила доступа: id устройство называет само, а
	// пользователя не подделать без его ключа.
	User      string    `json:"user,omitempty"`
	FirstSeen time.Time `json:"firstSeen,omitempty"`
	LastSeen  time.Time `json:"lastSeen"`
	Platform  string    `json:"platform,omitempty"`
	OS        string    `json:"os,omitempty"`
	App       string    `json:"app,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Via       string    `json:"via,omitempty"`
	Hostname  string    `json:"hostname,omitempty"`
	Sessions  int64     `json:"sessions,omitempty"`
	// NAT — последняя проверка NAT с этого устройства (см. проверку NAT).
	NAT       *natInfo `json:"nat,omitempty"`
	BytesUp   int64    `json:"bytesUp,omitempty"`
	BytesDown int64    `json:"bytesDown,omitempty"`
	// Since — с какого времени (unix) устройство держит свой адрес; старше —
	// сильнее в споре за адрес. AliasT — когда дано имя из панели.
	Since  int64 `json:"since,omitempty"`
	AliasT int64 `json:"aliasT,omitempty"`
	// On — с какими серверами сети устройство на связи (последний отчёт).
	On []serverSeen `json:"on,omitempty"`

	// claims — устройство само называет адрес (новый клиент): при споре ему
	// хватает сообщения readdress, старого приходится переподключать.
	claims      bool
	quiet       bool
	ctrl        *ctrlConn
	connectedAt time.Time
	direct      []directLink // прямые соединения (по отчёту устройства)
	p2pBudget   int          // сколько предложений p2p осталось в эту минуту
	p2pMinute   int64
	echoBudget  int // сколько ping к другим устройствам осталось в эту минуту
	echoMinute  int64
	up, down    atomic.Int64 // трафик за время жизни процесса, поверх Bytes*
	activeCalls atomic.Int64
	link        linkStats
}

// displayName — имя для людей: переименование из панели главнее.
func (d *device) displayName() string {
	if d.Alias != "" {
		return d.Alias
	}
	return d.Name
}

// linkStats — качество связи с устройством по замерам ping/pong. Под s.mu.
type linkStats struct {
	sent, got int
	last      time.Duration
	avg       float64 // миллисекунды, скользящее среднее
	jitter    float64 // миллисекунды, скользящее среднее отклонения
}

func (l *linkStats) add(rtt time.Duration) {
	ms := float64(rtt) / float64(time.Millisecond)
	if l.got == 0 {
		l.avg = ms
	} else {
		l.jitter = 0.7*l.jitter + 0.3*math.Abs(ms-float64(l.last)/float64(time.Millisecond))
		l.avg = 0.7*l.avg + 0.3*ms
	}
	l.last = rtt
	l.got++
}

// loss — доля пропавших замеров, в процентах. Последний отправленный ping
// мог ещё не вернуться — его не считаем.
func (l *linkStats) loss() float64 {
	if l.sent <= 1 {
		return 0
	}
	lost := l.sent - 1 - l.got
	if lost < 0 {
		lost = 0
	}
	return 100 * float64(lost) / float64(l.sent-1)
}

// quality — оценка связи словом, для значка в панели.
func (l *linkStats) quality() string {
	switch {
	case l.got == 0:
		return "unknown"
	case l.loss() > 20 || l.avg > 400:
		return "poor"
	case l.loss() > 5 || l.avg > 150:
		return "fair"
	case l.avg > 60:
		return "good"
	default:
		return "excellent"
	}
}

type network struct {
	Devices map[string]*device `json:"devices"`
	// Name — имя сети из панели (необязательное). Created — когда сеть
	// появилась, EmptySince — с какого момента в ней нет устройств (0 — есть);
	// оба в секундах unix. В старых файлах состояния этих полей нет.
	Name       string `json:"name,omitempty"`
	Created    int64  `json:"created,omitempty"`
	EmptySince int64  `json:"emptySince,omitempty"`
}

type call struct {
	net    string
	target string // id вызванного устройства
	answer chan answer
}

type answer struct {
	conn net.Conn // nil — устройство отказалось
	err  string
}

// relay — побочный сервер, подключённый к этому. В памяти: после
// перезапуска он переподключится сам.
type relay struct {
	ID, Name, Host, App string
	Addrs               []string
	connectedAt         time.Time
	ctrl                *ctrlConn
	link                linkStats
}

type server struct {
	mu     sync.Mutex
	nets   map[string]*network // по хешу ключа
	calls  map[string]*call
	relays map[string]*relay
	// self — сам этот сервер как узел сети; relayRecs — адреса побочных
	// серверов (постоянные, как у устройств). Хранятся в servers.json.
	self      selfConfig
	relayRecs map[string]*relayRecord
	// stunPorts — порты проверки NAT (UDP), если она включена.
	stunPorts []int
	// policy — права доступа устройств (см. «права доступа»). Хранятся в
	// policy.json, задаёт панель.
	policy policy
	// connUser узнаёт SSH-пользователя соединения; в тестах подменяется.
	connUser func(net.Conn) string
	// local — соединения устройств со службами самого сервера (127.0.0.1)
	// по порту, с которого их открыл meshd: по нему служба спрашивает «кто
	// это» (op who). sameOwner — тот ли пользователь системы спрашивает, что
	// слушает порт службы; в тестах подменяется.
	local     map[int]*localCall
	sameOwner func(conn net.Conn, port int) bool
	path      string
	saveMu    sync.Mutex
	dirty     chan struct{}
	started   time.Time
	// now — часы; в тестах подменяются.
	now func() time.Time
	// events — журнал событий (кольцо на maxEvents), evSeq — номер последнего.
	// Только в памяти: после перезапуска meshd журнал пуст, нумерация
	// начинается заново.
	events []event
	evSeq  int64
	// hadState — при запуске нашлось прежнее состояние (сервер обновили, а
	// не поставили заново): см. ensureSelfLocked.
	hadState bool
}

func newServer(path string) *server {
	s := &server{
		nets:   map[string]*network{},
		calls:  map[string]*call{},
		relays: map[string]*relay{},
		self:   selfConfig{Enabled: true},
		path:   path,

		relayRecs: map[string]*relayRecord{},
		connUser:  procConnUser,
		local:     map[int]*localCall{},
		sameOwner: procSameOwner,
		dirty:     make(chan struct{}, 1),
		started:   time.Now(),
		now:       time.Now,
	}
	s.load()
	s.loadPolicy()
	s.ensureSelfLocked()
	return s
}

// peerID — постоянный id устройства, который видят другие устройства
// (права зеркальных папок, ТЗ 18). По нему не восстановить id устройства,
// поэтому позвонить от чужого имени с ним нельзя. Так же считает
// internal/mesh (PeerID) — тест сверяет.
func peerID(device string) string {
	if device == "" {
		return ""
	}
	h := sha256.Sum256([]byte("ssh_tunnel peer v1\x00" + device))
	return hex.EncodeToString(h[:12])
}

func netID(key string) string {
	h := sha256.Sum256([]byte("ssh_tunnel mesh v1\x00" + key))
	return hex.EncodeToString(h[:])
}

func (s *server) load() {
	if s.path == "" {
		return
	}
	s.loadServers()
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	s.hadState = true
	var nets map[string]*network
	if err := json.Unmarshal(data, &nets); err != nil {
		log.Printf("состояние %s повреждено, начинаю с чистого: %v", s.path, err)
		return
	}
	now := time.Now()
	for id, n := range nets {
		if n == nil || n.Devices == nil {
			continue
		}
		for did, d := range n.Devices {
			if now.Sub(d.LastSeen) > forgetAfter {
				delete(n.Devices, did)
			}
		}
		// Старые файлы без created/emptySince: дата создания — по самому
		// раннему устройству, пустая сеть считается пустой с этого момента.
		if n.Created == 0 {
			for _, d := range n.Devices {
				if u := unixOrZero(d.FirstSeen); u != 0 && (n.Created == 0 || u < n.Created) {
					n.Created = u
				}
			}
			if n.Created == 0 {
				n.Created = now.Unix()
			}
		}
		if len(n.Devices) == 0 && n.EmptySince == 0 {
			n.EmptySince = now.Unix()
		}
		if len(n.Devices) > 0 {
			n.EmptySince = 0
		}
		s.nets[id] = n
	}
	s.pruneLocked(now)
}

// saveLoop пишет состояние на диск не чаще раза в секунду.
func (s *server) saveLoop() {
	for range s.dirty {
		time.Sleep(time.Second)
		s.save()
	}
}

func (s *server) markDirty() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *server) save() {
	if s.path == "" {
		return
	}
	s.mu.Lock()
	// Живые счётчики трафика — поверх сохранённых: на диск идёт сумма.
	type saved struct {
		d        *device
		up, down int64
	}
	var restore []saved
	for _, n := range s.nets {
		for _, d := range n.Devices {
			restore = append(restore, saved{d, d.BytesUp, d.BytesDown})
			d.BytesUp += d.up.Load()
			d.BytesDown += d.down.Load()
		}
	}
	data, err := json.MarshalIndent(s.nets, "", " ")
	for _, r := range restore {
		r.d.BytesUp, r.d.BytesDown = r.up, r.down
	}
	srv, _ := json.MarshalIndent(serversFile{Self: s.self, Relays: s.relayRecs}, "", " ")
	s.mu.Unlock()
	if err != nil {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	os.MkdirAll(filepath.Dir(s.path), 0o700)
	if srv != nil {
		tmp := s.serversPath() + ".tmp"
		if os.WriteFile(tmp, srv, 0o600) == nil {
			os.Rename(tmp, s.serversPath())
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("не удалось сохранить состояние: %v", err)
		return
	}
	os.Rename(tmp, s.path)
}

// peersLocked — список устройств сети для устройства me: только те, с
// кем ему разрешено общаться хоть в одну сторону (и оно само). Под s.mu.
func (s *server) peersLocked(n *network, me *device) []peer {
	out := make([]peer, 0, len(n.Devices)+len(s.relayRecs)+1)
	for _, d := range n.Devices {
		if d != me && !s.policy.visible(me.User, d.User) {
			continue
		}
		p := peer{ID: peerID(d.ID), Name: d.displayName(), Host: d.Host, IP: d.IP, Online: d.ctrl != nil,
			Platform: d.Platform, OS: d.OS, Via: d.Via}
		for _, o := range d.On {
			p.On = append(p.On, serverSeen{ID: o.ID, Name: o.Name})
		}
		if !d.LastSeen.IsZero() {
			p.LastSeen = d.LastSeen.Unix()
		}
		out = append(out, p)
	}
	if s.policy.visible(me.User, serversGroup) {
		out = append(out, s.serverPeersLocked(n)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// broadcastLocked рассылает всем подключённым устройствам сети свежий
// список. Под s.mu.
func (s *server) broadcastLocked(n *network) {
	for _, d := range n.Devices {
		if d.ctrl != nil {
			d.ctrl.send(msg{Op: "peers", Peers: s.peersLocked(n, d)})
		}
	}
}

// allocIPLocked выдаёт свободный адрес в сети. Под s.mu.
func allocIPLocked(n *network) (string, bool) {
	used := map[string]bool{}
	for _, d := range n.Devices {
		used[d.IP] = true
	}
	a := meshRange.Addr().Next() // .0.0 — адрес сети, пропускаем
	for meshRange.Contains(a) {
		b := a.As4()
		if b[3] != 0 && b[3] != 255 && !used[a.String()] && !serverBlock.Contains(a) {
			return a.String(), true
		}
		a = a.Next()
	}
	return "", false
}

// hostLabel делает из имени устройства имя для DNS: латиница, цифры и
// дефисы. Кириллица переводится в латиницу.
func hostLabel(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case translit[r] != "":
			b.WriteString(translit[r])
			dash = false
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '.':
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
		if b.Len() >= 40 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh",
	'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o",
	'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "h", 'ц': "ts",
	'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu",
	'я': "ya", 'і': "i", 'ї': "yi", 'є': "ye", 'ґ': "g",
}

// uniqueHostLocked — hostLabel(name), а при совпадении с чужим — с номером.
func (s *server) uniqueHostLocked(n *network, self, name string) string {
	base := hostLabel(name)
	if base == "" {
		base = "device"
	}
	servers := s.serverLabelsLocked()
	taken := func(h string) bool {
		if servers[h] {
			return true
		}
		for id, d := range n.Devices {
			if id != self && d.Host == h {
				return true
			}
		}
		return false
	}
	h := base
	for i := 2; taken(h); i++ {
		h = base + "-" + itoa(i)
	}
	return h
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// ---------- соединения ----------

// ctrlConn — управляющее соединение устройства. Пишется из отдельной
// горутины через очередь: иначе одно медленное устройство держало бы общую
// блокировку, пока ему шлют список, и тормозило бы всех остальных.
type ctrlConn struct {
	conn net.Conn
	out  chan msg
	once sync.Once
}

func newCtrlConn(conn net.Conn) *ctrlConn {
	c := &ctrlConn{conn: conn, out: make(chan msg, 64)}
	go func() {
		enc := json.NewEncoder(conn)
		for m := range c.out {
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if enc.Encode(m) != nil {
				conn.Close()
			}
		}
	}()
	return c
}

// send не ждёт: очередь переполнена — устройство не успевает читать, и
// соединение с ним закрывается (оно переподключится само).
func (c *ctrlConn) send(m msg) {
	defer func() { recover() }() // очередь уже закрыта
	select {
	case c.out <- m:
	default:
		c.conn.Close()
	}
}

func (c *ctrlConn) close() {
	c.once.Do(func() { close(c.out) })
}

func writeMsg(conn net.Conn, m msg) error {
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetWriteDeadline(time.Time{})
	return json.NewEncoder(conn).Encode(m)
}

// readFirst читает первую строку соединения, не заглядывая дальше неё:
// после неё могут идти сырые байты, и их нельзя проглотить в буфер.
func readFirst(conn net.Conn) (msg, error) {
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	var line []byte
	var b [1]byte
	for len(line) < maxLine {
		if _, err := conn.Read(b[:]); err != nil {
			return msg{}, err
		}
		if b[0] == '\n' {
			var m msg
			err := json.Unmarshal(line, &m)
			return m, err
		}
		line = append(line, b[0])
	}
	return msg{}, errors.New("слишком длинная строка")
}

func (s *server) handle(conn net.Conn) {
	m, err := readFirst(conn)
	if err != nil {
		conn.Close()
		return
	}
	if m.Op == "server" {
		s.relayLink(conn, m)
		return
	}
	if m.Op == "who" {
		s.who(conn, m)
		return
	}
	if m.Op == "id" {
		// Кто этот сервер — до hello: так устройство узнаёт, что два его
		// пути ведут в один meshd, и не выбивает своё же соединение.
		s.mu.Lock()
		info := s.infoLocked()
		s.mu.Unlock()
		writeMsg(conn, msg{Op: "id", Server: info})
		conn.Close()
		return
	}
	if m.Net == "" || m.Device == "" || len(m.Device) > 128 {
		writeMsg(conn, msg{Op: "error", Error: "нет ключа сети или id устройства"})
		conn.Close()
		return
	}
	switch m.Op {
	case "hello":
		s.control(conn, m)
	case "dial":
		s.dial(conn, m)
	case "accept":
		s.accept(conn, m)
	default:
		writeMsg(conn, msg{Op: "error", Error: "неизвестная операция"})
		conn.Close()
	}
}

func (s *server) control(conn net.Conn, m msg) {
	defer conn.Close()
	id := netID(m.Net)
	name := strings.TrimSpace(m.Name)
	if name == "" {
		name = "устройство"
	}
	if len([]rune(name)) > 64 {
		name = string([]rune(name)[:64])
	}
	user := s.connUser(conn)
	c := newCtrlConn(conn)
	defer c.close()

	s.mu.Lock()
	n := s.nets[id]
	if n == nil {
		if len(s.nets) >= maxNetworks {
			s.mu.Unlock()
			writeMsg(conn, msg{Op: "error", Error: "на сервере слишком много сетей"})
			return
		}
		n = &network{Devices: map[string]*device{}, Created: s.now().Unix()}
		s.nets[id] = n
	}
	d := n.Devices[m.Device]
	newDevice := false
	if d == nil {
		if len(n.Devices) >= maxDevicesPerNet {
			s.mu.Unlock()
			writeMsg(conn, msg{Op: "error", Error: "в сети слишком много устройств"})
			return
		}
		if _, ok := allocIPLocked(n); !ok {
			s.mu.Unlock()
			writeMsg(conn, msg{Op: "error", Error: "кончились адреса"})
			return
		}
		d = &device{ID: m.Device}
		n.Devices[m.Device] = d
		n.EmptySince = 0
		newDevice = true
	}
	if d.ctrl != nil {
		// То же устройство подключилось заново (сменилась сеть) — старое
		// соединение уже не нужно.
		d.ctrl.conn.Close()
	}
	now := time.Now()
	d.Name = name
	d.User = user
	d.claims = m.Since > 0
	d.quiet = m.Quiet
	d.On = nil // новый клиент расскажет о других серверах заново (op on)
	s.aliasLocked(d, m.Alias, m.AliasT)
	s.claimLocked(id, n, d, m.IP, m.Host, m.Since)
	if d.FirstSeen.IsZero() {
		d.FirstSeen = now
	}
	d.LastSeen = now
	// Необязательные сведения: старый клиент их не шлёт — прежние не стираем.
	for dst, src := range map[*string]string{
		&d.Platform: m.Platform, &d.OS: m.OS, &d.App: m.App,
		&d.Mode: m.Mode, &d.Via: m.Via, &d.Hostname: m.Hostname,
	} {
		if v := clip(src); v != "" {
			*dst = v
		}
	}
	d.Sessions++
	d.connectedAt = now
	d.link = linkStats{}
	d.ctrl = c
	c.send(msg{Op: "welcome", V: protoVersion, IP: d.IP, Host: d.Host, Since: d.Since,
		Alias: d.Alias, AliasT: d.AliasT, Peers: s.peersLocked(n, d), Stun: s.stunPorts,
		Server: s.infoLocked(), Servers: s.self.Known})
	s.broadcastLocked(n)
	if newDevice {
		s.emitLocked(event{Type: "new-device", Net: id, Device: d.ID, Text: d.displayName()})
	}
	s.emitLocked(event{Type: "connect", Net: id, Device: d.ID, Text: d.displayName()})
	s.mu.Unlock()
	s.markDirty()

	every, idle := pingEvery, ctrlIdle
	if m.Quiet {
		every, idle = quietPingEvery, quietIdle
	}
	s.keepAlive(conn, c, every, idle, func(f func(*linkStats)) {
		s.mu.Lock()
		if d.ctrl == c {
			f(&d.link)
		}
		s.mu.Unlock()
	}, func(in msg) {
		if in.Op == "self" || in.Op == "on" {
			s.selfMessage(id, n, d, c, in)
			return
		}
		if in.Op == "p2p" || in.Op == "p2pstat" {
			s.p2pMessage(n, d, c, in)
			return
		}
		if in.Op == "echo" || in.Op == "echoreply" {
			s.echoMessage(n, d, c, in)
			return
		}
		if in.Op == "nat" && in.NAT != nil {
			info := in.NAT.clean()
			s.mu.Lock()
			if d.ctrl == c {
				d.NAT = &info
			}
			s.mu.Unlock()
			s.markDirty()
		}
	})

	s.mu.Lock()
	if d.ctrl == c {
		d.ctrl = nil
		d.direct = nil
		d.LastSeen = time.Now()
		s.broadcastLocked(n)
		s.emitLocked(event{Type: "disconnect", Net: id, Device: d.ID, Text: d.displayName()})
	}
	s.mu.Unlock()
	s.markDirty()
}

// keepAlive обслуживает управляющее соединение, пока оно живо: отвечает на
// ping и сам меряет задержку. link даёт доступ к замерам под блокировкой —
// или не вызывает f вовсе, если соединение уже сменилось новым.
//
// Замер: сервер шлёт ping с меткой времени, другая сторона возвращает её в
// pong. Старые клиенты незнакомое сообщение пропускают — у них задержка
// просто остаётся неизвестной.
func (s *server) keepAlive(conn net.Conn, c *ctrlConn, every, idle time.Duration, link func(func(*linkStats)), other func(msg)) {
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		send := func() {
			link(func(l *linkStats) { l.sent++ })
			c.send(msg{Op: "ping", T: time.Now().UnixNano()})
		}
		send()
		for {
			select {
			case <-stopPing:
				return
			case <-t.C:
				send()
			}
		}
	}()

	r := bufio.NewReaderSize(conn, maxLine)
	for {
		conn.SetReadDeadline(time.Now().Add(idle))
		line, err := r.ReadSlice('\n')
		if err != nil {
			return
		}
		var in msg
		if json.Unmarshal(line, &in) != nil {
			continue
		}
		switch in.Op {
		case "ping":
			c.send(msg{Op: "pong", T: in.T})
		case "pong":
			if in.T > 0 {
				rtt := time.Since(time.Unix(0, in.T))
				if rtt >= 0 && rtt < time.Minute {
					link(func(l *linkStats) { l.add(rtt) })
				}
			}
		default:
			if other != nil {
				other(in)
			}
		}
	}
}

// relayLink — управляющее соединение побочного сервера.
func (s *server) relayLink(conn net.Conn, m msg) {
	defer conn.Close()
	id := clip(m.Device)
	if id == "" {
		writeMsg(conn, msg{Op: "error", Error: "нет id сервера"})
		return
	}
	c := newCtrlConn(conn)
	defer c.close()
	r := &relay{ID: id, Name: clip(m.Name), Host: clip(m.Host), App: clip(m.App), connectedAt: time.Now(), ctrl: c}
	for _, a := range m.Addrs {
		if v := clip(a); v != "" && len(r.Addrs) < 8 {
			r.Addrs = append(r.Addrs, v)
		}
	}

	s.mu.Lock()
	if old := s.relays[id]; old != nil {
		old.ctrl.conn.Close()
	} else if len(s.relays) >= maxRelays {
		s.mu.Unlock()
		writeMsg(conn, msg{Op: "error", Error: "подключено слишком много серверов"})
		return
	}
	rec := s.relayRecs[id]
	if rec == nil {
		ip := s.allocRelayIPLocked()
		if ip == "" {
			s.mu.Unlock()
			writeMsg(conn, msg{Op: "error", Error: "кончились адреса для серверов"})
			return
		}
		rec = &relayRecord{IP: ip}
		s.relayRecs[id] = rec
	}
	rec.Name = r.Name
	if rec.Name == "" {
		rec.Name = r.Host
	}
	rec.LastSeen = time.Now()
	s.relays[id] = r
	s.broadcastAllLocked()
	s.emitLocked(event{Type: "server-up", Device: id, Text: rec.Name})
	s.mu.Unlock()
	s.markDirty()
	log.Printf("подключился сервер %q (%s), адрес в сети %s", r.Name, r.Host, rec.IP)
	s.mu.Lock()
	welcome := msg{Op: "welcome", V: protoVersion, IP: rec.IP, Host: hostLabel(rec.Name)}
	s.mu.Unlock()
	c.send(welcome)

	s.keepAlive(conn, c, pingEvery, ctrlIdle, func(f func(*linkStats)) {
		s.mu.Lock()
		if s.relays[id] == r {
			f(&r.link)
		}
		s.mu.Unlock()
	}, nil)

	s.mu.Lock()
	if s.relays[id] == r {
		delete(s.relays, id)
		if rec := s.relayRecs[id]; rec != nil {
			rec.LastSeen = time.Now()
		}
		s.broadcastAllLocked()
		s.emitLocked(event{Type: "server-down", Device: id, Text: r.Name})
	}
	s.mu.Unlock()
	s.markDirty()
	log.Printf("сервер %q отключился", r.Name)
}

// clip обрезает строку, которую устройство прислало о себе, до разумной.
func clip(v string) string {
	v = strings.TrimSpace(v)
	if r := []rune(v); len(r) > maxField {
		v = string(r[:maxField])
	}
	return v
}

func (s *server) dial(conn net.Conn, m msg) {
	id := netID(m.Net)
	fail := func(text string) {
		writeMsg(conn, msg{Op: "error", Error: text})
		conn.Close()
	}
	if m.Port <= 0 || m.Port > 65535 {
		fail("неверный порт")
		return
	}

	s.mu.Lock()
	n := s.nets[id]
	if n == nil || n.Devices[m.Device] == nil {
		s.mu.Unlock()
		fail("это устройство не в сети — сначала нужно управляющее соединение")
		return
	}
	from := n.Devices[m.Device]
	user := s.connUser(conn)
	if user != from.User {
		// Чужой id устройства: права считаются по тому, кто звонит на
		// самом деле, а адрес вызывающего — по устройству. Не сходится —
		// не пускаем, иначе можно звонить от имени чужого устройства.
		s.mu.Unlock()
		fail("это устройство подключено под другим пользователем")
		return
	}
	if ip, ok := s.serverTargetLocked(n, m.To); ok {
		if !s.policy.allowed(user, serversGroup, m.Port) {
			s.emitLocked(event{Type: "denied", Net: id, Device: from.ID, Peer: m.To, Port: m.Port, Text: "server"})
			s.mu.Unlock()
			fail(deniedText)
			return
		}
		s.dialServer(conn, id, from, ip, m.Port, fail) // отпускает s.mu
		return
	}
	var target *device
	for _, d := range n.Devices {
		if d.IP == m.To || d.Host == m.To {
			target = d
			break
		}
	}
	if target == nil || !s.policy.visible(user, target.User) {
		s.mu.Unlock()
		fail("нет такого устройства в сети")
		return
	}
	if !s.policy.allowed(user, target.User, m.Port) {
		s.emitLocked(event{Type: "denied", Net: id, Device: from.ID, Peer: target.ID, Port: m.Port})
		s.mu.Unlock()
		fail(deniedText)
		return
	}
	if target.ctrl == nil {
		text := "устройство " + target.Host + " сейчас не в сети"
		if others := s.otherServersLocked(target.On); others != "" {
			text = "нет общего сервера: " + target.Host + " сейчас на связи только через " + others
		}
		s.mu.Unlock()
		fail(text)
		return
	}
	if len(s.calls) >= maxPendingCalls {
		s.mu.Unlock()
		fail("сервер занят, попробуй ещё раз")
		return
	}
	callID := randomID()
	cl := &call{net: id, target: target.ID, answer: make(chan answer, 1)}
	s.calls[callID] = cl
	target.ctrl.send(msg{Op: "incoming", Call: callID, From: from.IP, FromHost: from.Host, FromID: peerID(from.ID), Port: m.Port})
	s.mu.Unlock()

	var got answer
	timedOut := false
	select {
	case got = <-cl.answer:
	case <-time.After(callTimeout):
		timedOut = true
	}
	s.mu.Lock()
	delete(s.calls, callID)
	s.mu.Unlock()
	if timedOut {
		// Ответ мог успеть прийти между таймаутом и удалением вызова —
		// тогда его соединение никому не нужно.
		select {
		case late := <-cl.answer:
			if late.conn != nil {
				late.conn.Close()
			}
		default:
		}
		fail("устройство не ответило")
		return
	}
	if got.conn == nil {
		if got.err == "" {
			got.err = "устройство отказало в соединении"
		}
		fail(got.err)
		return
	}
	peerConn := got.conn
	if err := writeMsg(conn, msg{Op: "connected"}); err != nil {
		conn.Close()
		peerConn.Close()
		return
	}
	// Трафик — обоим устройствам: вызывающему «отдано» то, что ушло к
	// вызванному, и наоборот.
	from.activeCalls.Add(1)
	target.activeCalls.Add(1)
	defer from.activeCalls.Add(-1)
	defer target.activeCalls.Add(-1)
	splice(conn, peerConn, &from.up, &target.down, &target.up, &from.down)
	s.markDirty()
}

func (s *server) accept(conn net.Conn, m msg) {
	id := netID(m.Net)
	s.mu.Lock()
	cl := s.calls[m.Call]
	if cl == nil || cl.net != id || cl.target != m.Device {
		s.mu.Unlock()
		conn.Close()
		return
	}
	delete(s.calls, m.Call)
	s.mu.Unlock()

	if !m.OK {
		// Устройство отказалось (порт закрыт, входящие выключены) — говорим
		// вызывающему сразу, а не по таймауту.
		conn.Close()
		cl.answer <- answer{err: m.Error}
		return
	}
	cl.answer <- answer{conn: conn}
}

// splice перекладывает байты в обе стороны и ждёт, пока закончат обе.
// Счётчики: aUp/bDown — то, что пришло от a и ушло к b; bUp/aDown — обратно.
func splice(a, b net.Conn, aUp, bDown, bUp, aDown *atomic.Int64) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn, c1, c2 *atomic.Int64) {
		io.Copy(dst, countingReader{src, c1, c2})
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(b, a, aUp, bDown)
	go cp(a, b, bUp, aDown)
	<-done
	<-done
	a.Close()
	b.Close()
}

type countingReader struct {
	r      io.Reader
	c1, c2 *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.c1.Add(int64(n))
		c.c2.Add(int64(n))
	}
	return n, err
}

// ---------- вход для панели (HTTP по unix-сокету) ----------

// adminDevice — устройство глазами панели.
type adminDevice struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Alias       string       `json:"alias,omitempty"`
	Display     string       `json:"display"`
	Host        string       `json:"host"`
	IP          string       `json:"ip"`
	User        string       `json:"user,omitempty"`
	Online      bool         `json:"online"`
	Platform    string       `json:"platform,omitempty"`
	OS          string       `json:"os,omitempty"`
	App         string       `json:"app,omitempty"`
	Mode        string       `json:"mode,omitempty"`
	Via         string       `json:"via,omitempty"`
	Hostname    string       `json:"hostname,omitempty"`
	FirstSeen   int64        `json:"firstSeen,omitempty"`
	LastSeen    int64        `json:"lastSeen,omitempty"`
	ConnectedAt int64        `json:"connectedAt,omitempty"`
	Sessions    int64        `json:"sessions"`
	RTTMs       float64      `json:"rttMs,omitempty"`
	RTTAvgMs    float64      `json:"rttAvgMs,omitempty"`
	JitterMs    float64      `json:"jitterMs,omitempty"`
	LossPct     float64      `json:"lossPct"`
	Quality     string       `json:"quality"`
	BytesUp     int64        `json:"bytesUp"`
	BytesDown   int64        `json:"bytesDown"`
	ActiveCalls int64        `json:"activeCalls"`
	NAT         *natInfo     `json:"nat,omitempty"`
	Direct      []directLink `json:"direct,omitempty"`
}

type adminNetwork struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Created    int64         `json:"created"`
	EmptySince int64         `json:"emptySince"` // 0 — в сети есть устройства
	Devices    []adminDevice `json:"devices"`
}

// adminServer — подключённый побочный сервер глазами панели.
type adminServer struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Host        string   `json:"host"`
	IP          string   `json:"ip,omitempty"` // адрес в сети устройств
	MeshHost    string   `json:"meshHost,omitempty"`
	Addrs       []string `json:"addrs,omitempty"`
	App         string   `json:"app,omitempty"`
	ConnectedAt int64    `json:"connectedAt"`
	RTTMs       float64  `json:"rttMs,omitempty"`
	RTTAvgMs    float64  `json:"rttAvgMs,omitempty"`
	JitterMs    float64  `json:"jitterMs,omitempty"`
	LossPct     float64  `json:"lossPct"`
	Quality     string   `json:"quality"`
}

// adminSelf — этот сервер как узел сети.
type adminSelf struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	IP      string `json:"ip"`
	Enabled bool   `json:"enabled"`
	Ports   string `json:"ports,omitempty"`
	// ID — id этого сервера; Pol — отпечаток его правил доступа.
	ID  string `json:"id,omitempty"`
	Pol string `json:"pol,omitempty"`
}

// adminSeen — другой сервер сети, о котором рассказали устройства: с ним
// они тоже на связи. Pol — отпечаток его правил доступа, Same — совпадают
// ли они с правилами этого сервера.
type adminSeen struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Pol      string `json:"pol,omitempty"`
	Same     bool   `json:"same"`
	Devices  int    `json:"devices"`
	LastSeen int64  `json:"lastSeen,omitempty"`
}

type adminState struct {
	Version   string         `json:"version"`
	StartedAt int64          `json:"startedAt"`
	Networks  []adminNetwork `json:"networks"`
	Servers   []adminServer  `json:"servers"`
	Self      adminSelf      `json:"self"`
	// Peers — другие серверы сети по рассказам устройств; Known — серверы,
	// которые советует панель (ТЗ 12).
	Peers []adminSeen `json:"peers"`
	Known []serverAd  `json:"known"`
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func (s *server) adminState() adminState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := adminState{Version: version, StartedAt: s.started.Unix(), Networks: []adminNetwork{}}
	for id, n := range s.nets {
		an := adminNetwork{ID: id, Name: n.Name, Created: n.Created, EmptySince: n.EmptySince}
		for _, d := range n.Devices {
			ad := adminDevice{
				ID: d.ID, Name: d.Name, Alias: d.Alias, Display: d.displayName(),
				Host: d.Host, IP: d.IP, User: d.User, Online: d.ctrl != nil,
				Platform: d.Platform, OS: d.OS, App: d.App, Mode: d.Mode, Via: d.Via,
				Hostname: d.Hostname, FirstSeen: unixOrZero(d.FirstSeen), LastSeen: unixOrZero(d.LastSeen),
				Sessions:  d.Sessions,
				NAT:       d.NAT,
				Direct:    d.direct,
				BytesUp:   d.BytesUp + d.up.Load(),
				BytesDown: d.BytesDown + d.down.Load(),
				Quality:   "unknown",
			}
			if d.ctrl != nil {
				ad.ConnectedAt = unixOrZero(d.connectedAt)
				ad.ActiveCalls = d.activeCalls.Load()
				ad.Quality = d.link.quality()
				ad.LossPct = round1(d.link.loss())
				if d.link.got > 0 {
					ad.RTTMs = round1(float64(d.link.last) / float64(time.Millisecond))
					ad.RTTAvgMs = round1(d.link.avg)
					ad.JitterMs = round1(d.link.jitter)
				}
			}
			an.Devices = append(an.Devices, ad)
		}
		sort.Slice(an.Devices, func(i, j int) bool { return an.Devices[i].IP < an.Devices[j].IP })
		st.Networks = append(st.Networks, an)
	}
	sort.Slice(st.Networks, func(i, j int) bool { return len(st.Networks[i].Devices) > len(st.Networks[j].Devices) })
	st.Servers = []adminServer{}
	for _, r := range s.relays {
		as := adminServer{
			ID: r.ID, Name: r.Name, Host: r.Host, Addrs: r.Addrs, App: r.App,
			ConnectedAt: unixOrZero(r.connectedAt),
			Quality:     r.link.quality(),
			LossPct:     round1(r.link.loss()),
		}
		if r.link.got > 0 {
			as.RTTMs = round1(float64(r.link.last) / float64(time.Millisecond))
			as.RTTAvgMs = round1(r.link.avg)
			as.JitterMs = round1(r.link.jitter)
		}
		if rec := s.relayRecs[r.ID]; rec != nil {
			as.IP, as.MeshHost = rec.IP, hostLabel(rec.Name)
		}
		st.Servers = append(st.Servers, as)
	}
	sort.Slice(st.Servers, func(i, j int) bool { return st.Servers[i].Name < st.Servers[j].Name })
	st.Self = adminSelf{Name: s.selfNameLocked(), Host: hostLabel(s.selfNameLocked()), IP: s.self.IP, ID: s.self.ID, Pol: s.policyFPLocked(),
		Enabled: s.self.Enabled, Ports: s.self.Ports}
	st.Peers = s.seenLocked()
	st.Known = append([]serverAd{}, s.self.Known...)
	return st
}

// rename задаёт устройству имя из панели. Пустое — вернуть его собственное.
func (s *server) rename(netID, devID, alias string) error {
	alias = clip(alias)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nets[netID]
	if n == nil || n.Devices[devID] == nil {
		return errors.New("нет такого устройства")
	}
	d := n.Devices[devID]
	d.Alias = alias
	d.AliasT = s.now().Unix()
	s.hostLocked(netID, n, d, "")
	s.emitLocked(event{Type: "rename", Net: netID, Device: devID, Text: d.displayName()})
	// Новый клиент запомнит имя и отнесёт его на остальные серверы сети.
	if d.ctrl != nil {
		d.ctrl.send(msg{Op: "alias", Alias: d.Alias, AliasT: d.AliasT, Host: d.Host})
	}
	s.broadcastLocked(n)
	s.markDirty()
	return nil
}

// forget убирает устройство из сети: его адрес освобождается. Если оно
// сейчас на связи — соединение рвётся; вернуться оно сможет, но уже как новое.
func (s *server) forget(netID, devID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nets[netID]
	if n == nil || n.Devices[devID] == nil {
		return errors.New("нет такого устройства")
	}
	d := n.Devices[devID]
	delete(n.Devices, devID)
	if d.ctrl != nil {
		// Новый клиент хранит имя из панели сам и принёс бы его обратно —
		// снимаем и у него (позднее имя побеждает, пустое тоже).
		if c := d.ctrl; d.Alias != "" {
			c.send(msg{Op: "alias", Alias: "", AliasT: s.now().Unix() + 1})
			time.AfterFunc(300*time.Millisecond, func() { c.conn.Close() })
		} else {
			c.conn.Close()
		}
		d.ctrl = nil
	}
	if len(n.Devices) == 0 {
		n.EmptySince = s.now().Unix()
	}
	s.emitLocked(event{Type: "forget", Net: netID, Device: devID, Text: d.displayName()})
	s.broadcastLocked(n)
	s.markDirty()
	return nil
}

// ---------- сети: имя, удаление, уборка пустых ----------

// validNetName — имя сети из панели: пустое (убрать имя) или до maxNetName
// символов без управляющих. Пробелы по краям срезаются до проверки.
func validNetName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if len([]rune(name)) > maxNetName {
		return "", errors.New("имя сети длиннее 64 символов")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("недопустимый символ в имени сети")
		}
	}
	return name, nil
}

// renameNet задаёт сети имя; пустое — убрать.
func (s *server) renameNet(netID, name string) error {
	name, err := validNetName(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nets[netID]
	if n == nil {
		return errors.New("нет такой сети")
	}
	n.Name = name
	s.emitLocked(event{Type: "rename", Net: netID, Text: name})
	s.markDirty()
	return nil
}

// deleteNet удаляет сеть целиком: устройства, которые сейчас на связи,
// отключаются, состояние сети стирается, адреса освобождаются.
//
// Сеть берётся из ключа, который знают устройства, поэтому она может
// появиться снова: как только какое-нибудь устройство подключится с тем же
// ключом, создастся новая пустая сеть (без имени, с новой датой создания) и
// устройство получит в ней новый адрес. Чтобы сеть исчезла насовсем, ключ
// нужно убрать с устройств.
func (s *server) deleteNet(netID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nets[netID]
	if n == nil {
		return errors.New("нет такой сети")
	}
	for _, d := range n.Devices {
		if d.ctrl != nil {
			d.ctrl.conn.Close()
			d.ctrl = nil
		}
	}
	// Вызовы, ждущие устройства этой сети, никому уже не нужны.
	for _, cl := range s.calls {
		if cl.net == netID {
			select {
			case cl.answer <- answer{err: "сеть удалена"}:
			default:
			}
		}
	}
	delete(s.nets, netID)
	s.emitLocked(event{Type: "net-delete", Net: netID, Text: n.Name})
	s.markDirty()
	return nil
}

// pruneLocked удаляет сети, в которых нет устройств уже emptyNetKeep. Сеть с
// устройствами не удаляется никогда. Под s.mu.
func (s *server) pruneLocked(now time.Time) {
	for id, n := range s.nets {
		if len(n.Devices) > 0 {
			n.EmptySince = 0
			continue
		}
		if n.EmptySince == 0 {
			n.EmptySince = now.Unix()
			continue
		}
		if now.Sub(time.Unix(n.EmptySince, 0)) >= emptyNetKeep {
			delete(s.nets, id)
			s.emitLocked(event{Type: "net-delete", Net: id, Text: "пустая сеть убрана автоматически"})
			s.markDirty()
		}
	}
}

func (s *server) prune() {
	s.mu.Lock()
	s.pruneLocked(s.now())
	s.mu.Unlock()
}

// pruneLoop раз в час убирает старые пустые сети.
func (s *server) pruneLoop() {
	for range time.Tick(time.Hour) {
		s.prune()
	}
}

// ---------- журнал событий ----------

// event — одна запись журнала. Net/Device/Peer — id; Text — подпись для
// человека. Журнал живёт только в памяти.
type event struct {
	Seq    int64  `json:"seq"`
	Time   int64  `json:"time"`
	Type   string `json:"type"`
	Net    string `json:"net,omitempty"`
	Device string `json:"device,omitempty"`
	Peer   string `json:"peer,omitempty"`
	Port   int    `json:"port,omitempty"`
	Text   string `json:"text,omitempty"`
}

// emitLocked дописывает событие в журнал. Под s.mu.
func (s *server) emitLocked(e event) {
	s.evSeq++
	e.Seq = s.evSeq
	e.Time = s.now().Unix()
	if len(s.events) >= maxEvents {
		copy(s.events, s.events[1:])
		s.events = s.events[:maxEvents-1]
	}
	s.events = append(s.events, e)
}

// eventsAfter — события с seq > after, самые новые последними, не больше limit
// (по умолчанию 200, максимум maxEvents). Если событий больше лимита —
// самые новые из них.
func (s *server) eventsAfter(after int64, limit int) (int64, []event) {
	if limit <= 0 {
		limit = 200
	}
	if limit > maxEvents {
		limit = maxEvents
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []event{}
	for _, e := range s.events {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return s.evSeq, out
}

// ---------- проверка доступа ----------

type checkResult struct {
	Allowed bool   `json:"allowed"`
	Rule    int    `json:"rule"` // индекс правила или -1
	Reason  string `json:"reason"`
	Visible bool   `json:"visible"`
}

// userOfLocked — пользователь устройства (или serversGroup для сервера) по
// id; ok=false — такого нет. Под s.mu.
func (s *server) userOfLocked(n *network, id string) (user string, isServer, ok bool) {
	if d := n.Devices[id]; d != nil {
		return d.User, false, true
	}
	if id == s.self.IP {
		return serversGroup, true, true
	}
	if _, found := s.relayRecs[id]; found {
		return serversGroup, true, true
	}
	return "", false, false
}

// check отвечает, может ли устройство from открыть соединение к to на порт.
// Решает тот же policy, что и dial.
func (s *server) check(netID, from, to string, port int) (checkResult, error) {
	if port < 0 || port > 65535 {
		return checkResult{}, errors.New("неверный порт")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nets[netID]
	if n == nil {
		return checkResult{}, errors.New("нет такой сети")
	}
	fu, fromServer, ok1 := s.userOfLocked(n, from)
	tu, _, ok2 := s.userOfLocked(n, to)
	if !ok1 || !ok2 {
		return checkResult{Rule: -1, Reason: "unknown device"}, nil
	}
	res := checkResult{Rule: -1, Visible: s.policy.visible(fu, tu)}
	switch {
	case fromServer:
		res.Reason = "server cannot initiate"
	case !s.policy.Enabled:
		res.Allowed, res.Reason = true, "policy off"
	default:
		res.Rule = s.policy.match(fu, tu, port)
		if res.Rule >= 0 {
			res.Allowed, res.Reason = true, "rule"
		} else {
			res.Reason = "no rule"
		}
	}
	return res, nil
}

func (s *server) adminHandler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(v)
	}
	type req struct {
		Net    string `json:"net"`
		Device string `json:"device"`
		Alias  string `json:"alias"`
		// Для /v1/self.
		Name    string `json:"name"`
		Enabled *bool  `json:"enabled"`
		Ports   string `json:"ports"`
		// Для /v1/check.
		From string `json:"from"`
		To   string `json:"to"`
		Port int    `json:"port"`
		// Для /v1/self (адрес сервера) и /v1/known.
		IP      string     `json:"ip"`
		Servers []serverAd `json:"servers"`
	}
	decode := func(r *http.Request) (req, error) {
		var q req
		if r.Method != http.MethodPost {
			return q, errors.New("нужен POST")
		}
		err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&q)
		return q, err
	}
	mux.HandleFunc("/v1/state", func(w http.ResponseWriter, r *http.Request) {
		reply(w, s.adminState(), nil)
	})
	mux.HandleFunc("/v1/rename", func(w http.ResponseWriter, r *http.Request) {
		q, err := decode(r)
		if err == nil {
			err = s.rename(q.Net, q.Device, q.Alias)
		}
		reply(w, map[string]bool{"ok": true}, err)
	})
	mux.HandleFunc("/v1/self", func(w http.ResponseWriter, r *http.Request) {
		q, err := decode(r)
		if err == nil {
			err = s.setSelf(q.Name, q.Enabled, q.Ports)
		}
		if err == nil && q.IP != "" {
			err = s.setSelfIP(q.IP)
		}
		reply(w, map[string]bool{"ok": true}, err)
	})
	mux.HandleFunc("/v1/known", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s.mu.Lock()
			k := append([]serverAd{}, s.self.Known...)
			s.mu.Unlock()
			reply(w, map[string]any{"servers": k}, nil)
			return
		}
		q, err := decode(r)
		if err == nil {
			err = s.setKnown(q.Servers)
		}
		reply(w, map[string]bool{"ok": true}, err)
	})
	mux.HandleFunc("/v1/forget-server", func(w http.ResponseWriter, r *http.Request) {
		q, err := decode(r)
		if err == nil {
			s.forgetRelay(q.Device)
		}
		reply(w, map[string]bool{"ok": true}, err)
	})
	mux.HandleFunc("/v1/policy", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s.mu.Lock()
			p := s.policy
			s.mu.Unlock()
			reply(w, p, nil)
			return
		}
		if r.Method != http.MethodPost {
			reply(w, nil, errors.New("нужен POST"))
			return
		}
		var p policy
		err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p)
		if err == nil {
			err = s.setPolicy(p)
		}
		reply(w, map[string]bool{"ok": true}, err)
	})
	mux.HandleFunc("/v1/net/rename", func(w http.ResponseWriter, r *http.Request) {
		q, err := decode(r)
		if err == nil {
			err = s.renameNet(q.Net, q.Name)
		}
		reply(w, map[string]bool{"ok": true}, err)
	})
	mux.HandleFunc("/v1/net/delete", func(w http.ResponseWriter, r *http.Request) {
		q, err := decode(r)
		if err == nil {
			err = s.deleteNet(q.Net)
		}
		reply(w, map[string]bool{"ok": true}, err)
	})
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		seq, evs := s.eventsAfter(after, limit)
		reply(w, map[string]any{"seq": seq, "events": evs}, nil)
	})
	mux.HandleFunc("/v1/check", func(w http.ResponseWriter, r *http.Request) {
		q, err := decode(r)
		var res checkResult
		if err == nil {
			res, err = s.check(q.Net, q.From, q.To, q.Port)
		}
		reply(w, res, err)
	})
	mux.HandleFunc("/v1/forget", func(w http.ResponseWriter, r *http.Request) {
		q, err := decode(r)
		if err == nil {
			err = s.forget(q.Net, q.Device)
		}
		reply(w, map[string]bool{"ok": true}, err)
	})
	return mux
}

// serveAdmin слушает unix-сокет для панели. Права 0600: достучаться может
// только владелец службы и root — клиентам туннеля (другие пользователи
// системы) он недоступен даже через проброс unix-сокетов в SSH.
func (s *server) serveAdmin(path string) error {
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	log.Printf("вход для панели: %s", path)
	go (&http.Server{Handler: s.adminHandler(), ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
	return nil
}

// ---------- сами серверы как узлы сети ----------
//
// У каждого сервера с meshd в сети есть свой адрес (s.self.IP) и имя — как у
// устройства. Соединение на него meshd ведёт на 127.0.0.1:ПОРТ самого
// сервера: так с телефона открывается, например, панель или контейнер на
// VPS, хотя наружу его порт закрыт. У старых побочных серверов (проброс к
// meshd, без своего) адреса ниже legacySelfIP;
// соединение на такой адрес meshd передаёт побочному по его управляющему
// соединению, и тот подключается к своему 127.0.0.1:ПОРТ.

const (
	// legacySelfIP — адрес, который был у единственного «главного» сервера.
	// Сервер, обновлённый с тех времён, его сохраняет; новый получает свой
	// (см. ensureSelfLocked), иначе два сервера сети заняли бы один адрес.
	legacySelfIP = "198.19.255.254"
	// relayNet — «сеть» в ответе побочного сервера на вызов: ключа сети у
	// него нет, вызов узнаётся по id.
	relayNet = "@relay"
)

// serverBlock — адреса серверов, устройствам не выдаются.
var serverBlock = netip.MustParsePrefix("198.19.255.0/24")

// selfConfig — этот сервер как узел сети; настраивается из панели.
type selfConfig struct {
	Name    string `json:"name,omitempty"`
	Enabled bool   `json:"enabled"`
	// Ports — какие порты сервера открыты устройствам сети: "22,80,8000-8100";
	// пусто — все.
	Ports string `json:"ports,omitempty"`
	// ID — постоянный id сервера (придумывается при первом запуске): по
	// нему устройство понимает, что два его соединения ведут в один meshd.
	// IP — адрес сервера в сети устройств (см. ensureSelfLocked). Known —
	// другие серверы сети, которые советует панель (serverAd).
	ID    string     `json:"id,omitempty"`
	IP    string     `json:"ip,omitempty"`
	Known []serverAd `json:"known,omitempty"`
}

type relayRecord struct {
	Name     string    `json:"name"`
	IP       string    `json:"ip"`
	LastSeen time.Time `json:"lastSeen"`
}

type serversFile struct {
	Self   selfConfig              `json:"self"`
	Relays map[string]*relayRecord `json:"relays"`
}

func (s *server) serversPath() string {
	return filepath.Join(filepath.Dir(s.path), "servers.json")
}

func (s *server) loadServers() {
	data, err := os.ReadFile(s.serversPath())
	if err != nil {
		return
	}
	var f serversFile
	if json.Unmarshal(data, &f) != nil {
		return
	}
	s.self = f.Self
	s.hadState = true
	for id, r := range f.Relays {
		if r != nil && serverBlock.Contains(netip.MustParseAddr(safeAddr(r.IP))) {
			s.relayRecs[id] = r
		}
	}
}

// safeAddr — адрес из файла, испорченный — нулевой (не попадёт в блок).
func safeAddr(ip string) string {
	if _, err := netip.ParseAddr(ip); err != nil {
		return "0.0.0.0"
	}
	return ip
}

func (s *server) selfNameLocked() string {
	if s.self.Name != "" {
		return s.self.Name
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "server"
}

// serverLabelsLocked — имена серверов: устройствам они не достаются.
func (s *server) serverLabelsLocked() map[string]bool {
	m := map[string]bool{}
	if s.self.Enabled {
		m[hostLabel(s.selfNameLocked())] = true
	}
	for _, r := range s.relayRecs {
		m[hostLabel(r.Name)] = true
	}
	return m
}

// serverPeersLocked — серверы в списке устройств сети n. Имя совпало с
// давно заведённым устройством — серверу достаётся суффикс.
func (s *server) serverPeersLocked(n *network) []peer {
	used := map[string]bool{}
	for _, d := range n.Devices {
		used[d.Host] = true
	}
	var out []peer
	add := func(id, name, ip string, online bool, lastSeen time.Time) {
		base := hostLabel(name)
		if base == "" {
			base = "server"
		}
		h := base
		for i := 2; used[h]; i++ {
			h = base + "-" + itoa(i)
		}
		used[h] = true
		p := peer{Name: name, Host: h, IP: ip, Online: online, Platform: "server"}
		if id != "" {
			p.ID = peerID("server\x00" + id)
		}
		if !lastSeen.IsZero() {
			p.LastSeen = lastSeen.Unix()
		}
		out = append(out, p)
	}
	if s.self.Enabled {
		add(s.self.ID, s.selfNameLocked(), s.self.IP, true, time.Time{})
	}
	ids := make([]string, 0, len(s.relayRecs))
	for id := range s.relayRecs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return s.relayRecs[ids[i]].IP > s.relayRecs[ids[j]].IP })
	for _, id := range ids {
		r := s.relayRecs[id]
		_, online := s.relays[id]
		add(id, r.Name, r.IP, online, r.LastSeen)
	}
	return out
}

// serverTargetLocked — адрес сервера, если to — сервер этой сети.
func (s *server) serverTargetLocked(n *network, to string) (string, bool) {
	for _, p := range s.serverPeersLocked(n) {
		if p.IP == to || p.Host == to {
			return p.IP, true
		}
	}
	return "", false
}

func (s *server) allocRelayIPLocked() string {
	used := map[string]bool{s.self.IP: true, legacySelfIP: true}
	for _, r := range s.relayRecs {
		used[r.IP] = true
	}
	a := netip.MustParseAddr(legacySelfIP).Prev()
	for serverBlock.Contains(a) && a.As4()[3] != 0 {
		if !used[a.String()] {
			return a.String()
		}
		a = a.Prev()
	}
	return ""
}

func (s *server) broadcastAllLocked() {
	for _, n := range s.nets {
		s.broadcastLocked(n)
	}
}

// portAllowed — открыт ли порт по списку вида "22,80,8000-8100" (пусто — все).
func portAllowed(spec string, port int) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return true
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err1 := atoiStrict(strings.TrimSpace(lo))
		b := a
		var err2 error
		if isRange {
			b, err2 = atoiStrict(strings.TrimSpace(hi))
		}
		if err1 == nil && err2 == nil && port >= a && port <= b {
			return true
		}
	}
	return false
}

func atoiStrict(v string) (int, error) {
	if v == "" || len(v) > 5 {
		return 0, errors.New("не число")
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, errors.New("не число")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// validPorts — список портов из панели: пустой или разбирается целиком.
func validPorts(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	for _, part := range strings.Split(spec, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := atoiStrict(strings.TrimSpace(lo))
		if err != nil || a < 1 || a > 65535 {
			return errors.New("неверный список портов: " + part)
		}
		if isRange {
			b, err := atoiStrict(strings.TrimSpace(hi))
			if err != nil || b < a || b > 65535 {
				return errors.New("неверный список портов: " + part)
			}
		}
	}
	return nil
}

func (s *server) setSelf(name string, enabled *bool, ports string) error {
	if err := validPorts(ports); err != nil {
		return err
	}
	s.mu.Lock()
	s.self.Name = clip(name)
	if enabled != nil {
		s.self.Enabled = *enabled
	}
	s.self.Ports = strings.TrimSpace(ports)
	s.broadcastAllLocked()
	s.mu.Unlock()
	s.markDirty()
	return nil
}

func (s *server) forgetRelay(id string) {
	s.mu.Lock()
	if _, online := s.relays[id]; !online {
		delete(s.relayRecs, id)
		s.broadcastAllLocked()
	}
	s.mu.Unlock()
	s.markDirty()
}

// dialServer — соединение устройства с самим сервером. Вызывается под s.mu
// и отпускает его.
func (s *server) dialServer(conn net.Conn, nid string, from *device, ip string, port int, fail func(string)) {
	if ip == s.self.IP {
		allowed := portAllowed(s.self.Ports, port)
		s.mu.Unlock()
		if !allowed || port == 47831 {
			fail("сервер не пускает устройства сети на порт " + itoa(port) + " — это настраивается в его панели")
			return
		}
		local, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), 5*time.Second)
		if err != nil {
			fail("на сервере на порту " + itoa(port) + " ничего не слушает")
			return
		}
		src := 0
		lc := &localCall{port: port, net: nid, dev: from}
		if la, ok := local.LocalAddr().(*net.TCPAddr); ok {
			src = la.Port
			s.mu.Lock()
			s.local[src] = lc
			s.mu.Unlock()
		}
		s.serveCall(conn, local, from)
		if src != 0 {
			s.mu.Lock()
			if s.local[src] == lc {
				delete(s.local, src)
			}
			s.mu.Unlock()
		}
		return
	}
	var r *relay
	for id, rec := range s.relayRecs {
		if rec.IP == ip {
			r = s.relays[id]
			break
		}
	}
	if r == nil {
		s.mu.Unlock()
		fail("сервер " + ip + " сейчас не в сети")
		return
	}
	if len(s.calls) >= maxPendingCalls {
		s.mu.Unlock()
		fail("сервер занят, попробуй ещё раз")
		return
	}
	callID := randomID()
	cl := &call{net: netID(relayNet), target: r.ID, answer: make(chan answer, 1)}
	s.calls[callID] = cl
	r.ctrl.send(msg{Op: "incoming", Call: callID, From: from.IP, FromHost: from.Host, FromID: peerID(from.ID), Port: port})
	s.mu.Unlock()

	peerConn, errText := s.waitCall(callID, cl)
	if peerConn == nil {
		fail(errText)
		return
	}
	s.serveCall(conn, peerConn, from)
}

// waitCall ждёт ответа на вызов; nil — отказ или тишина, с объяснением.
func (s *server) waitCall(callID string, cl *call) (net.Conn, string) {
	var got answer
	timedOut := false
	select {
	case got = <-cl.answer:
	case <-time.After(callTimeout):
		timedOut = true
	}
	s.mu.Lock()
	delete(s.calls, callID)
	s.mu.Unlock()
	if timedOut {
		select {
		case late := <-cl.answer:
			if late.conn != nil {
				late.conn.Close()
			}
		default:
		}
		return nil, "не ответил"
	}
	if got.conn == nil {
		if got.err == "" {
			got.err = "в соединении отказано"
		}
		return nil, got.err
	}
	return got.conn, ""
}

// serveCall сообщает вызывающему, что соединение есть, и перекачивает данные.
func (s *server) serveCall(conn, peerConn net.Conn, from *device) {
	if err := writeMsg(conn, msg{Op: "connected"}); err != nil {
		conn.Close()
		peerConn.Close()
		return
	}
	from.activeCalls.Add(1)
	defer from.activeCalls.Add(-1)
	var sink1, sink2 atomic.Int64
	splice(conn, peerConn, &from.up, &sink1, &sink2, &from.down)
	s.markDirty()
}

// ---------- несколько серверов без главного ----------
//
// В сети может быть сколько угодно серверов, и на каждом — свой meshd.
// Устройство держит управляющее соединение с каждым сервером, который
// знает, и само хранит свой адрес и имя. Серверы друг с другом не говорят:
// meshd только проверяет то, что устройство о себе сказало, и сводит
// устройства, которые сейчас на связи с ним. Два устройства видят друг
// друга, если у них есть хоть один общий живой сервер.
//
// Адрес и имя устройства. Устройство называет адрес (IP) и имя (Host),
// которые уже держит, и с какого времени держит адрес (Since). Свободно на
// этом сервере — принимается как есть. Занято другим устройством —
// побеждает тот, кто держит адрес дольше (меньший Since), при равенстве —
// меньший id устройства. Правило одно на всех серверах и зависит только от
// самих двух устройств, поэтому любой сервер, который знает обоих, решит
// одинаково, ни с кем не советуясь. Проигравший получает следующий
// свободный адрес из своей собственной последовательности (addrSeq: хеш
// сети, id устройства и номера) и новое Since; узнав его, устройство
// называет новый адрес и остальным серверам (op self). Новое устройство без
// адреса начинает с той же последовательности — поэтому на разных серверах
// оно, скорее всего, сразу получит один и тот же адрес. Имя — так же:
// проигравший получает имя с хвостом из его id («laptop-3f2a»).

// ensureSelfLocked придумывает серверу id и адрес, если их ещё нет.
// Сервер, обновлённый с тех времён, когда главный был один, оставляет себе
// прежний адрес legacySelfIP: его помнят устройства. Новый получает адрес
// из своего id — у двух серверов он совпадёт с вероятностью 1/200, тогда
// адрес меняется в панели (/v1/self, поле ip).
func (s *server) ensureSelfLocked() {
	changed := false
	if s.self.ID == "" {
		s.self.ID = randomID()
		changed = true
		if s.self.IP == "" && s.hadState {
			s.self.IP = legacySelfIP
		}
	}
	if s.self.IP == "" {
		h := sha256.Sum256([]byte("ssh_tunnel mesh server\x00" + s.self.ID))
		s.self.IP = "198.19.255." + itoa(1+int(binary.BigEndian.Uint16(h[:2]))%200)
		changed = true
	}
	if changed {
		s.markDirty()
	}
}

// infoLocked — этот сервер для welcome.
func (s *server) infoLocked() *serverInfo {
	return &serverInfo{ID: s.self.ID, Name: s.selfNameLocked(), Host: hostLabel(s.selfNameLocked()),
		IP: s.self.IP, Pol: s.policyFPLocked()}
}

// policyFPLocked — отпечаток правил доступа: одинаковые правила на двух
// серверах дают одинаковый отпечаток (encoding/json пишет ключи по порядку).
func (s *server) policyFPLocked() string {
	b, _ := json.Marshal(s.policy)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:6])
}

// addrSeq — i-й адрес в последовательности устройства dev сети nid.
func addrSeq(nid, dev string, i int) string {
	h := sha256.Sum256([]byte("ssh_tunnel mesh addr\x00" + nid + "\x00" + dev + "\x00" + itoa(i)))
	return "198.19." + itoa(int(h[0])) + "." + itoa(int(h[1]))
}

// deviceAddr — годится ли ip в адреса устройств.
func deviceAddr(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is4() || !meshRange.Contains(a) || serverBlock.Contains(a) {
		return false
	}
	b := a.As4()
	return b[3] != 0 && b[3] != 255
}

// holderLocked — другое (не d) устройство сети с адресом ip.
func holderLocked(n *network, ip string, d *device) *device {
	for _, o := range n.Devices {
		if o != d && o.IP == ip {
			return o
		}
	}
	return nil
}

// freeIPLocked — первый свободный адрес из последовательности устройства.
func freeIPLocked(nid string, n *network, d *device) string {
	for i := 0; i < 4096; i++ {
		if ip := addrSeq(nid, d.ID, i); deviceAddr(ip) && holderLocked(n, ip, d) == nil {
			return ip
		}
	}
	ip, _ := allocIPLocked(n)
	return ip
}

// sinceOf — с какого времени устройство держит адрес. В старых файлах
// состояния Since нет — тогда с первого появления.
func sinceOf(d *device) int64 {
	if d.Since > 0 {
		return d.Since
	}
	if u := unixOrZero(d.FirstSeen); u > 0 {
		return u
	}
	return math.MaxInt64
}

// outranks — устройство a (держит адрес с aSince) сильнее b в споре.
func outranks(aSince int64, aID string, bSince int64, bID string) bool {
	if aSince != bSince {
		return aSince < bSince
	}
	return aID < bID
}

// claimLocked решает адрес и имя устройства d по тому, что оно назвало
// само (ip, host, since; у старого клиента всё пустое). Под s.mu.
func (s *server) claimLocked(nid string, n *network, d *device, ip, host string, since int64) {
	now := s.now().Unix()
	if since <= 0 || since > now {
		since = now
	}
	switch {
	case deviceAddr(ip):
		holder := holderLocked(n, ip, d)
		switch {
		case holder == nil:
			d.IP, d.Since = ip, since
		case outranks(since, d.ID, sinceOf(holder), holder.ID):
			d.IP, d.Since = ip, since
			s.readdressLocked(nid, n, holder)
		default:
			// Проиграл: прежний адрес на этом сервере, если он свободен,
			// иначе следующий из своей последовательности.
			if d.IP == "" || d.IP == ip || holderLocked(n, d.IP, d) != nil {
				d.IP = freeIPLocked(nid, n, d)
			}
			d.Since = now
			s.emitLocked(event{Type: "readdress", Net: nid, Device: d.ID, Peer: holder.ID, Text: d.IP})
		}
	case d.IP == "" || holderLocked(n, d.IP, d) != nil:
		d.IP, d.Since = freeIPLocked(nid, n, d), now
	}
	if d.Since == 0 {
		d.Since = sinceOf(d)
		if d.Since == math.MaxInt64 {
			d.Since = now
		}
	}
	s.hostLocked(nid, n, d, host)
}

// hostLocked выбирает имя устройства: названное им самим (если оно от его
// же имени), иначе от имени; занято более сильным — с хвостом из id.
func (s *server) hostLocked(nid string, n *network, d *device, claim string) {
	base := hostLabel(d.displayName())
	if base == "" {
		base = "device"
	}
	want := base
	if claim != "" && hostLabel(claim) == claim && (claim == base || strings.HasPrefix(claim, base+"-")) {
		want = claim
	}
	servers := s.serverLabelsLocked()
	holderOf := func(h string) *device {
		for _, o := range n.Devices {
			if o != d && o.Host == h {
				return o
			}
		}
		return nil
	}
	h := sha256.Sum256([]byte(d.ID))
	tail := hex.EncodeToString(h[:4])
	for _, c := range []string{want, base, base + "-" + tail[:4], base + "-" + tail} {
		if servers[c] {
			continue
		}
		o := holderOf(c)
		if o == nil {
			d.Host = c
			return
		}
		if outranks(sinceOf(d), d.ID, sinceOf(o), o.ID) {
			d.Host = c
			o.Host = ""
			s.hostLocked(nid, n, o, "")
			if o.ctrl != nil && o.claims {
				o.ctrl.send(msg{Op: "readdress", IP: o.IP, Host: o.Host, Since: o.Since})
			}
			return
		}
	}
	d.Host = s.uniqueHostLocked(n, d.ID, d.displayName())
}

// readdressLocked — устройство o проиграло свой адрес: новый из его
// последовательности. Новому клиенту хватает сообщения, старый
// переподключится и получит адрес в welcome.
func (s *server) readdressLocked(nid string, n *network, o *device) {
	old := o.IP
	o.IP, o.Since = freeIPLocked(nid, n, o), s.now().Unix()
	s.emitLocked(event{Type: "readdress", Net: nid, Device: o.ID, Text: old + " → " + o.IP})
	if o.ctrl == nil {
		return
	}
	if o.claims {
		o.ctrl.send(msg{Op: "readdress", IP: o.IP, Host: o.Host, Since: o.Since})
	} else {
		o.ctrl.conn.Close()
	}
}

// aliasLocked — имя из панели, которое принесло устройство: побеждает более
// позднее (так переименование в панели одного сервера расходится по
// остальным через само устройство).
func (s *server) aliasLocked(d *device, alias string, t int64) {
	if t <= d.AliasT || t > s.now().Unix()+60 {
		return
	}
	d.Alias, d.AliasT = clip(alias), t
}

// selfMessage — устройство обновило о себе адрес и имя (op self) или
// список серверов, с которыми оно на связи (op on).
func (s *server) selfMessage(nid string, n *network, d *device, c *ctrlConn, in msg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.ctrl != c {
		return
	}
	if in.Op == "on" {
		var on []serverSeen
		for _, o := range in.On {
			if o.ID = clip(o.ID); o.ID == "" || o.ID == s.self.ID || len(on) >= 16 {
				continue
			}
			on = append(on, serverSeen{ID: o.ID, Name: clip(o.Name), Pol: clip(o.Pol)})
		}
		was, _ := json.Marshal(d.On)
		now, _ := json.Marshal(on)
		d.On = on
		if string(was) != string(now) {
			// Списки устройств несут «on» — разослать, только если он другой.
			s.broadcastLocked(n)
			s.markDirty()
		}
		return
	}
	d.claims = true
	s.aliasLocked(d, in.Alias, in.AliasT)
	s.claimLocked(nid, n, d, in.IP, in.Host, in.Since)
	c.send(msg{Op: "self", IP: d.IP, Host: d.Host, Since: d.Since, Alias: d.Alias, AliasT: d.AliasT})
	s.broadcastLocked(n)
	s.markDirty()
}

// otherServersLocked — имена серверов из отчёта устройства, кроме этого.
func (s *server) otherServersLocked(on []serverSeen) string {
	var names []string
	for _, o := range on {
		if o.ID == s.self.ID {
			continue
		}
		name := o.Name
		if name == "" {
			name = o.ID
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// seenLocked — другие серверы сети по рассказам устройств, которые сейчас
// на связи с этим.
func (s *server) seenLocked() []adminSeen {
	own := s.policyFPLocked()
	by := map[string]*adminSeen{}
	for _, n := range s.nets {
		for _, d := range n.Devices {
			if d.ctrl == nil {
				continue
			}
			for _, o := range d.On {
				a := by[o.ID]
				if a == nil {
					a = &adminSeen{ID: o.ID}
					by[o.ID] = a
				}
				if o.Name != "" {
					a.Name = o.Name
				}
				if o.Pol != "" {
					a.Pol = o.Pol
				}
				a.Devices++
				a.LastSeen = s.now().Unix()
			}
		}
	}
	out := []adminSeen{}
	for _, a := range by {
		a.Same = a.Pol == "" || a.Pol == own
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// setKnown — какие серверы сети советовать устройствам. Только подсказка:
// устройство пойдёт на такой сервер, лишь если у него есть свой вход туда
// (профиль с ключом и проверенным ключом сервера).
func (s *server) setKnown(list []serverAd) error {
	if len(list) > 16 {
		return errors.New("слишком много серверов")
	}
	out := []serverAd{}
	for _, a := range list {
		a.Host, a.Name = clip(a.Host), clip(a.Name)
		if a.Host == "" || strings.ContainsAny(a.Host, " /@") {
			return errors.New("неверный адрес сервера: " + a.Host)
		}
		if a.Port < 0 || a.Port > 65535 {
			return errors.New("неверный порт")
		}
		out = append(out, a)
	}
	s.mu.Lock()
	s.self.Known = out
	s.mu.Unlock()
	s.markDirty()
	return nil
}

// setSelfIP — адрес сервера в сети устройств (если два сервера сети
// получили один и тот же).
func (s *server) setSelfIP(ip string) error {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is4() || !serverBlock.Contains(a) || a.As4()[3] == 0 || a.As4()[3] == 255 {
		return errors.New("адрес сервера должен быть из 198.19.255.1–254")
	}
	s.mu.Lock()
	s.self.IP = a.String()
	s.broadcastAllLocked()
	s.mu.Unlock()
	s.markDirty()
	return nil
}

// ---------- прямые соединения (p2p) ----------
//
// Устройства пробуют соединяться друг с другом напрямую, минуя сервер. Сам
// meshd в этом только посредник: пересылает предложение («вот мои адреса и
// отпечаток ключа») второму устройству той же сети и ответ обратно. Трафик
// прямого соединения сюда не идёт.

// directLink — прямое соединение устройства с другим (по его отчёту).
type directLink struct {
	IP    string  `json:"ip"`
	RTTMs float64 `json:"rttMs,omitempty"`
}

const p2pPerMinute = 30

func (s *server) p2pMessage(n *network, d *device, c *ctrlConn, in msg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.ctrl != c {
		return
	}
	if in.Op == "p2pstat" {
		var out []directLink
		for _, l := range in.Direct {
			if _, err := netip.ParseAddr(l.IP); err == nil && len(out) < 64 {
				out = append(out, directLink{IP: l.IP, RTTMs: round1(l.RTTMs)})
			}
		}
		d.direct = out
		return
	}
	now := time.Now().Unix() / 60
	if d.p2pMinute != now {
		d.p2pMinute, d.p2pBudget = now, p2pPerMinute
	}
	if d.p2pBudget <= 0 {
		return
	}
	d.p2pBudget--
	var target *device
	for _, t := range n.Devices {
		if t.IP == in.To {
			target = t
			break
		}
	}
	if target == nil || target.ctrl == nil || target == d || len(in.Call) > 64 || len(in.FP) > 128 {
		return
	}
	// По прямому соединению устройства ходят друг к другу на любые порты и в
	// обе стороны, и сервер этого уже не видит. Поэтому сводим их напрямую,
	// только если правила и так разрешают всё в обе стороны; иначе
	// соединения идут через сервер, где правила проверяются.
	if !s.policy.allowed(d.User, target.User, 0) || !s.policy.allowed(target.User, d.User, 0) {
		return
	}
	var cands []string
	for _, a := range in.Cands {
		if ap, err := netip.ParseAddrPort(a); err == nil && len(cands) < 16 {
			cands = append(cands, ap.String())
		}
	}
	target.ctrl.send(msg{Op: "p2p", From: d.IP, FromHost: d.Host, Call: in.Call, Cands: cands, FP: in.FP, OK: in.OK})
}

// ---------- ping между устройствами ----------
//
// ping к адресу сети устройств: устройство шлёт сюда echo, meshd пересылает
// его адресату, тот отвечает echoreply, и meshd несёт ответ обратно. За
// серверы сети (198.19.255.x) отвечает сам meshd, если сервер на связи.

const echoPerMinute = 240

func (s *server) echoMessage(n *network, d *device, c *ctrlConn, in msg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.ctrl != c || len(in.Call) > 64 {
		return
	}
	now := time.Now().Unix() / 60
	if d.echoMinute != now {
		d.echoMinute, d.echoBudget = now, echoPerMinute
	}
	if d.echoBudget <= 0 {
		return
	}
	d.echoBudget--
	if in.Op == "echo" {
		for _, p := range s.serverPeersLocked(n) {
			if p.IP == in.To {
				if p.Online && s.policy.visible(d.User, serversGroup) {
					c.send(msg{Op: "echoreply", From: in.To, Call: in.Call})
				}
				return
			}
		}
	}
	for _, t := range n.Devices {
		if t.IP == in.To && t != d && t.ctrl != nil && s.policy.visible(d.User, t.User) {
			t.ctrl.send(msg{Op: in.Op, From: d.IP, Call: in.Call})
			return
		}
	}
}

// ---------- проверка NAT (STUN) ----------
//
// Чтобы понять, смогут ли два устройства когда-нибудь соединиться напрямую,
// каждое проверяет свой NAT: шлёт сюда запрос на один порт, потом на второй,
// и сравнивает, с какого внешнего адреса пришло. Формат — стандартный STUN
// (RFC 5389/5780): тот же, что у видеозвонков, ничем не выделяется.
//
// Сервер только отвечает «ты пришёл с такого адреса» — с того же порта или,
// по просьбе, с другого (проверка фильтрации). Ответ не намного больше
// запроса, а частота ограничена по адресу: для усиления атак он бесполезен.

const (
	stunMagic       = 0x2112A442
	stunBindReq     = 0x0001
	stunBindResp    = 0x0101
	stunAttrMapped  = 0x0020 // XOR-MAPPED-ADDRESS
	stunAttrChange  = 0x0003 // CHANGE-REQUEST
	stunChangePort  = 0x02
	stunMinRequest  = 28 // заголовок + CHANGE-REQUEST: без него не отвечаем
	stunPerIPPerSec = 30
)

func (s *server) serveSTUN(spec string) error {
	var conns []net.PacketConn
	var ports []int
	for _, p := range strings.Split(spec, ",") {
		port, err := atoiStrict(strings.TrimSpace(p))
		if err != nil || port < 0 || port > 65535 {
			return errors.New("неверный порт проверки NAT: " + p)
		}
		// 0 — любой свободный (для тестов).
		pc, err := net.ListenPacket("udp", ":"+itoa(port))
		if err != nil {
			for _, c := range conns {
				c.Close()
			}
			return err
		}
		conns = append(conns, pc)
		ports = append(ports, pc.LocalAddr().(*net.UDPAddr).Port)
	}
	s.mu.Lock()
	s.stunPorts = ports
	s.mu.Unlock()
	lim := &stunLimiter{seen: map[string]int{}}
	for i, pc := range conns {
		other := conns[(i+1)%len(conns)]
		go stunLoop(pc, other, lim)
	}
	names := make([]string, len(ports))
	for i, p := range ports {
		names[i] = itoa(p)
	}
	log.Printf("проверка NAT: UDP %s", strings.Join(names, ","))
	return nil
}

type stunLimiter struct {
	mu     sync.Mutex
	second int64
	seen   map[string]int
}

func (l *stunLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().Unix()
	if now != l.second {
		l.second = now
		clear(l.seen)
	}
	l.seen[ip]++
	return l.seen[ip] <= stunPerIPPerSec
}

func stunLoop(pc, other net.PacketConn, lim *stunLimiter) {
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		ua, ok := from.(*net.UDPAddr)
		if !ok || !lim.allow(ua.IP.String()) {
			continue
		}
		resp, changePort, ok := stunAnswer(buf[:n], ua)
		if !ok {
			continue
		}
		out := pc
		if changePort {
			out = other
		}
		out.WriteTo(resp, from)
	}
}

// stunAnswer разбирает запрос и собирает ответ с адресом отправителя.
func stunAnswer(req []byte, from *net.UDPAddr) ([]byte, bool, bool) {
	if len(req) < stunMinRequest || len(req) > 512 {
		return nil, false, false
	}
	if binary.BigEndian.Uint16(req[0:2]) != stunBindReq || binary.BigEndian.Uint32(req[4:8]) != stunMagic {
		return nil, false, false
	}
	if int(binary.BigEndian.Uint16(req[2:4]))+20 != len(req) {
		return nil, false, false
	}
	changePort := false
	for a := req[20:]; len(a) >= 4; {
		typ, l := binary.BigEndian.Uint16(a[0:2]), int(binary.BigEndian.Uint16(a[2:4]))
		padded := (l + 3) &^ 3
		if 4+padded > len(a) {
			return nil, false, false
		}
		if typ == stunAttrChange && l == 4 {
			changePort = a[7]&stunChangePort != 0
		}
		a = a[4+padded:]
	}
	ip := from.IP.To4()
	family := byte(1)
	if ip == nil {
		ip, family = from.IP.To16(), 2
	}
	attr := make([]byte, 4+4+len(ip))
	binary.BigEndian.PutUint16(attr[0:2], stunAttrMapped)
	binary.BigEndian.PutUint16(attr[2:4], uint16(4+len(ip)))
	attr[5] = family
	binary.BigEndian.PutUint16(attr[6:8], uint16(from.Port)^uint16(stunMagic>>16))
	key := append([]byte{}, req[4:20]...) // cookie + id транзакции
	for i := range ip {
		attr[8+i] = ip[i] ^ key[i]
	}
	resp := make([]byte, 20, 20+len(attr))
	binary.BigEndian.PutUint16(resp[0:2], stunBindResp)
	binary.BigEndian.PutUint16(resp[2:4], uint16(len(attr)))
	copy(resp[4:20], req[4:20])
	return append(resp, attr...), changePort, true
}

// natInfo — результат проверки NAT, который присылает устройство.
type natInfo struct {
	Tested        int64  `json:"tested"`
	UDP           bool   `json:"udp"`
	Mapping       string `json:"mapping,omitempty"`   // independent / dependent
	Filtering     string `json:"filtering,omitempty"` // open / restricted
	PublicIP      string `json:"publicIp,omitempty"`
	PortPreserved bool   `json:"portPreserved,omitempty"`
	PortDelta     int    `json:"portDelta,omitempty"`
	IPv6          bool   `json:"ipv6,omitempty"`
	Network       string `json:"network,omitempty"` // wifi / mobile / ethernet — если устройство знает
}

func (n natInfo) clean() natInfo {
	pick := func(v string, ok ...string) string {
		for _, o := range ok {
			if v == o {
				return v
			}
		}
		return ""
	}
	n.Mapping = pick(n.Mapping, "independent", "dependent")
	n.Filtering = pick(n.Filtering, "open", "restricted")
	n.Network = clip(n.Network)
	if _, err := netip.ParseAddr(n.PublicIP); err != nil {
		n.PublicIP = ""
	}
	if n.PortDelta < -1000 || n.PortDelta > 1000 {
		n.PortDelta = 0
	}
	n.Tested = time.Now().Unix()
	return n
}

func randomID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---------- права доступа ----------
//
// Как группы и политики в NetBird. Панель кладёт каждого SSH-пользователя
// сервера (клиента туннеля) в группы и задаёт правила «группа → группа,
// порты». Устройство получает права своего пользователя: его не подделать,
// в отличие от id устройства, который оно называет само.
//
// Особые группы: "*" — все устройства и серверы сети, "@servers" — серверы.
// Пользователь, которого нет в политике (в том числе устройства, пришедшие
// через побочный сервер под meshlink), состоит только в "*".
//
// Пока политика выключена, разрешено всё — как было до прав доступа.

const (
	allGroup     = "*"
	serversGroup = "@servers" // и группа серверов, и «пользователь» сервера
	deniedText   = "нет доступа: правила сети устройств не разрешают это соединение"
	maxRules     = 256
	maxGroups    = 64
)

type policyRule struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Ports string `json:"ports,omitempty"` // "22,80,8000-8100"; пусто — все
}

type policy struct {
	Enabled bool                `json:"enabled"`
	Users   map[string][]string `json:"users,omitempty"` // пользователь → группы
	Rules   []policyRule        `json:"rules,omitempty"`
}

func (p policy) member(user, group string) bool {
	if group == allGroup {
		return true
	}
	if user == serversGroup {
		return group == serversGroup
	}
	for _, g := range p.Users[user] {
		if g == group {
			return true
		}
	}
	return false
}

// allowed — может ли устройство пользователя from открыть соединение к
// устройству пользователя to на порт port. port 0 — «на любой порт»:
// подходит только правило без ограничения портов.
func (p policy) allowed(from, to string, port int) bool {
	if !p.Enabled {
		return true
	}
	return p.match(from, to, port) >= 0
}

// match — индекс первого правила, которое разрешает from → to на port, или
// -1. Не смотрит на Enabled: «выключено» решает allowed.
func (p policy) match(from, to string, port int) int {
	if from == serversGroup {
		return -1
	}
	for i, r := range p.Rules {
		if !p.member(from, r.From) || !p.member(to, r.To) {
			continue
		}
		if r.Ports == "" || (port > 0 && portAllowed(r.Ports, port)) {
			return i
		}
	}
	return -1
}

// visible — видят ли устройства друг друга в списке: да, если хоть в одну
// сторону разрешено хоть что-то.
func (p policy) visible(a, b string) bool {
	if !p.Enabled {
		return true
	}
	for _, r := range p.Rules {
		if (p.member(a, r.From) && p.member(b, r.To)) || (p.member(b, r.From) && p.member(a, r.To)) {
			return true
		}
	}
	return false
}

func validGroup(g string) error {
	if g == "" || len(g) > 64 {
		return errors.New("пустое или слишком длинное имя группы")
	}
	for _, r := range g {
		if unicode.IsControl(r) {
			return errors.New("недопустимый символ в имени группы")
		}
	}
	return nil
}

func (p policy) validate() error {
	if len(p.Rules) > maxRules {
		return errors.New("слишком много правил")
	}
	for _, r := range p.Rules {
		if err := validGroup(r.From); err != nil {
			return err
		}
		if err := validGroup(r.To); err != nil {
			return err
		}
		if r.From == serversGroup {
			return errors.New("серверы сами соединения не открывают — их можно указать только как цель")
		}
		if err := validPorts(r.Ports); err != nil {
			return err
		}
	}
	for u, gs := range p.Users {
		if u == "" || len(u) > 64 || len(gs) > maxGroups {
			return errors.New("неверный пользователь в политике")
		}
		for _, g := range gs {
			if err := validGroup(g); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *server) policyPath() string {
	if s.path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.path), "policy.json")
}

// loadPolicy читает политику при старте. Файл есть, но не читается —
// закрываем всё, а не открываем: испорченные права не должны означать
// «можно всё».
func (s *server) loadPolicy() {
	path := s.policyPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var p policy
	if err == nil {
		err = json.Unmarshal(data, &p)
	}
	if err == nil {
		err = p.validate()
	}
	if err != nil {
		log.Printf("права доступа: %s не читается (%v) — все соединения между устройствами закрыты", path, err)
		p = policy{Enabled: true}
	}
	s.policy = p
}

// setPolicy — новая политика от панели. Сохраняется на диск и сразу
// рассылается: каждое устройство получает свой список видимых. Уже идущие
// соединения не рвутся; прямые (p2p) живут, пока не оборвутся сами, —
// поэтому при сужении прав панель советует переподключить устройство.
func (s *server) setPolicy(p policy) error {
	if err := p.validate(); err != nil {
		return err
	}
	if path := s.policyPath(); path != "" {
		data, _ := json.MarshalIndent(p, "", "  ")
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.policy = p
	s.broadcastAllLocked()
	s.mu.Unlock()
	return nil
}

// ---------- кто звонит службе на сервере ----------
//
// Служба на самом сервере (плагины: ssh_tunnel_node) получает соединение
// устройства от meshd с 127.0.0.1:M на свой 127.0.0.1:N. Кто это, она
// спрашивает здесь же: {"op":"who","port":N,"src":M}. Подделать ответ
// нельзя:
//   - meshd отвечает только про соединения, которые открыл сам и которые
//     ещё живы (пара портов занята, пока соединение открыто);
//   - спрашивать может только тот пользователь системы, чей сокет слушает
//     127.0.0.1:N (uid из /proc/net/tcp), — чужой локальный пользователь не
//     узнает даже, кто ходит к чужой службе.
// Старый meshd на who отвечает «неизвестная операция» — служба тогда никого
// не пускает.

// localCall — соединение устройства со службой сервера.
type localCall struct {
	port int
	net  string
	dev  *device
}

// whoWait — сколько ждать, пока meshd запишет соединение: служба может
// спросить раньше, чем meshd дойдёт до записи после connect.
const whoWait = 2 * time.Second

func (s *server) who(conn net.Conn, m msg) {
	defer conn.Close()
	fail := func(text string) { writeMsg(conn, msg{Op: "error", Error: text}) }
	if m.Port <= 0 || m.Port > 65535 || m.Src <= 0 || m.Src > 65535 {
		fail("неверный порт")
		return
	}
	if !s.sameOwner(conn, m.Port) {
		fail("спрашивать может только служба, которая слушает этот порт")
		return
	}
	deadline := time.Now().Add(whoWait)
	for {
		s.mu.Lock()
		lc := s.local[m.Src]
		if lc != nil && lc.port == m.Port {
			d := lc.dev
			out := msg{Op: "whois", OK: true, Net: lc.net, Device: d.ID, IP: d.IP, Host: d.Host,
				Name: d.displayName(), User: d.User, Platform: d.Platform}
			s.mu.Unlock()
			writeMsg(conn, out)
			return
		}
		s.mu.Unlock()
		if time.Now().After(deadline) {
			fail("это соединение пришло не из сети устройств")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// procSameOwner — спрашивающий (сокет conn с той стороны) и служба,
// слушающая 127.0.0.1:port, — один пользователь системы. Только Linux:
// meshd в бою работает только там; на других системах (тесты на ПК) — да.
func procSameOwner(conn net.Conn, port int) bool {
	if runtime.GOOS != "linux" {
		return true
	}
	ra, ok := conn.RemoteAddr().(*net.TCPAddr)
	la, ok2 := conn.LocalAddr().(*net.TCPAddr)
	if !ok || !ok2 {
		return false
	}
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return false
	}
	asker, ok := socketUID(string(data), ra, la)
	if !ok {
		return false
	}
	owner, ok := listenUID(string(data), port)
	return ok && owner == asker
}

// listenUID — uid сокета, который слушает порт port на 127.0.0.1 или на
// всех адресах (состояние 0A в /proc/net/tcp).
func listenUID(table string, port int) (string, bool) {
	ph := strings.ToUpper(hex.EncodeToString([]byte{byte(port >> 8), byte(port)}))
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) > 7 && f[3] == "0A" && (f[1] == "0100007F:"+ph || f[1] == "00000000:"+ph) {
			return f[7], true
		}
	}
	return "", false
}

// procConnUser — SSH-пользователь, чей процесс sshd открыл это соединение.
// Устройства приходят к meshd через проброс SSH: соединение с 127.0.0.1
// открывает процесс sshd, работающий от имени пользователя туннеля, и ядро
// записывает этого пользователя владельцем сокета (колонка uid в
// /proc/net/tcp — она видна всем, root не нужен). Не нашли — "".
func procConnUser(conn net.Conn) string {
	ra, ok := conn.RemoteAddr().(*net.TCPAddr)
	la, ok2 := conn.LocalAddr().(*net.TCPAddr)
	if !ok || !ok2 {
		return ""
	}
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return ""
	}
	uid, ok := socketUID(string(data), ra, la)
	if !ok {
		return ""
	}
	u, err := user.LookupId(uid)
	if err != nil {
		return ""
	}
	return u.Username
}

// socketUID ищет в /proc/net/tcp сокет с локальным адресом local и
// удалённым remote (с той стороны соединения они наоборот) и возвращает его uid.
func socketUID(table string, local, remote *net.TCPAddr) (string, bool) {
	hexAddr := func(a *net.TCPAddr) string {
		ip := a.IP.To4()
		if ip == nil {
			return ""
		}
		return strings.ToUpper(hex.EncodeToString([]byte{ip[3], ip[2], ip[1], ip[0]})) + ":" +
			strings.ToUpper(hex.EncodeToString([]byte{byte(a.Port >> 8), byte(a.Port)}))
	}
	l, r := hexAddr(local), hexAddr(remote)
	if l == "" || r == "" {
		return "", false
	}
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) > 7 && f[1] == l && f[2] == r {
			return f[7], true
		}
	}
	return "", false
}

func main() {
	listen := flag.String("listen", "127.0.0.1:47831", "адрес, на котором слушать (только localhost)")
	state := flag.String("state", "/var/lib/meshd/state.json", "где хранить адреса устройств")
	admin := flag.String("admin", "", "unix-сокет для панели на сервере (пусто — выключен)")
	stun := flag.String("stun", "3478,3479", "UDP-порты проверки NAT для устройств (пусто — выключена)")
	showVersion := flag.Bool("version", false, "напечатать версию и выйти")
	flag.Parse()
	if *showVersion {
		os.Stdout.WriteString(version + "\n")
		return
	}

	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatalf("неверный адрес %q: %v", *listen, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		log.Fatalf("слушать можно только localhost, а не %q: снаружи сервис доступен быть не должен", host)
	}

	s := newServer(*state)
	go s.saveLoop()
	go s.pruneLoop()
	if *stun != "" {
		if err := s.serveSTUN(*stun); err != nil {
			log.Printf("проверка NAT не запустилась (%v) — сеть работает без неё", err)
		}
	}
	if *admin != "" {
		if err := s.serveAdmin(*admin); err != nil {
			log.Printf("вход для панели не открылся (%v) — сеть работает, но панель её не увидит", err)
		}
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("не могу занять %s: %v", *listen, err)
	}
	log.Printf("meshd слушает %s", ln.Addr())
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go s.handle(conn)
	}
}
