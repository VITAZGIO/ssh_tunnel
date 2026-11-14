package mesh

// Сеть устройств без сервера (ТЗ 3): прямые соединения переживают потерю
// meshd, устройства находят друг друга после перезапуска по запомненным
// адресам и маякам.

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// node — клиент, которого можно остановить и запустить заново с тем же
// StateDir и DeviceID («перезапуск программы»).
type node struct {
	t    *testing.T
	cfg  Config
	c    *Client
	stop func()
	// хуки маяков, если нужны
	listen  func() (net.PacketConn, error)
	targets func() []*net.UDPAddr
}

func newNode(t *testing.T, addr string, cfg Config) *node {
	cfg.Addr = addr
	if cfg.DeviceID == "" {
		cfg.DeviceID = NewDeviceID()
	}
	cfg.NoNATProbe = true
	if cfg.Via == "" {
		cfg.Via = "127.0.0.1"
	}
	return &node{t: t, cfg: cfg}
}

func (n *node) start() *Client {
	c := New(n.cfg, directDial, nil)
	c.beaconListen, c.beaconTargets = n.listen, n.targets
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	n.c = c
	n.stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			n.t.Error("Run не вернулся")
		}
	}
	n.t.Cleanup(n.stop)
	return c
}

func (n *node) ip() string { return n.c.Status().Self.IP.String() }

func dialAsk(t *testing.T, c *Client, target, text string) string {
	t.Helper()
	conn, err := c.DialPeer(target)
	if err != nil {
		t.Fatalf("DialPeer(%s): %v", target, err)
	}
	return ask(t, conn, text)
}

// Два устройства пошли напрямую, сервер пропал: новые соединения между ними
// работают, статус — «local», имена отвечают.
func TestБезСервераПрямоеСоединениеЖивёт(t *testing.T) {
	addr, _, _, kill := startMeshdSTUNKill(t)
	key := NewKey()
	a := newNode(t, addr, Config{Key: key, Name: "a", AllowIncoming: true})
	b := newNode(t, addr, Config{Key: key, Name: "b", AllowIncoming: true})
	a.start()
	b.start()
	waitFor(t, func() bool { return a.c.Status().State == "online" && b.c.Status().State == "online" }, "не подключились")
	bIP := b.ip()
	waitLong(t, 15*time.Second, func() bool { return peerStatus(a.c, bIP).Direct }, "прямое соединение не установилось")
	port := echoService(t, "b")

	kill()
	waitLong(t, 10*time.Second, func() bool { return a.c.Status().State == "local" }, "нет состояния local")
	st := a.c.Status()
	if st.Error == "" {
		t.Fatal("без сервера пропал текст ошибки")
	}
	if p := peerStatus(a.c, bIP); !p.Online || !p.Direct {
		t.Fatalf("устройство с прямым путём не на связи: %+v", p)
	}
	if _, ok := a.c.Resolve("b.mesh"); !ok {
		t.Fatal("имя не отвечает без сервера")
	}
	ips, ours := StaticDNS(a.c, "b.mesh.")
	if !ours || len(ips) != 1 || ips[0].String() != bIP {
		t.Fatalf("StaticDNS без сервера: %v %v", ips, ours)
	}
	for i := 0; i < 3; i++ {
		if got := dialAsk(t, a.c, "b.mesh:"+strconv.Itoa(port), "x"); got != "b: x" {
			t.Fatalf("без сервера ответили %q", got)
		}
	}
}

// Без сервера к устройству, до которого прямого пути нет, — быстрый и
// понятный отказ, а не 20 секунд ожидания.
func TestБезСервераНетПутиБыстрыйОтказ(t *testing.T) {
	addr, _, _, kill := startMeshdSTUNKill(t)
	key := NewKey()
	a := newNode(t, addr, Config{Key: key, Name: "a", AllowIncoming: true})
	// У b прямые выключены: путь к нему только через сервер.
	b := newNode(t, addr, Config{Key: key, Name: "b", AllowIncoming: true, NoDirect: true})
	a.start()
	b.start()
	waitFor(t, func() bool { _, ok := a.c.Resolve("b"); return ok && b.c.Status().State == "online" }, "a не увидел b")
	kill()
	waitLong(t, 10*time.Second, func() bool { return a.c.Status().State == "error" }, "a не заметил, что сервера нет")
	start := time.Now()
	_, err := a.c.DialPeer("b.mesh:22")
	if err == nil || !strings.Contains(err.Error(), "нет связи с сервером") || !strings.Contains(err.Error(), "b.mesh") {
		t.Fatalf("ожидался понятный отказ, получено %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("отказ занял %v", d)
	}
}

// Ключ постоянный, список на диске; одно устройство перезапустилось при
// мёртвом сервере — они находят друг друга по запомненным адресам.
func TestБезСервераПерезапускПоЗапомненнымАдресам(t *testing.T) {
	addr, _, _, kill := startMeshdSTUNKill(t)
	key := NewKey()
	a := newNode(t, addr, Config{Key: key, Name: "a", AllowIncoming: true, StateDir: t.TempDir()})
	b := newNode(t, addr, Config{Key: key, Name: "b", AllowIncoming: true, StateDir: t.TempDir()})
	a.start()
	b.start()
	waitFor(t, func() bool { return a.c.Status().State == "online" && b.c.Status().State == "online" }, "не подключились")
	aIP, bIP := a.ip(), b.ip()
	waitLong(t, 15*time.Second, func() bool { return peerStatus(a.c, bIP).Direct && peerStatus(b.c, aIP).Direct },
		"прямое соединение не установилось")
	bFP := b.c.direct().fp
	// Список с отпечатками уже на диске.
	waitFor(t, func() bool {
		pr, ok := loadCache(b.c.stateDir()).peer(aIP)
		return ok && pr.FP != "" && len(pr.Cands) > 0
	}, "список устройств не записался")
	port := echoService(t, "a")
	bport := echoService(t, "b")

	kill()
	b.stop()
	b.start()
	waitLong(t, 10*time.Second, func() bool { return b.c.Status().State == "error" || b.c.Status().State == "local" },
		"b не понял, что сервера нет")
	if b.c.direct().fp != bFP {
		t.Fatal("после перезапуска другой ключ")
	}
	if b.ip() != bIP {
		t.Fatalf("после перезапуска свой адрес %q, был %s", b.ip(), bIP)
	}
	waitLong(t, 15*time.Second, func() bool { return peerStatus(b.c, aIP).Direct && peerStatus(a.c, bIP).Direct },
		"после перезапуска без сервера не нашли друг друга")
	if got := dialAsk(t, b.c, "a.mesh:"+strconv.Itoa(port), "y"); got != "a: y" {
		t.Fatalf("b→a: %q", got)
	}
	if got := dialAsk(t, a.c, "b.mesh:"+strconv.Itoa(bport), "z"); got != "b: z" {
		t.Fatalf("a→b: %q", got)
	}
}

// То же, но адресов нет (другая сеть Wi-Fi, новый порт) — находит маяк в
// локальной сети.
func TestБезСервераПерезапускПоМаяку(t *testing.T) {
	addr, _, _, kill := startMeshdSTUNKill(t)
	key := NewKey()
	a := newNode(t, addr, Config{Key: key, Name: "a", AllowIncoming: true, StateDir: t.TempDir()})
	b := newNode(t, addr, Config{Key: key, Name: "b", AllowIncoming: true, StateDir: t.TempDir()})
	// «Локальная сеть» — порт на 127.0.0.1: маяки b доходят до приёмника a.
	ln, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a.listen = func() (net.PacketConn, error) { return ln, nil }
	b.targets = func() []*net.UDPAddr { return []*net.UDPAddr{ln.LocalAddr().(*net.UDPAddr)} }
	a.start()
	b.start()
	waitFor(t, func() bool { return a.c.Status().State == "online" && b.c.Status().State == "online" }, "не подключились")
	aIP, bIP := a.ip(), b.ip()
	waitLong(t, 15*time.Second, func() bool { return peerStatus(a.c, bIP).Direct && peerStatus(b.c, aIP).Direct },
		"прямое соединение не установилось")
	waitFor(t, func() bool { pr, ok := loadCache(b.c.stateDir()).peer(aIP); return ok && pr.FP != "" }, "список не записался")

	kill()
	b.stop()
	// Стереть запомненные адреса у b: искать только маяком.
	path := filepath.Join(b.c.stateDir(), "peers.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f cacheFile
	json.Unmarshal(raw, &f)
	for i := range f.Peers {
		f.Peers[i].Cands = nil
	}
	raw, _ = json.Marshal(f)
	os.WriteFile(path, raw, 0o600)

	b.start()
	waitLong(t, 15*time.Second, func() bool { return peerStatus(b.c, aIP).Direct && peerStatus(a.c, bIP).Direct },
		"маяк не помог найти друг друга")
	port := echoService(t, "a")
	if got := dialAsk(t, b.c, aIP+":"+strconv.Itoa(port), "m"); got != "a: m" {
		t.Fatalf("после маяка: %q", got)
	}
}

// Маяк с чужой подписью, устаревший или повторённый — не принимается.
func TestМаякПодписьИСвежесть(t *testing.T) {
	key := beaconKeyFor(NewKey())
	now := time.Now()
	b := beacon{V: 1, Net: "n", ID: "d", IP: "198.19.0.5", Port: 1, FP: "f", T: now.UnixMilli()}
	if _, err := decodeBeacon(key, encodeBeacon(key, b), now); err != nil {
		t.Fatalf("свой маяк отвергнут: %v", err)
	}
	if _, err := decodeBeacon(beaconKeyFor(NewKey()), encodeBeacon(key, b), now); err == nil {
		t.Fatal("маяк чужой сети принят")
	}
	raw := encodeBeacon(key, b)
	raw[10] ^= 1
	if _, err := decodeBeacon(key, raw, now); err == nil {
		t.Fatal("подделанный маяк принят")
	}
	old := b
	old.T = now.Add(-3 * time.Minute).UnixMilli()
	if _, err := decodeBeacon(key, encodeBeacon(key, old), now); err == nil {
		t.Fatal("устаревший маяк принят")
	}
	future := b
	future.T = now.Add(3 * time.Minute).UnixMilli()
	if _, err := decodeBeacon(key, encodeBeacon(key, future), now); err == nil {
		t.Fatal("маяк из будущего принят")
	}
	if _, err := decodeBeacon(key, []byte{0, 'b', 'c', 'n'}, now); err == nil {
		t.Fatal("обрывок принят")
	}
	p := &p2p{beaconT: map[string]int64{}}
	if !p.fresh(b.IP, b.T) || p.fresh(b.IP, b.T) || p.fresh(b.IP, b.T-1) || !p.fresh(b.IP, b.T+1) {
		t.Fatal("повтор маяка не отсекается")
	}
}

// Маяк от устройства, которого нет в последнем списке meshd (убрано), или с
// другим отпечатком, — не ведёт к соединению.
func TestМаякТолькоОтИзвестных(t *testing.T) {
	c := New(Config{Key: NewKey(), DeviceID: NewDeviceID(), Via: "127.0.0.1"}, directDial, nil)
	ln, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	c.beaconListen = func() (net.PacketConn, error) { return ln, nil }
	c.status.Self.IP = netip.MustParseAddr("198.19.0.9")
	c.cache.setList(cachedPeer{IP: "198.19.0.9"}, []cachedPeer{{IP: "198.19.0.5", Host: "x"}})
	c.cache.worked("198.19.0.5", "fp-x", "")
	cert, fp, _ := p2pCert()
	p, err := startP2P(context.Background(), c, cert, fp)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	send := func(b beacon) {
		s, _ := net.ListenPacket("udp4", "127.0.0.1:0")
		defer s.Close()
		s.WriteTo(encodeBeacon(p.bkey, b), ln.LocalAddr())
	}
	now := time.Now().UnixMilli()
	send(beacon{V: 1, Net: p.netID, IP: "198.19.0.7", FP: "fp-x", T: now}) // нет в списке
	send(beacon{V: 1, Net: p.netID, IP: "198.19.0.5", FP: "fp-y", T: now}) // не тот ключ
	send(beacon{V: 1, Net: "other", IP: "198.19.0.5", FP: "fp-x", T: now}) // чужая сеть
	time.Sleep(300 * time.Millisecond)
	p.mu.Lock()
	n := len(p.beaconT)
	p.mu.Unlock()
	if n != 0 {
		t.Fatal("принят маяк от чужого устройства")
	}
	send(beacon{V: 1, Net: p.netID, IP: "198.19.0.5", FP: "fp-x", T: now})
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.beaconT["198.19.0.5"] == now }, "свой маяк не принят")
}

// Устройство старой версии (ключ на каждый запуск, без StateDir) работает с
// новым: через сервер и напрямую.
func TestСтароеУстройствоБезПостоянногоКлюча(t *testing.T) {
	addr, _, _ := startMeshdSTUN(t)
	key := NewKey()
	a := newNode(t, addr, Config{Key: key, Name: "a", AllowIncoming: true, StateDir: t.TempDir()})
	old := newNode(t, addr, Config{Key: key, Name: "old", AllowIncoming: true})
	a.start()
	old.start()
	waitFor(t, func() bool { _, ok := a.c.Resolve("old"); return ok && old.c.Status().State == "online" }, "не видят друг друга")
	port := echoService(t, "old")
	if got := dialAsk(t, a.c, "old.mesh:"+strconv.Itoa(port), "1"); got != "old: 1" {
		t.Fatalf("через сервер: %q", got)
	}
	oIP := old.ip()
	waitLong(t, 15*time.Second, func() bool { return peerStatus(a.c, oIP).Direct }, "напрямую не вышло")
	if got := dialAsk(t, a.c, "old.mesh:"+strconv.Itoa(port), "2"); got != "old: 2" {
		t.Fatalf("напрямую: %q", got)
	}
	if old.c.stateDir() != "" {
		t.Fatal("у старого есть StateDir")
	}
}

// Ключ создаётся один раз и потом тот же; файлы — 0600 и без хвостов.
func TestКлючИСписокНаДиске(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "net")
	_, fp1, err := loadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, fp2, _ := loadIdentity(dir)
	if fp1 != fp2 || fp1 == "" {
		t.Fatalf("отпечаток сменился: %s → %s", fp1, fp2)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(dir, "key")); st.Mode().Perm() != 0o600 {
			t.Fatalf("права ключа %v", st.Mode().Perm())
		}
	}
	// Испорченный файл — новый ключ, а не отказ.
	os.WriteFile(filepath.Join(dir, "key"), []byte("мусор"), 0o600)
	if _, fp3, err := loadIdentity(dir); err != nil || fp3 == fp1 {
		t.Fatalf("испорченный ключ: %v %s", err, fp3)
	}

	pc := loadCache(dir)
	pc.setList(cachedPeer{IP: "198.19.0.1", Host: "me"}, []cachedPeer{{IP: "198.19.0.2", Host: "a"}, {IP: "198.19.0.3", Host: "b"}})
	pc.worked("198.19.0.2", "fpa", "192.168.1.5:4000")
	pc.worked("198.19.0.9", "fpz", "1.2.3.4:5") // не из списка — не запоминается
	if err := pc.save(); err != nil {
		t.Fatal(err)
	}
	back := loadCache(dir)
	if pr, ok := back.peer("198.19.0.2"); !ok || pr.FP != "fpa" || pr.Cands[0] != "192.168.1.5:4000" {
		t.Fatalf("после чтения: %+v", pr)
	}
	if back.byFP("fpz") != "" {
		t.Fatal("запомнилось устройство не из списка")
	}
	// Убрали b — его нет, а отпечаток a сохранился.
	removed := back.setList(cachedPeer{IP: "198.19.0.1"}, []cachedPeer{{IP: "198.19.0.2", Host: "a"}})
	if len(removed) != 1 || removed[0] != "198.19.0.3" {
		t.Fatalf("убранные: %v", removed)
	}
	if back.byFP("fpa") != "198.19.0.2" {
		t.Fatal("отпечаток пропал при обновлении списка")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("остался временный файл %s", e.Name())
		}
	}
}

// После Run не остаётся горутин: ни сессий, ни прямых соединений, ни маяков.
func TestRunБезУтечекГорутин(t *testing.T) {
	addr, _, _, kill := startMeshdSTUNKill(t)
	time.Sleep(200 * time.Millisecond)
	before := runtime.NumGoroutine()
	key := NewKey()
	var nodes []*node
	for _, name := range []string{"a", "b"} {
		n := newNode(t, addr, Config{Key: key, Name: name, AllowIncoming: true, StateDir: t.TempDir()})
		ln, _ := net.ListenPacket("udp4", "127.0.0.1:0")
		n.listen = func() (net.PacketConn, error) { return ln, nil }
		n.start()
		nodes = append(nodes, n)
	}
	a, b := nodes[0], nodes[1]
	waitFor(t, func() bool { return a.c.Status().State == "online" && b.c.Status().State == "online" }, "не подключились")
	bIP := b.ip()
	waitLong(t, 15*time.Second, func() bool { return peerStatus(a.c, bIP).Direct }, "прямое соединение не установилось")
	kill() // и без сервера (маяки идут) тоже
	waitLong(t, 10*time.Second, func() bool { return a.c.Status().State == "local" }, "нет local")
	a.stop()
	b.stop()
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		buf := make([]byte, 1<<20)
		t.Fatalf("горутин было %d, стало %d:\n%s", before, n, buf[:runtime.Stack(buf, true)])
	}
}
