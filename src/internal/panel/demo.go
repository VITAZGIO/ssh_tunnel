package panel

// Режим показа (ssh_tunnel_panel -demo): настоящая страница панели
// (assets/index.html) и выдуманный сервер в памяти за ней. Нужен, чтобы
// смотреть и проверять интерфейс без VPS: несколько сетей (одна пустая),
// устройства в сети и нет, прямые связи, два сервера, права доступа и
// журнал, который сам пополняется.
//
// Ничего не трогает на машине: ни файлов, ни системных команд, ни сети —
// только память процесса. Поэтому здесь не вызываются Capacity (sshd -T),
// Autostart (systemctl), updater.Check (GitHub) и хранилища на диске:
// ответы для них собраны прямо тут.
//
// Входа нет: /api/session всегда отвечает «вошёл» и demo:true — по этому
// флагу страница рисует плашку «режим показа». Сам режим включается только
// явным флагом -demo и слушает только петлевой адрес (cmd/ssh_tunnel_panel).
//
// Для проверки краёв есть ручки /api/demo/* (страница их не вызывает):
// перезапуск meshd (журнал с seq=1), старая версия meshd, готовые наборы
// прав доступа.

import (
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/diag/probe"
	"sshtunnel/internal/meshsvc"
	"sshtunnel/internal/nodeapi"
	"sshtunnel/internal/share"
)

// demoTick — как часто в журнале появляется новое событие.
const demoTick = 6 * time.Second

// demoReturnAfter — через сколько удалённая сеть «возвращается»: так ведёт
// себя настоящий meshd, когда устройство с тем же ключом подключается снова.
const demoReturnAfter = 45 * time.Second

type demoBackend struct {
	mu      sync.Mutex
	version string
	started time.Time
	rnd     *rand.Rand

	clients  []Client
	access   AccessSettings
	nets     []*MeshNetwork
	servers  []MeshServer
	self     meshsvc.Self
	known    []meshsvc.KnownServer // серверы, которые панель советует устройствам (ТЗ 12)
	capacity int

	meshdStarted int64
	events       []meshsvc.Event
	seq          int64
	lastTick     time.Time
	step         int
	oldMeshd     bool

	// returning — удалённые сети, которые вернутся: id → когда и с чем.
	returning map[string]demoReturn

	// nodeInstalled/nodeConfig/nodeFolders — хост плагинов (ТЗ 9) в режиме
	// показа: только память, ничего реального.
	nodeInstalled bool
	nodeConfig    nodeapi.Config
	nodeFolders   map[string][]nodeapi.Folder
	nodeClipboard map[string]int
	// nodeBackups — резервные копии устройств по сетям (ТЗ 13); nodeOrphan —
	// данные сети, которую уже удалили в meshd («сеть удалена, данные
	// остались»).
	nodeBackups map[string][]nodeapi.BackupSet
	nodeOrphan  string
	// ТЗ 15: общий список блокировки, стёртая история машин, последняя
	// проверка сайта с сервера.
	nodeAd        nodeapi.AdblockState
	nodeMachClear map[string]bool
	nodeSiteAt    int64
	// egress — выход этого сервера через другой сервер (ТЗ 10): свой
	// маленький стейт-машин для показа, не настоящий EgressManager (ему
	// нужна система).
	egress demoEgress
}

// demoEgressWindowSec — окно подтверждения в показе: короче настоящих 60 с
// (egressWindowSec), чтобы откат было видно без долгого ожидания.
const demoEgressWindowSec = 20

type demoEgress struct {
	phase        string
	mode         string
	target       *EgressTarget
	appliedAt    time.Time
	token        string
	exitIP       string
	rolledBackAt int64
}

type demoReturn struct {
	at  time.Time
	dev MeshDevice
}

// NewDemoHandler — обработчик режима показа.
func NewDemoHandler(version string) http.Handler {
	d := &demoBackend{
		version:       version,
		started:       time.Now(),
		rnd:           rand.New(rand.NewSource(time.Now().UnixNano())),
		capacity:      DefaultMaxDevices,
		returning:     map[string]demoReturn{},
		nodeConfig:    nodeapi.Config{QuotaMB: nodeapi.DefaultQuotaMB},
		nodeFolders:   map[string][]nodeapi.Folder{},
		nodeClipboard: map[string]int{},
		nodeBackups:   map[string][]nodeapi.BackupSet{},
		nodeOrphan:    "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0",
		nodeMachClear: map[string]bool{},
		nodeAd: nodeapi.AdblockState{AdblockConf: nodeapi.AdblockConf{
			Sources: []string{"https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"},
			Allow:   []string{}, Deny: []string{}}},
	}
	d.seed()
	return d.routes()
}

// ---------- выдуманные данные ----------

func demoUser(n int) string { return fmt.Sprintf("tun_%016x", 0xa1b2c3d400000000+uint64(n)) }

func (d *demoBackend) seed() {
	d.egress.phase, d.egress.mode = EgressOff, EgressModeBlock

	now := time.Now()
	unix := now.Unix()
	day := int64(86400)

	type cl struct {
		n    int
		name string
		typ  DeviceType
		on   int
		gb   uint64
	}
	for _, c := range []cl{
		{1, "Рабочий ПК", DeviceWindows, 2, 41},
		{2, "Ноутбук", DeviceWindows, 1, 12},
		{3, "Телефон", DeviceAndroid, 1, 7},
		{4, "Домашний сервер", DeviceLinux, 1, 95},
		{5, "Виртуалка", DeviceWindows, 1, 3},
		{6, "Планшет гостя", DeviceAndroid, 0, 1},
		{7, "Дача: роутер", DeviceLinux, 1, 2},
		{8, "Старый ноутбук", DeviceWindows, 0, 0},
	} {
		u := demoUser(c.n)
		client := Client{
			ID: u, Username: u, Name: c.name, DeviceType: c.typ,
			CreatedAt: now.Add(-time.Duration(30+c.n*9) * 24 * time.Hour),
			State:     StateActive, Sessions: c.on,
			RxBytes: c.gb << 30, TxBytes: (c.gb << 30) / 5,
			PublicKey: "ssh-ed25519 AAAAdemo", PrivateKey: "демо-ключ: в режиме показа настоящих ключей нет",
		}
		if c.on > 0 {
			client.LastSeenAt = now
		} else if c.n == 6 {
			client.LastSeenAt = now.Add(-20 * time.Hour)
		}
		d.clients = append(d.clients, client)
	}

	dev := func(id, name, platform, ip string, user int, online bool, rtt float64, server string) MeshDevice {
		host := strings.ToLower(name)
		md := MeshDevice{Device: meshsvc.Device{
			ID: id, Name: name, Display: name, Host: host, IP: ip, Online: online,
			Platform: platform, OS: map[string]string{"windows": "Windows 11", "android": "Android 15", "linux": "Debian 13"}[platform],
			App: "1.9.0", Mode: "vpn", FirstSeen: unix - 40*day, Quality: "good",
			Sessions: 2, BytesUp: int64(user) * 700 << 20, BytesDown: int64(user) * 2100 << 20,
		}, Server: server}
		if user > 0 {
			md.User = demoUser(user)
		} else {
			// Пришло через побочный сервер под чужим пользователем: в
			// политике этой панели его нет, он состоит только в «*».
			md.User = "meshlink"
		}
		if online {
			md.ConnectedAt = unix - int64(user+1)*1900
			md.LastSeen = unix
			md.RTTMs, md.RTTAvgMs, md.JitterMs = rtt, rtt*1.1, rtt/8
			if rtt > 50 {
				md.Quality = "fair"
			} else if rtt < 10 {
				md.Quality = "excellent"
			}
		} else {
			md.LastSeen = unix - 20*3600
			md.Quality = "unknown"
		}
		md.NAT = &meshsvc.NAT{Tested: unix - 3600, UDP: true, Mapping: "endpoint-independent", Filtering: "address-dependent"}
		return md
	}

	home := &MeshNetwork{ID: "9f3c2a71d04e8b65", Name: "Дом", Created: unix - 120*day}
	pc := dev("e1a04f", "rabochiy-pk", "windows", "198.19.0.1", 1, true, 3.2, "main")
	wsl := dev("e1a050", "rabochiy-pk-wsl", "linux", "198.19.0.7", 1, true, 3.4, "main")
	nb := dev("7c22d9", "noutbuk", "windows", "198.19.0.2", 2, true, 5.1, "main")
	ph := dev("3b9e10", "telefon", "android", "198.19.0.3", 3, true, 61, "london")
	srv := dev("b07d33", "domashniy-server", "linux", "198.19.0.4", 4, true, 2.1, "main")
	vm := dev("55aa01", "vm", "windows", "198.19.0.5", 5, true, 48, "main")
	gst := dev("c4f0e2", "planshet-gostya", "android", "198.19.0.6", 6, false, 0, "london")
	frn := dev("0d0d77", "druga-noutbuk", "windows", "198.19.0.8", 0, true, 35, "london")
	pc.Direct = []meshsvc.DirectLink{{IP: nb.IP, RTTMs: 1.4}, {IP: srv.IP, RTTMs: 0.9}}
	nb.Direct = []meshsvc.DirectLink{{IP: pc.IP, RTTMs: 1.4}}
	srv.Direct = []meshsvc.DirectLink{{IP: pc.IP, RTTMs: 0.9}}
	wsl.Alias, wsl.Display = "", "rabochiy-pk-wsl"
	home.Devices = []MeshDevice{pc, wsl, nb, ph, srv, vm, gst, frn}

	dacha := &MeshNetwork{ID: "41be09c7aa13f2d0", Created: unix - 30*day}
	dacha.Devices = []MeshDevice{
		dev("a9e001", "dacha-router", "linux", "198.19.1.1", 7, true, 22, "main"),
		dev("a9e002", "dacha-kamera", "linux", "198.19.1.2", 7, false, 0, "main"),
	}
	test := &MeshNetwork{ID: "d2c8e4f6b1a39057", Name: "Проба", Created: unix - 12*day, EmptySince: unix - 2*day - 3600}
	d.nets = []*MeshNetwork{home, dacha, test}

	d.servers = []MeshServer{
		{ID: "main", Name: "Франкфурт", Host: "demo.example", Main: true, Online: true, Known: true,
			MeshIP: "198.18.0.1", MeshHost: "frankfurt"},
		{ID: "london", Name: "Лондон", Host: "london.demo.example", Online: true, Known: true,
			Added: now.Add(-60 * 24 * time.Hour), MeshIP: "198.18.0.2", MeshHost: "london",
			Link: &meshsvc.Server{ID: "london", Name: "Лондон", Host: "london.demo.example", ConnectedAt: unix - 5*3600,
				RTTMs: 31, RTTAvgMs: 30, Quality: "good"}},
	}
	d.self = meshsvc.Self{Name: "Франкфурт", Host: "frankfurt", IP: "198.18.0.1", Enabled: true}
	d.meshdStarted = unix - 3*day
	d.setPolicyPreset("simple")

	// Немного истории, чтобы журнал не начинался с пустоты.
	past := []meshsvc.Event{
		{Type: "server-up", Device: "london", Text: "Лондон"},
		{Type: "connect", Net: home.ID, Device: srv.ID},
		{Type: "connect", Net: home.ID, Device: pc.ID},
		{Type: "new-device", Net: home.ID, Device: frn.ID, Text: frn.Name},
		{Type: "connect", Net: home.ID, Device: frn.ID},
		{Type: "rename", Net: dacha.ID, Text: ""},
		{Type: "connect", Net: home.ID, Device: gst.ID},
		{Type: "denied", Net: home.ID, Device: gst.ID, Peer: pc.ID, Port: 47835},
		{Type: "disconnect", Net: home.ID, Device: gst.ID},
		{Type: "forget", Net: home.ID, Device: "ffee99"},
		{Type: "connect", Net: home.ID, Device: ph.ID},
		{Type: "connect", Net: dacha.ID, Device: "a9e001"},
	}
	for i, e := range past {
		e.Time = unix - int64(len(past)-i)*1700
		d.add(e)
	}
	d.seedDevices()
	d.lastTick = now
}

// setPolicyPreset — готовые наборы прав: simple — как их сохраняет простой
// режим страницы, hand — свои группы и правила без простого режима,
// default — «все → все» (как после первого включения), off — правила
// выключены, empty — ничего не задано.
func (d *demoBackend) setPolicyPreset(name string) error {
	groups := map[int][]string{}
	switch name {
	case "simple":
		for _, n := range []int{1, 2, 3, 8} {
			groups[n] = []string{"~mine"}
		}
		groups[4] = []string{"~server", "работа"}
		groups[5] = []string{"~server", "работа"}
		groups[6] = []string{"~guest"}
		groups[7] = []string{"~server"}
		d.access = AccessSettings{Enabled: true, Groups: []string{"~guest", "~mine", "~server", "работа"}, Rules: []AccessRule{
			{From: "работа", To: "работа", Ports: "22,3389"},
			{From: "~mine", To: "~mine"},
			{From: "~mine", To: "~server"},
			{From: "~mine", To: "@servers"},
		}}
	case "hand":
		groups[1], groups[2], groups[3] = []string{"семья"}, []string{"семья"}, []string{"семья"}
		groups[4], groups[5] = []string{"серверы"}, []string{"серверы"}
		d.access = AccessSettings{Enabled: true, Groups: []string{"семья", "серверы"}, Rules: []AccessRule{
			{From: "семья", To: "*"},
			{From: "серверы", To: "серверы", Ports: "22"},
		}}
	case "default":
		d.access = AccessSettings{Enabled: true, Rules: []AccessRule{{From: "*", To: "*"}}}
	case "off":
		d.access.Enabled = false
		return nil
	case "empty":
		d.access = AccessSettings{}
	default:
		return errors.New("нет такого набора: simple, hand, default, off, empty")
	}
	for i := range d.clients {
		n, _ := strconv.ParseUint(strings.TrimPrefix(d.clients[i].Username, "tun_"), 16, 64)
		d.clients[i].Groups = groups[int(n-0xa1b2c3d400000000)]
	}
	return nil
}

// ---------- журнал и «жизнь» сети ----------

func (d *demoBackend) add(e meshsvc.Event) {
	d.seq++
	e.Seq = d.seq
	if e.Time == 0 {
		e.Time = time.Now().Unix()
	}
	d.events = append(d.events, e)
	if len(d.events) > 2000 {
		d.events = append([]meshsvc.Event(nil), d.events[len(d.events)-1000:]...)
	}
}

// tick двигает выдуманную сеть: события появляются при обращениях к
// серверу, а не в своём потоке, — так демо ничего не делает, пока страницу
// никто не смотрит.
func (d *demoBackend) tick() {
	now := time.Now()
	for id, r := range d.returning {
		if now.After(r.at) {
			delete(d.returning, id)
			n := &MeshNetwork{ID: id, Created: now.Unix()}
			r.dev.Online, r.dev.ConnectedAt, r.dev.LastSeen = true, now.Unix(), now.Unix()
			n.Devices = []MeshDevice{r.dev}
			d.nets = append(d.nets, n)
			d.add(meshsvc.Event{Type: "connect", Net: id, Device: r.dev.ID})
		}
	}
	for i := 0; now.Sub(d.lastTick) >= demoTick && i < 5; i++ {
		d.lastTick = d.lastTick.Add(demoTick)
		d.step++
		d.simulate()
	}
	if now.Sub(d.lastTick) >= demoTick {
		d.lastTick = now
	}
	if d.egress.phase == EgressApplying && now.Sub(d.egress.appliedAt) >= demoEgressWindowSec*time.Second {
		d.egress = demoEgress{phase: EgressOff, mode: d.egress.mode, rolledBackAt: now.Unix()}
	}
}

// egressStatus — EgressStatus показа (без реальной системы): своя маленькая
// стейт-машина в demoEgress, не настоящий EgressManager.
func (d *demoBackend) egressStatus() EgressStatus {
	st := EgressStatus{Phase: d.egress.phase, Mode: d.egress.mode, Target: d.egress.target, RolledBackAt: d.egress.rolledBackAt}
	switch d.egress.phase {
	case EgressApplying:
		remain := demoEgressWindowSec - int(time.Since(d.egress.appliedAt).Seconds())
		if remain < 0 {
			remain = 0
		}
		st.Remaining, st.Token = remain, d.egress.token
	case EgressOn:
		st.ExitIP = d.egress.exitIP
	}
	return st
}

func (d *demoBackend) simulate() {
	// Каждое третье событие — попытка соединения, остальные — устройства
	// приходят и уходят, изредка пропадает побочный сервер.
	switch {
	case d.step%11 == 0:
		s := &d.servers[1]
		s.Online = !s.Online
		typ := "server-down"
		if s.Online {
			typ = "server-up"
		}
		d.add(meshsvc.Event{Type: typ, Device: s.ID, Text: s.Name})
	case d.step%3 == 0:
		n := d.nets[0]
		var on []MeshDevice
		for _, x := range n.Devices {
			if x.Online {
				on = append(on, x)
			}
		}
		if len(on) < 2 {
			return
		}
		a, b := on[d.rnd.Intn(len(on))], on[d.rnd.Intn(len(on))]
		if a.ID == b.ID {
			return
		}
		ports := []int{22, 3389, 47835, 47837, 445}
		port := ports[d.rnd.Intn(len(ports))]
		if !d.policy().allowed(a.User, b.User, port) {
			d.add(meshsvc.Event{Type: "denied", Net: n.ID, Device: a.ID, Peer: b.ID, Port: port})
		}
	default:
		var all []*MeshDevice
		for _, n := range d.nets {
			for i := range n.Devices {
				if n.Devices[i].ID != "e1a04f" { // главный ПК всегда на связи
					all = append(all, &n.Devices[i])
				}
			}
		}
		if len(all) == 0 {
			return
		}
		x := all[d.rnd.Intn(len(all))]
		net := d.netOf(x.ID)
		x.Online = !x.Online
		now := time.Now().Unix()
		x.LastSeen = now
		if x.Online {
			x.ConnectedAt = now
			if x.RTTMs == 0 {
				x.RTTMs, x.RTTAvgMs, x.Quality = 40, 42, "good"
			}
			d.add(meshsvc.Event{Type: "connect", Net: net, Device: x.ID})
		} else {
			d.add(meshsvc.Event{Type: "disconnect", Net: net, Device: x.ID})
		}
	}
}

func (d *demoBackend) netOf(device string) string {
	for _, n := range d.nets {
		for _, x := range n.Devices {
			if x.ID == device {
				return n.ID
			}
		}
	}
	return ""
}

func (d *demoBackend) findNet(id string) (*MeshNetwork, int) {
	for i, n := range d.nets {
		if n.ID == id {
			return n, i
		}
	}
	return nil, -1
}

// ---------- права: то же решение, что у meshd ----------

// demoPolicy — копия решения meshd (cmd/meshd, policy.allowed/match/visible):
// meshd — отдельная программа на стандартной библиотеке, импортировать его
// нельзя.
type demoPolicy meshsvc.Policy

func (d *demoBackend) policy() demoPolicy { return demoPolicy(buildPolicy(d.access, d.clients)) }

func (p demoPolicy) member(user, group string) bool {
	if group == GroupAll {
		return true
	}
	if user == GroupServers {
		return group == GroupServers
	}
	for _, g := range p.Users[user] {
		if g == group {
			return true
		}
	}
	return false
}

func (p demoPolicy) match(from, to string, port int) int {
	if from == GroupServers {
		return -1
	}
	for i, r := range p.Rules {
		if !p.member(from, r.From) || !p.member(to, r.To) {
			continue
		}
		if r.Ports == "" || (port > 0 && meshsvc.PortAllowed(r.Ports, port)) {
			return i
		}
	}
	return -1
}

func (p demoPolicy) allowed(from, to string, port int) bool {
	return !p.Enabled || p.match(from, to, port) >= 0
}

func (p demoPolicy) visible(a, b string) bool {
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

// nodeStatus — состояние хоста плагинов для /api/node/state.
func (d *demoBackend) nodeStatus() NodeStatus {
	if !d.nodeInstalled {
		return NodeStatus{Unit: UnitState{Installed: false}}
	}
	st := nodeapi.State{Version: d.version, Config: d.nodeConfig, Meshd: "ok"}
	now := time.Now().Unix()
	for i, n := range d.nets {
		net := nodeapi.Network{ID: n.ID, Folders: []nodeapi.Folder{}}
		// Последняя активность — выдуманная, но разная по плагинам.
		net.LastActive = map[string]int64{}
		if len(d.nodeFolders[n.ID]) > 0 {
			net.LastActive[nodeapi.PluginMirror] = now - int64(40+i*300)
		}
		if d.nodeConfig.Clipboard {
			net.LastActive[nodeapi.PluginClipboard] = now - int64(90+i*600)
		}
		if bs := d.nodeBackups[n.ID]; len(bs) > 0 {
			net.Backups = append([]nodeapi.BackupSet(nil), bs...)
			for _, b := range bs {
				net.BackupBytes += b.Bytes * 2 / 3
			}
			net.LastActive[nodeapi.PluginBackup] = bs[0].Last
		}
		for _, f := range d.nodeFolders[n.ID] {
			f.UsedBytes = int64(len(f.Name)) << 20 // выдуманное, но не ноль
			net.Folders = append(net.Folders, f)
		}
		if items := d.nodeClipboard[n.ID]; items > 0 {
			net.Clipboard = nodeapi.Clipboard{Items: items, Bytes: int64(items) << 10, Devices: len(n.Devices)}
		}
		if d.nodeConfig.Machines && !d.nodeMachClear[n.ID] && len(n.Devices) > 0 {
			// Показания присылает первое устройство сети (домашний сервер).
			dev := n.Devices[0]
			net.Machines = &nodeapi.MachinesNet{Bytes: int64(3+i) << 20, Oldest: now - 9*86400,
				Reporters: []nodeapi.MachinesReporter{{ID: nodeapi.PeerID(dev.ID), Name: dev.Name, Shared: true,
					Last: now - 20, Machines: []string{"NAS", "Proxmox", "Ubuntu"}}}}
			net.UsedBytes += net.Machines.Bytes
			net.LastActive[nodeapi.PluginMachines] = now - 20
		}
		if d.nodeConfig.Adblock && d.nodeAd.Version != "" {
			net.LastActive[nodeapi.PluginAdblock] = now - int64(300+i*120)
		}
		if d.nodeConfig.Sitecheck && d.nodeSiteAt > 0 && i == 0 {
			net.LastActive[nodeapi.PluginSitecheck] = d.nodeSiteAt
		}
		st.Networks = append(st.Networks, net)
	}
	ad := d.nodeAd
	if d.nodeConfig.Adblock && ad.Version != "" {
		ad.Devices = len(d.nets)
	}
	st.Adblock = &ad
	if d.nodeOrphan != "" {
		st.Networks = append(st.Networks, nodeapi.Network{ID: d.nodeOrphan, Folders: []nodeapi.Folder{
			{ID: "old1", Name: "Старые фото", QuotaMB: 2048, UsedBytes: 734 << 20, Files: 212}},
			FilesBytes: 12 << 20, UsedBytes: 746 << 20})
	}
	for _, n := range st.Networks {
		st.UsedBytes += n.UsedBytes + n.BackupBytes
		for _, f := range n.Folders {
			st.UsedBytes += f.UsedBytes
		}
	}
	ns := NodeStatus{Unit: UnitState{Installed: true, Active: true, Enabled: true, Detail: "active"}, State: &st}
	ns.Warnings = append(pluginWarnings(MeshRoleMain, d.self.Enabled, d.self.Ports, d.access, d.clients, d.nodeConfig), stateWarnings(&st)...)
	nets := make([]MeshNetwork, 0, len(d.nets))
	for _, n := range d.nets {
		nets = append(nets, *n)
	}
	ns.Access = nodeAccess(nets, &st)
	return ns
}

// demoAdUpdate — «скачать» источники: выдуманный счёт имён.
func (d *demoBackend) demoAdUpdate() {
	now := time.Now().Unix()
	d.nodeAd.Checked, d.nodeAd.Error, d.nodeAd.Fetched = now, "", nil
	if len(d.nodeAd.Sources) == 0 {
		d.nodeAd.Error = "не задан ни один источник"
		return
	}
	total := len(d.nodeAd.Deny)
	for i, u := range d.nodeAd.Sources {
		n := 48000 + i*7311
		d.nodeAd.Fetched = append(d.nodeAd.Fetched, nodeapi.AdblockSource{URL: u, Names: n})
		total += n
	}
	d.nodeAd.Updated, d.nodeAd.Names = now, total-len(d.nodeAd.Allow)
	d.nodeAd.Version = strconv.FormatInt(now, 16)
}

// demoSiteCheck — проверка сайта с сервера в показе: без сети, по шагам.
func demoSiteCheck(req nodeapi.SiteReq) (probe.ServerResult, error) {
	t, err := probe.ParseSite(req.Site)
	if err != nil {
		return probe.ServerResult{}, err
	}
	if t.Port != "443" {
		return probe.ServerResult{}, probe.ErrPort
	}
	lang := req.Lang
	res := probe.ServerResult{Site: req.Site, Host: t.Host, Time: time.Now()}
	if a, err := netip.ParseAddr(t.Host); (err == nil && !probe.Public(a)) || !strings.Contains(t.Host, ".") || strings.HasSuffix(t.Host, ".local") || strings.HasSuffix(t.Host, ".mesh") {
		res.Server = res.Server.FailAt(probe.Step{ID: "dns", Kind: probe.KindRefused, Note: t.Host})
	} else if strings.HasSuffix(t.Host, ".invalid") || strings.Contains(t.Host, "nosuch") {
		res.Server = res.Server.FailAt(probe.Step{ID: "dns", Kind: probe.KindDNSFail, Ms: 31, Note: "no such host"})
	} else {
		res.Server = probe.Path{OK: true, IPs: []string{"203.0.113.10"}, Steps: []probe.Step{
			{ID: "dns", OK: true, Ms: 12, Note: "203.0.113.10"}, {ID: "tcp", OK: true, Ms: 38},
			{ID: "tls", OK: true, Ms: 61, Note: "Let's Encrypt"}, {ID: "http", OK: true, Ms: 95, Note: "200 OK"}}}
	}
	res.Verdict = probe.ServerVerdict(res.Server)
	res.Text = probe.ServerPhrase(res.Verdict, lang)
	return res, nil
}

// seedBackups — выдуманные резервные копии устройств (показ).
func (d *demoBackend) seedBackups() {
	if len(d.nodeBackups) > 0 || len(d.nets) == 0 {
		return
	}
	now := time.Now().Unix()
	n := d.nets[0]
	var out []nodeapi.BackupSet
	names := []string{"Документы", "Фото с телефона"}
	for i, dev := range n.Devices {
		if i >= len(names) {
			break
		}
		out = append(out, nodeapi.BackupSet{Scope: "u-demo" + strconv.Itoa(i), Device: dev.ID, DeviceName: dev.Name,
			Set: "set" + strconv.Itoa(i+1), Name: names[i], Keep: 10, Versions: 4 + i*3,
			Bytes: int64(380+i*1900) << 20, Last: now - int64(3600*(i*20+2))})
	}
	d.nodeBackups[n.ID] = out
}

// ---------- ручки ----------

func (d *demoBackend) routes() http.Handler {
	mux := http.NewServeMux()
	ok := map[string]bool{"ok": true}
	page := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := assets.ReadFile("assets/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
	}
	mux.HandleFunc("/", page)
	// Каждая ручка — под общим замком и после шага «жизни» сети.
	h := func(path string, fn func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.tick()
			fn(w, r)
		})
	}

	h("/api/session", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": "demo", "mustChange": false, "demo": true})
	})
	h("/api/login", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": "demo", "mustChange": false})
	})
	h("/api/logout", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, ok) })
	h("/api/change-password", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, ok) })
	h("/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"os": "linux", "uptimeSeconds": int64(time.Since(d.started).Seconds()) + 3*86400,
			"version": d.version, "demo": true})
	})

	// --- клиенты ---
	h("/api/clients", func(w http.ResponseWriter, r *http.Request) {
		list := append([]Client{}, d.clients...)
		for i := range list {
			list[i].PrivateKey, list[i].CreatorHash = "", ""
		}
		writeJSON(w, http.StatusOK, map[string]any{"clients": list})
	})
	h("/api/clients/create", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name       string `json:"name"`
			DeviceType string `json:"deviceType"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" || len([]rune(name)) > maxNameLength || !validDeviceType(DeviceType(req.DeviceType)) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "имя или тип устройства не подходят"})
			return
		}
		u, err := GenerateUsername()
		if err == nil {
			var pub, priv string
			pub, priv, err = generateKeyPair()
			c := Client{ID: u, Username: u, Name: name, DeviceType: DeviceType(req.DeviceType), CreatedAt: time.Now(),
				State: StateActive, PublicKey: pub, PrivateKey: priv}
			if err == nil {
				d.clients = append(d.clients, c)
				resp := map[string]any{"ok": true, "client": c}
				if cfg, err := BuildClientConfigPayload(c, "demo.example", 22, "", true); err != nil {
					resp["configError"] = err.Error()
				} else {
					resp["config"] = cfg
				}
				writeJSON(w, http.StatusOK, resp)
				return
			}
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	})
	clientAct := func(fn func(c *Client) error) func(w http.ResponseWriter, r *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				ID string `json:"id"`
			}
			if !decodePost(w, r, &req) {
				return
			}
			c := d.client(req.ID)
			if c == nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "нет такого клиента"})
				return
			}
			replyErr(w, fn(c), ok)
		}
	}
	h("/api/clients/delete", clientAct(func(c *Client) error {
		for i := range d.clients {
			if d.clients[i].ID == c.ID {
				d.clients = append(d.clients[:i], d.clients[i+1:]...)
				break
			}
		}
		return nil
	}))
	h("/api/clients/freeze", clientAct(func(c *Client) error { c.State, c.Sessions = StateFrozen, 0; return nil }))
	h("/api/clients/unfreeze", clientAct(func(c *Client) error { c.State = StateActive; return nil }))
	h("/api/clients/disconnect", clientAct(func(c *Client) error { c.Sessions = 0; return nil }))
	h("/api/clients/config", func(w http.ResponseWriter, r *http.Request) {
		c := d.client(r.URL.Query().Get("id"))
		if c == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "client_not_found"})
			return
		}
		cfg, err := BuildClientConfigPayload(*c, "demo.example", 22, "", false)
		replyErr(w, err, map[string]any{"ok": true, "config": cfg})
	})
	h("/api/clients/groups", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     string   `json:"id"`
			Groups []string `json:"groups"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		groups, err := cleanGroups(req.Groups)
		known := map[string]bool{}
		for _, g := range d.access.Groups {
			known[g] = true
		}
		for _, g := range groups {
			if err == nil && !known[g] {
				err = errors.New("нет такой группы: " + g)
			}
		}
		c := d.client(req.ID)
		if err == nil && c == nil {
			err = errors.New("нет такого клиента")
		}
		if err == nil {
			c.Groups = groups
		}
		replyErr(w, err, ok)
	})
	h("/api/clients/creator", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID        string `json:"id"`
			CanCreate bool   `json:"canCreate"`
			Password  string `json:"password"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		c := d.client(req.ID)
		switch {
		case c == nil:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "нет такого клиента"})
		case req.Password != "" && len([]rune(req.Password)) < minCreatorPassword:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "пароль — не короче 8 символов"})
		case req.CanCreate && !c.CanCreate && req.Password == "":
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "задай пароль, по которому он будет создавать пользователей"})
		default:
			c.CanCreate = req.CanCreate
			writeJSON(w, http.StatusOK, ok)
		}
	})
	h("/api/traffic", func(w http.ResponseWriter, r *http.Request) {
		period := r.URL.Query().Get("period")
		div := map[string]uint64{"day": 30, "week": 4, "month": 1, "all": 1}[period]
		if div == 0 {
			period, div = "month", 1
		}
		var rx, tx uint64
		out := []clientTraffic{}
		for _, c := range d.clients {
			t := clientTraffic{ID: c.ID, Name: c.Name, DeviceType: c.DeviceType, RxBytes: c.RxBytes / div, TxBytes: c.TxBytes / div}
			if period == "all" {
				t.RxBytes, t.TxBytes = t.RxBytes*3, t.TxBytes*3
			}
			rx, tx = rx+t.RxBytes, tx+t.TxBytes
			out = append(out, t)
		}
		writeJSON(w, http.StatusOK, map[string]any{"period": period, "rxBytes": rx, "txBytes": tx, "clients": out})
	})

	// --- настройки панели ---
	h("/api/autostart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "autostart_unsupported"})
			return
		}
		writeJSON(w, http.StatusOK, AutostartStatus{})
	})
	h("/api/update", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"error": "в режиме показа обновления не проверяются", "code": "update_failed"})
	})
	h("/api/update/install", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"error": "в режиме показа обновление выключено"})
	})
	h("/api/update/progress", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"running": false, "version": d.version})
	})
	capacity := func() CapacityStatus {
		n := EffectiveMaxDevices(d.capacity)
		return CapacityStatus{MaxDevices: n, Min: MinMaxDevices, Max: MaxMaxDevices, Default: DefaultMaxDevices,
			PerDevice: connsPerDevice, Wanted: MaxStartupsFor(n), Effective: MaxStartupsFor(n)}
	}
	h("/api/settings/capacity", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, capacity())
			return
		}
		var req struct {
			MaxDevices int `json:"maxDevices"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		err := ValidMaxDevices(req.MaxDevices)
		if err == nil {
			d.capacity = req.MaxDevices
		}
		replyErr(w, err, capacity())
	})

	// --- права доступа: те же проверки, что у настоящей панели ---
	h("/api/access", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{"access": d.access})
			return
		}
		var req AccessSettings
		if !decodePost(w, r, &req) {
			return
		}
		if err := req.clean(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		have := map[string]bool{}
		for _, g := range req.Groups {
			have[g] = true
		}
		for _, c := range d.clients {
			for _, g := range c.Groups {
				if !have[g] {
					writeJSON(w, http.StatusBadRequest, map[string]string{
						"error": "в группе «" + g + "» ещё есть устройства (" + c.Name + ") — сначала убери их из неё"})
					return
				}
			}
		}
		d.access = req
		writeJSON(w, http.StatusOK, ok)
	})

	// --- сеть устройств ---
	h("/api/mesh/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, d.state()) })
	h("/api/mesh/role", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Role string `json:"role"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		if req.Role == MeshRoleMain {
			writeJSON(w, http.StatusOK, ok)
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "в режиме показа сервер остаётся главным"})
	})
	// «Обновить meshd» в показе лечит «старую версию» — чтобы было видно,
	// как страница возвращается к полной работе.
	h("/api/mesh/reinstall", func(w http.ResponseWriter, r *http.Request) {
		d.oldMeshd = false
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/udprelay/install", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, ok) })
	h("/api/egress", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, d.egressStatus()) })
	h("/api/egress/apply", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Link string `json:"link"`
			Mode string `json:"mode"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		if d.egress.phase != EgressOff {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "выход через другой сервер уже включён — сначала выключи его"})
			return
		}
		if req.Mode == "" {
			req.Mode = d.egress.mode
		}
		if req.Mode != EgressModeBlock && req.Mode != EgressModeDirect {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "неизвестное поведение без сервера"})
			return
		}
		target := d.egress.target
		if link := strings.TrimSpace(req.Link); link != "" {
			doc, err := share.Parse([]byte(link))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			if !doc.KeyIncluded {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "в ссылке нет ключа — нужна ссылка нового клиента из панели того сервера"})
				return
			}
			name := doc.Name
			if name == "" {
				name = doc.Host
			}
			target = &EgressTarget{Name: name, Host: doc.Host, IP: "203.0.113." + strconv.Itoa(10+d.rnd.Intn(240)),
				Port: doc.SSHPort, User: doc.User}
		} else if target == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "вставь ссылку клиента из панели сервера, через который выходить"})
			return
		}
		d.egress = demoEgress{phase: EgressApplying, mode: req.Mode, target: target,
			appliedAt: time.Now(), token: randomHex(12)}
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/egress/confirm", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		if d.egress.phase != EgressApplying || req.Token == "" || req.Token != d.egress.token {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "подтверждать нечего — окно уже закрылось"})
			return
		}
		d.egress.phase, d.egress.token = EgressOn, ""
		d.egress.exitIP = d.egress.target.IP
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/egress/off", func(w http.ResponseWriter, r *http.Request) {
		var req struct{}
		if !decodePost(w, r, &req) {
			return
		}
		d.egress = demoEgress{phase: EgressOff, mode: d.egress.mode}
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/egress/mode", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Mode string `json:"mode"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		if req.Mode != EgressModeBlock && req.Mode != EgressModeDirect {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "неизвестное поведение без сервера"})
			return
		}
		d.egress.mode = req.Mode
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/mesh/links/create", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "code": "sshtunnel-link1:демо-код-не-настоящий",
			"link": MeshLink{ID: "demo", Name: "Демо", Added: time.Now()}})
	})
	h("/api/mesh/links/delete", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "в режиме показа серверы не удаляются"})
	})
	h("/api/mesh/join", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "в режиме показа сервер остаётся главным"})
	})
	h("/api/mesh/known", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Servers []meshsvc.KnownServer `json:"servers"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		for _, k := range req.Servers {
			if err := validHost(strings.TrimSpace(k.Host)); err != nil {
				replyErr(w, err, nil)
				return
			}
		}
		d.known = append([]meshsvc.KnownServer{}, req.Servers...)
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/mesh/serveraccess", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled bool   `json:"enabled"`
			Ports   string `json:"ports"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		err := meshsvc.ValidPorts(req.Ports)
		if err == nil {
			d.self.Enabled, d.self.Ports = req.Enabled, req.Ports
		}
		replyErr(w, err, ok)
	})
	h("/api/mesh/rename", func(w http.ResponseWriter, r *http.Request) {
		var req meshDeviceReq
		if !decodePost(w, r, &req) {
			return
		}
		x := d.device(req.Net, req.Device)
		if x == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "нет такого устройства"})
			return
		}
		alias := cleanName(req.Alias)
		x.Alias, x.Display = alias, alias
		if alias == "" {
			x.Display = x.Name
		}
		d.add(meshsvc.Event{Type: "rename", Net: req.Net, Device: x.ID, Text: x.Display})
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/mesh/forget", func(w http.ResponseWriter, r *http.Request) {
		var req meshDeviceReq
		if !decodePost(w, r, &req) {
			return
		}
		n, _ := d.findNet(req.Net)
		if n == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "нет такой сети"})
			return
		}
		for i, x := range n.Devices {
			if x.ID == req.Device {
				n.Devices = append(n.Devices[:i], n.Devices[i+1:]...)
				d.add(meshsvc.Event{Type: "forget", Net: n.ID, Device: x.ID})
				if len(n.Devices) == 0 {
					n.EmptySince = time.Now().Unix()
				}
				break
			}
		}
		writeJSON(w, http.StatusOK, ok)
	})
	unsupported := func(w http.ResponseWriter) bool {
		if d.oldMeshd {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": meshsvc.ErrUnsupported.Error()})
		}
		return d.oldMeshd
	}
	h("/api/mesh/net/rename", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Net  string `json:"net"`
			Name string `json:"name"`
		}
		if !decodePost(w, r, &req) || unsupported(w) {
			return
		}
		n, _ := d.findNet(req.Net)
		name := strings.TrimSpace(req.Name)
		switch {
		case n == nil:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "нет такой сети"})
		case len([]rune(name)) > 64:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "имя сети — не длиннее 64 символов"})
		default:
			n.Name = name
			d.add(meshsvc.Event{Type: "rename", Net: n.ID, Text: name})
			writeJSON(w, http.StatusOK, ok)
		}
	})
	h("/api/mesh/net/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Net string `json:"net"`
		}
		if !decodePost(w, r, &req) || unsupported(w) {
			return
		}
		n, i := d.findNet(req.Net)
		if n == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "нет такой сети"})
			return
		}
		d.nets = append(d.nets[:i], d.nets[i+1:]...)
		d.add(meshsvc.Event{Type: "net-delete", Net: n.ID, Text: n.Name})
		// Как у настоящего meshd: устройство с тем же ключом сети вернёт её.
		for _, x := range n.Devices {
			if x.Online {
				d.returning[n.ID] = demoReturn{at: time.Now().Add(demoReturnAfter), dev: x}
				break
			}
		}
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/mesh/events", func(w http.ResponseWriter, r *http.Request) {
		if unsupported(w) {
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 200
		}
		if limit > 1000 {
			limit = 1000
		}
		out := []meshsvc.Event{}
		for _, e := range d.events {
			if e.Seq > after {
				out = append(out, e)
			}
		}
		if len(out) > limit {
			out = out[:limit]
		}
		writeJSON(w, http.StatusOK, meshsvc.Events{Seq: d.seq, Events: out})
	})
	d.deviceRoutes(h)
	h("/api/mesh/check", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Net  string `json:"net"`
			From string `json:"from"`
			To   string `json:"to"`
			Port int    `json:"port"`
		}
		if !decodePost(w, r, &req) || unsupported(w) {
			return
		}
		res, err := d.check(req.Net, req.From, req.To, req.Port)
		replyErr(w, err, res)
	})

	// --- плагины сервера (ТЗ 9) ---
	h("/api/node/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, d.nodeStatus())
	})
	h("/api/node/install", func(w http.ResponseWriter, r *http.Request) {
		d.nodeInstalled = true
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/node/remove", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			DeleteData bool `json:"deleteData"`
		}
		decodePost(w, r, &req)
		d.nodeInstalled = false
		if req.DeleteData {
			d.nodeFolders, d.nodeClipboard = map[string][]nodeapi.Folder{}, map[string]int{}
			d.nodeBackups, d.nodeOrphan = map[string][]nodeapi.BackupSet{}, ""
		}
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/node/config", func(w http.ResponseWriter, r *http.Request) {
		var p nodeapi.ConfigPatch
		if !decodePost(w, r, &p) {
			return
		}
		if p.Mirror != nil {
			d.nodeConfig.Mirror = *p.Mirror
		}
		if p.Files != nil {
			d.nodeConfig.Files = *p.Files
		}
		if p.FilesWrite != nil {
			d.nodeConfig.FilesWrite = *p.FilesWrite
		}
		if p.Clipboard != nil {
			d.nodeConfig.Clipboard = *p.Clipboard
		}
		if p.Backup != nil {
			d.nodeConfig.Backup = *p.Backup
			if d.nodeConfig.Backup {
				d.seedBackups()
			}
		}
		if p.QuotaMB != nil && *p.QuotaMB > 0 {
			d.nodeConfig.QuotaMB = *p.QuotaMB
		}
		if p.Machines != nil {
			d.nodeConfig.Machines = *p.Machines
		}
		if p.Sitecheck != nil {
			d.nodeConfig.Sitecheck = *p.Sitecheck
		}
		if p.MachinesDays != nil {
			if *p.MachinesDays < 1 || *p.MachinesDays > 3650 {
				replyErr(w, errors.New("историю машин хранить — от 1 до 3650 дней"), nil)
				return
			}
			d.nodeConfig.MachinesDays = *p.MachinesDays
		}
		if p.Adblock != nil {
			d.nodeConfig.Adblock = *p.Adblock
			if d.nodeConfig.Adblock && d.nodeAd.Version == "" {
				d.demoAdUpdate()
			}
		}
		writeJSON(w, http.StatusOK, d.nodeStatus().State)
	})
	h("/api/node/adblock/set", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.AdblockConf
		if !decodePost(w, r, &req) {
			return
		}
		clean := func(in []string) []string {
			out := []string{}
			for _, x := range in {
				if x = strings.ToLower(strings.TrimSpace(x)); x != "" {
					out = append(out, x)
				}
			}
			return out
		}
		for _, u := range clean(req.Sources) {
			if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				replyErr(w, fmt.Errorf("источник — ссылка http:// или https://: %q", u), nil)
				return
			}
		}
		d.nodeAd.Sources, d.nodeAd.Allow, d.nodeAd.Deny = clean(req.Sources), clean(req.Allow), clean(req.Deny)
		if d.nodeAd.Version != "" {
			d.demoAdUpdate()
		}
		writeJSON(w, http.StatusOK, d.nodeAd)
	})
	h("/api/node/adblock/update", func(w http.ResponseWriter, r *http.Request) {
		var req struct{}
		if !decodePost(w, r, &req) {
			return
		}
		d.demoAdUpdate()
		writeJSON(w, http.StatusOK, d.nodeAd)
	})
	h("/api/node/sitecheck", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.SiteReq
		if !decodePost(w, r, &req) {
			return
		}
		res, err := demoSiteCheck(req)
		if err == nil {
			d.nodeSiteAt = time.Now().Unix()
		}
		replyErr(w, err, res)
	})
	h("/api/node/machines/clear", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Net string `json:"net"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		d.nodeMachClear[req.Net] = true
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/node/folder/add", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.FolderReq
		if !decodePost(w, r, &req) {
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			replyErr(w, errors.New("имя папки — от 1 до 64 символов"), nil)
			return
		}
		if req.QuotaMB <= 0 {
			req.QuotaMB = nodeapi.DefaultFolderQuotaMB
		}
		f := nodeapi.Folder{ID: fmt.Sprintf("demo%d", len(d.nodeFolders[req.Net])+1), Name: name, QuotaMB: req.QuotaMB,
			Devices: []string{}}
		d.nodeFolders[req.Net] = append(d.nodeFolders[req.Net], f)
		writeJSON(w, http.StatusOK, f)
	})
	h("/api/node/folder/remove", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.FolderReq
		if !decodePost(w, r, &req) {
			return
		}
		list := d.nodeFolders[req.Net]
		out := list[:0]
		for _, f := range list {
			if f.ID != req.ID {
				out = append(out, f)
			}
		}
		d.nodeFolders[req.Net] = out
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/node/folder/quota", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.FolderReq
		if !decodePost(w, r, &req) {
			return
		}
		list := d.nodeFolders[req.Net]
		for i := range list {
			if list[i].ID == req.ID {
				list[i].QuotaMB = req.QuotaMB
			}
		}
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/node/folder/access", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.AccessReq
		if !decodePost(w, r, &req) {
			return
		}
		if req.Device == "" {
			replyErr(w, errors.New("нужны папка и id устройства"), nil)
			return
		}
		list := d.nodeFolders[req.Net]
		found := false
		for i := range list {
			if req.Folder != "*" && list[i].ID != req.Folder {
				continue
			}
			found = true
			devs := []string{}
			for _, x := range list[i].Devices {
				if x != req.Device {
					devs = append(devs, x)
				}
			}
			if req.On {
				devs = append(devs, req.Device)
				sort.Strings(devs)
			}
			list[i].Devices, list[i].Old = devs, false
		}
		if !found {
			replyErr(w, errors.New("нет такой папки"), nil)
			return
		}
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/node/clipboard/clear", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Net string `json:"net"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		delete(d.nodeClipboard, req.Net)
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/node/backup/delete", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.BackupReq
		if !decodePost(w, r, &req) {
			return
		}
		list, out := d.nodeBackups[req.Net], []nodeapi.BackupSet{}
		for _, b := range list {
			if !(b.Scope == req.Scope && b.Device == req.Device && b.Set == req.Set) {
				out = append(out, b)
			}
		}
		if len(out) == len(list) {
			replyErr(w, errors.New("такой копии нет"), nil)
			return
		}
		d.nodeBackups[req.Net] = out
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/node/net/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Net string `json:"net"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		for _, n := range d.nets {
			if n.ID == req.Net {
				replyErr(w, errors.New("эта сеть ещё есть в сети устройств — сначала удали её в «Сетях»"), nil)
				return
			}
		}
		if req.Net == d.nodeOrphan {
			d.nodeOrphan = ""
		}
		delete(d.nodeFolders, req.Net)
		delete(d.nodeClipboard, req.Net)
		delete(d.nodeBackups, req.Net)
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/node/fix", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Code string `json:"code"`
		}
		if !decodePost(w, r, &req) {
			return
		}
		switch req.Code {
		case WarnServerOff, WarnPortsBlocked:
			d.self.Ports = fixServerPorts(d.self.Enabled, d.self.Ports, d.nodeConfig)
			d.self.Enabled = true
			writeJSON(w, http.StatusOK, ok)
		default:
			replyErr(w, errors.New("для этого исправления одной кнопкой нет"), nil)
		}
	})

	// --- только для проверки показа ---
	h("/api/demo/restart", func(w http.ResponseWriter, r *http.Request) {
		d.events, d.seq, d.meshdStarted = nil, 0, time.Now().Unix()
		d.add(meshsvc.Event{Type: "server-up", Device: "london", Text: "Лондон"})
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/demo/oldmeshd", func(w http.ResponseWriter, r *http.Request) {
		d.oldMeshd = r.URL.Query().Get("on") != "0"
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "oldMeshd": d.oldMeshd})
	})
	h("/api/demo/policy", func(w http.ResponseWriter, r *http.Request) {
		replyErr(w, d.setPolicyPreset(r.URL.Query().Get("preset")), ok)
	})
	h("/api/demo/burst", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		for i := 0; i < n && i < 1000; i++ {
			d.step++
			d.simulate()
		}
		writeJSON(w, http.StatusOK, map[string]int64{"seq": d.seq})
	})
	return mux
}

func (d *demoBackend) client(id string) *Client {
	for i := range d.clients {
		if d.clients[i].ID == id {
			return &d.clients[i]
		}
	}
	return nil
}

func (d *demoBackend) device(netID, id string) *MeshDevice {
	n, _ := d.findNet(netID)
	if n == nil {
		return nil
	}
	for i := range n.Devices {
		if n.Devices[i].ID == id {
			return &n.Devices[i]
		}
	}
	return nil
}

// userOf — SSH-пользователь устройства или сервера сети (у серверов —
// «@servers», как у meshd).
func (d *demoBackend) userOf(netID, id string) (user string, server, ok bool) {
	if x := d.device(netID, id); x != nil {
		return x.User, false, true
	}
	for _, s := range d.servers {
		if s.ID == id {
			return GroupServers, true, true
		}
	}
	return "", false, false
}

func (d *demoBackend) check(netID, from, to string, port int) (meshsvc.CheckResult, error) {
	if port < 0 || port > 65535 {
		return meshsvc.CheckResult{}, errors.New("неверный порт")
	}
	if n, _ := d.findNet(netID); n == nil {
		return meshsvc.CheckResult{}, errors.New("нет такой сети")
	}
	fu, fromServer, ok1 := d.userOf(netID, from)
	tu, _, ok2 := d.userOf(netID, to)
	if !ok1 || !ok2 {
		return meshsvc.CheckResult{Rule: -1, Reason: "unknown device"}, nil
	}
	p := d.policy()
	res := meshsvc.CheckResult{Rule: -1, Visible: p.visible(fu, tu)}
	held := d.heldReason(netID, from, to)
	switch {
	case held != "":
		res.Reason, res.Visible = held, false
	case fromServer:
		res.Reason = "server cannot initiate"
	case !p.Enabled:
		res.Allowed, res.Reason = true, "policy off"
	default:
		res.Rule = p.match(fu, tu, port)
		res.Allowed = res.Rule >= 0
		res.Reason = map[bool]string{true: "rule", false: "no rule"}[res.Allowed]
	}
	return res, nil
}

func (d *demoBackend) state() MeshState {
	st := MeshState{
		Role: MeshRoleMain, EffectiveRole: MeshRoleMain, Name: d.servers[0].Name,
		Meshd:    UnitState{Installed: true, Active: true, Enabled: true},
		UDPRelay: UnitState{Installed: true, Active: true, Enabled: true},
		SSHHost:  "demo.example", SSHPort: 22,
		ServerAccess: d.self.Enabled, ServerPorts: d.self.Ports,
		Version: "demo", StartedAt: d.meshdStarted,
		Servers: append([]MeshServer{}, d.servers...), Networks: []MeshNetwork{},
	}
	if d.oldMeshd {
		st.Version = "1.6.0 (старая)"
	}
	// Другие серверы сети по рассказам устройств: у Амстердама правила
	// доступа другие — панель должна это сказать.
	st.Peers = []meshsvc.SeenServer{
		{ID: "ams01", Name: "Амстердам", Pol: "a1b2c3", Same: false, Devices: 2, LastSeen: time.Now().Unix()},
	}
	st.Known = append([]meshsvc.KnownServer{}, d.known...)
	self := d.self
	st.Self = &self
	for _, n := range d.nets {
		c := *n
		c.Devices = append([]MeshDevice{}, n.Devices...)
		if d.oldMeshd {
			// Старый meshd не знает имён сетей, дат и пользователей устройств.
			c.Name, c.Created, c.EmptySince = "", 0, 0
			for i := range c.Devices {
				c.Devices[i].User = ""
			}
			stripDeviceProfile(&c)
		}
		st.Networks = append(st.Networks, c)
	}
	sort.SliceStable(st.Networks, func(i, j int) bool { return st.Networks[i].Created < st.Networks[j].Created })
	return st
}
