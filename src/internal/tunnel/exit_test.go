package tunnel

// Тесты цепочки выхода (ТЗ 8): устройство → А → Б → интернет. Сервер А в
// тестах отказывает в канале к «интернету» (rejectTo) и ведёт счёт целям
// (saw): так видно, что через А в интернет не ушло ни одного соединения.
// Перед А и Б стоят «ворота» (gate) — ими сервер «падает» и «поднимается».

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/knownhosts"

	"sshtunnel/internal/core"
	"sshtunnel/internal/events"
	"sshtunnel/internal/hostkey"
)

type chainLab struct {
	tun          *Tunnel
	a, b         *testSSHServer
	gateA, gateB *gate
	echo         string // «интернет»
	other        string // ещё одна цель — для пути сети устройств через А
	dir          string
	bKey         string
}

// fastChain сжимает паузы цепочки до долей секунды.
func fastChain(t *testing.T) {
	t.Helper()
	oldMin, oldMax, oldWait := exitRetryMin, exitRetryMax, dialWait
	exitRetryMin, exitRetryMax, dialWait = 30*time.Millisecond, 150*time.Millisecond, 1500*time.Millisecond
	t.Cleanup(func() { exitRetryMin, exitRetryMax, dialWait = oldMin, oldMax, oldWait })
}

func newChainLab(t *testing.T, bOpen bool, mod func(*Config)) *chainLab {
	t.Helper()
	fastChain(t)
	l := &chainLab{dir: t.TempDir()}
	l.echo = echoServer(t).String()
	l.other = echoServer(t).String()

	aKey, aPub := writeTestKey(t, l.dir)
	bDir := filepath.Join(l.dir, "b")
	os.MkdirAll(bDir, 0o700)
	bKey, bPub := writeTestKey(t, bDir) // у Б свой ключ — из его профиля
	l.bKey = bKey
	l.a = newTestSSHServer(t, aPub)
	l.a.rejectTo = l.echo
	l.b = newTestSSHServer(t, bPub)
	l.gateA = newGate(t, l.a.addr, true)
	l.gateB = newGate(t, l.b.addr, bOpen)

	cfg := Config{
		Name:           "Франкфурт",
		Host:           "127.0.0.1",
		SSHPort:        l.gateA.port(),
		User:           "test",
		KeyPath:        aKey,
		PoolSize:       2,
		KeepAlive:      200 * time.Millisecond,
		KnownHostsPath: filepath.Join(l.dir, "known_hosts"),
		LocalViaTunnel: true,
		Exit: []Hop{{
			Name: "Лондон", Host: "127.0.0.1", SSHPort: l.gateB.port(), User: "test", KeyPath: bKey,
		}},
	}
	if mod != nil {
		mod(&cfg)
	}
	l.tun = New(cfg, events.NewBus())
	if err := l.tun.Start(); err != nil {
		t.Fatalf("туннель не запустился: %v", err)
	}
	t.Cleanup(l.tun.Stop)
	return l
}

func (l *chainLab) exitUp() bool { return l.tun.ExitStatus().Up }

// fetch — GET /hello через tun.Dial (путь «через сервер»).
func (l *chainLab) fetch() (string, error) {
	c, err := l.tun.Dial("tcp", l.echo)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET /hello HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func mustExitDown(t *testing.T, err error) *ExitDownError {
	t.Helper()
	var ed *ExitDownError
	if !errors.As(err, &ed) {
		t.Fatalf("ждал ExitDownError (интернет выключен цепочкой), получил %v", err)
	}
	return ed
}

func TestЦепочкаВыходитЧерезПоследнийСервер(t *testing.T) {
	l := newChainLab(t, true, nil)
	waitFor(t, 5*time.Second, l.exitUp)
	body, err := l.fetch()
	if err != nil || !strings.Contains(body, "привет") {
		t.Fatalf("через цепочку не дошло: %q %v", body, err)
	}
	if !l.b.saw(l.echo) {
		t.Fatal("соединение не прошло через Б")
	}
	if l.a.saw(l.echo) {
		t.Fatal("соединение в интернет открывалось через А")
	}
	if !l.a.saw(l.b.addr) && !l.a.saw("127.0.0.1:"+strconv.Itoa(l.gateB.port())) {
		t.Fatal("до Б шли не через А")
	}
	st := l.tun.ExitStatus()
	if strings.Join(st.Path, " → ") != "Франкфурт → Лондон" {
		t.Fatalf("путь %v", st.Path)
	}
	// Ядро по-прежнему говорит «через сервер» — цепочка не решает «куда».
	if r, _ := l.tun.decide("", l.echo); r != core.RouteServer {
		t.Fatalf("ядро решило %v", r)
	}
}

func TestЦепочкаБезБПриСтартеНетИнтернета(t *testing.T) {
	l := newChainLab(t, false, nil)
	_, err := l.fetch()
	ed := mustExitDown(t, err)
	if ed.Name != "Лондон" {
		t.Fatalf("упавший сервер %q", ed.Name)
	}
	if l.a.saw(l.echo) {
		t.Fatal("без Б соединение пошло через А")
	}
	if st := l.tun.ExitStatus(); st.Up || st.Err == nil {
		t.Fatalf("состояние без Б: %+v", st)
	}
	if !l.tun.ServerUp() {
		t.Fatal("А должен быть на связи")
	}
}

func TestЦепочкаБПадаетИВозвращается(t *testing.T) {
	l := newChainLab(t, true, nil)
	waitFor(t, 5*time.Second, l.exitUp)
	if _, err := l.fetch(); err != nil {
		t.Fatal(err)
	}
	l.gateB.set(false) // Б упал посреди сессии
	waitFor(t, 5*time.Second, func() bool { return !l.exitUp() })
	_, err := l.fetch()
	mustExitDown(t, err)
	if l.a.saw(l.echo) {
		t.Fatal("после падения Б соединение пошло через А")
	}
	l.gateB.set(true) // вернулся — интернет возвращается сам
	waitFor(t, 5*time.Second, l.exitUp)
	if _, err := l.fetch(); err != nil {
		t.Fatalf("после возврата Б: %v", err)
	}
}

func TestЦепочкаПадениеАРоняетВсё(t *testing.T) {
	l := newChainLab(t, true, nil)
	waitFor(t, 5*time.Second, l.exitUp)
	l.gateA.set(false)
	waitFor(t, 5*time.Second, func() bool { return !l.exitUp() && !l.tun.ServerUp() })
	_, err := l.fetch()
	mustExitDown(t, err)
	if l.tun.ExitStatus().Err == nil {
		t.Fatal("без А нет причины в состоянии цепочки")
	}
	l.gateA.set(true) // пул переподключается — цепочка за ним
	waitFor(t, 10*time.Second, l.exitUp)
	if _, err := l.fetch(); err != nil {
		t.Fatal(err)
	}
}

func TestЦепочкаПослеKickИСливаПоднимаетсяСама(t *testing.T) {
	l := newChainLab(t, true, nil)
	waitFor(t, 5*time.Second, l.exitUp)
	l.tun.Kick()
	waitFor(t, 5*time.Second, l.exitUp)
	if _, err := l.fetch(); err != nil {
		t.Fatalf("после Kick: %v", err)
	}
	l.tun.Drain()
	if l.exitUp() {
		t.Fatal("в сливе цепочка жива")
	}
	if err := l.tun.Resume(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, l.exitUp)
	if _, err := l.fetch(); err != nil {
		t.Fatalf("после Resume: %v", err)
	}
	if l.a.saw(l.echo) {
		t.Fatal("соединение пошло через А")
	}
}

func TestЦепочкаDNSТолькоЧерезПоследний(t *testing.T) {
	dns, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dns.Close()
	go func() {
		for {
			c, err := dns.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	old := serverResolvers
	serverResolvers = []string{dns.Addr().String()}
	defer func() { serverResolvers = old }()

	l := newChainLab(t, false, nil)
	// Б нет — имя не разрешается вовсе, и запрос не уходит через А.
	if _, err := l.tun.LookupViaServer("example.com"); err == nil {
		t.Fatal("без Б имя разрешилось")
	}
	if l.a.saw(dns.Addr().String()) {
		t.Fatal("DNS-запрос ушёл через А")
	}
	l.gateB.set(true)
	waitFor(t, 5*time.Second, l.exitUp)
	l.tun.LookupViaServer("example.com") // ответа нет (резолвер закрывает), важно — куда пошёл
	if !l.b.saw(dns.Addr().String()) || l.a.saw(dns.Addr().String()) {
		t.Fatalf("DNS через сервер шёл не через Б: Б=%v А=%v", l.b.saw(dns.Addr().String()), l.a.saw(dns.Addr().String()))
	}
	// Решение DNS по-прежнему у ядра: имя «через сервер».
	if core.DecideDNS(l.tun.Rules(), "example.com") != core.DNSServer {
		t.Fatal("ядро отвечает на имя не через сервер")
	}
}

func TestЦепочкаUDPЧерезРетрансляторПоследнего(t *testing.T) {
	relay, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	go func() {
		for {
			c, err := relay.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, c)
		}
	}()
	// У А ретранслятор есть, у Б — нет: UDP отказом, через А не идёт.
	l := newChainLab(t, true, func(c *Config) {
		c.UDPRelayEnabled = true
		c.UDPRelayAddr = relay.Addr().String()
	})
	waitFor(t, 5*time.Second, l.exitUp)
	if l.tun.UDPRelay() != nil {
		t.Fatal("UDP пошёл без ретранслятора на Б")
	}
	if l.a.saw(relay.Addr().String()) {
		t.Fatal("UDP пошёл к ретранслятору А")
	}

	l2 := newChainLab(t, true, func(c *Config) {
		c.UDPRelayAddr = relay.Addr().String()
		c.Exit[0].UDPRelay = true
	})
	waitFor(t, 5*time.Second, l2.exitUp)
	if l2.tun.UDPRelay() == nil {
		t.Fatal("ретранслятор Б не поднялся")
	}
	if !l2.b.saw(relay.Addr().String()) || l2.a.saw(relay.Addr().String()) {
		t.Fatal("UDP шёл не через ретранслятор Б")
	}
}

func TestЦепочкаСетьУстройствИдётЧерезАБезБ(t *testing.T) {
	l := newChainLab(t, false, nil)
	// Шов ServerDial клиента сети устройств — пул А, цепочка ему не нужна.
	c, err := l.tun.serverDial("tcp", l.other)
	if err != nil {
		t.Fatalf("сеть устройств без Б: %v", err)
	}
	c.Close()
	if !l.a.saw(l.other) {
		t.Fatal("путь сети устройств шёл не через А")
	}
	// И служебное соединение с самим А — тоже мимо цепочки.
	c, err = l.tun.DialEntry("tcp", l.other)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestЦепочкаСменаКлючаБСообщаетсяПонятно(t *testing.T) {
	fastChain(t)
	// Заранее «запомнен» другой ключ для адреса Б.
	dir := t.TempDir()
	bKey, bPub := writeTestKey(t, dir)
	b := newTestSSHServer(t, bPub)
	gateB := newGate(t, b.addr, true)
	_, otherPub := writeTestKey(t, t.TempDir())
	kh := filepath.Join(dir, "known_hosts")
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(gateB.port()))
	os.WriteFile(kh, []byte(knownhosts.Line([]string{knownhosts.Normalize(addr)}, otherPub)+"\n"), 0o600)

	aDir := filepath.Join(dir, "a")
	os.MkdirAll(aDir, 0o700)
	aKey, aPub := writeTestKey(t, aDir)
	a := newTestSSHServer(t, aPub)
	host, port, _ := net.SplitHostPort(a.addr)
	p, _ := strconv.Atoi(port)
	tun := New(Config{
		Name: "А", Host: host, SSHPort: p, User: "test", KeyPath: aKey, PoolSize: 1,
		KnownHostsPath: kh, LocalViaTunnel: true,
		Exit: []Hop{{Name: "Б", Host: "127.0.0.1", SSHPort: gateB.port(), User: "test", KeyPath: bKey}},
	}, events.NewBus())
	if err := tun.Start(); err != nil {
		t.Fatal(err)
	}
	defer tun.Stop()
	waitFor(t, 5*time.Second, func() bool { return tun.ExitStatus().Err != nil })
	var changed *hostkey.ErrChanged
	if !errors.As(tun.ExitStatus().Err, &changed) {
		t.Fatalf("ждал смену ключа Б, получил %v", tun.ExitStatus().Err)
	}
	if b.channels.Load() != 0 || tun.ExitStatus().Up {
		t.Fatal("с подменённым ключом цепочка поднялась")
	}
}

func TestЦепочкаБезКлючаБНеСтартует(t *testing.T) {
	dir := t.TempDir()
	aKey, aPub := writeTestKey(t, dir)
	a := newTestSSHServer(t, aPub)
	host, port, _ := net.SplitHostPort(a.addr)
	p, _ := strconv.Atoi(port)
	tun := New(Config{
		Host: host, SSHPort: p, User: "test", KeyPath: aKey, PoolSize: 1,
		KnownHostsPath: filepath.Join(dir, "known_hosts"),
		Exit:           []Hop{{Name: "Б", Host: "127.0.0.1", SSHPort: 22, User: "x", KeyPath: filepath.Join(dir, "нет")}},
	}, events.NewBus())
	if err := tun.Start(); err == nil {
		tun.Stop()
		t.Fatal("туннель стартовал без ключа сервера цепочки")
	}
}

func TestHostkeyByNameЗапоминаетТолькоИмя(t *testing.T) {
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	_, k1 := writeTestKey(t, dir)
	_, k2 := writeTestKey(t, t.TempDir())
	cb := hostkey.CallbackByName(kh, nil)
	zero := &net.TCPAddr{IP: net.IPv4zero}
	if err := cb("b.example:22", zero, k1); err != nil {
		t.Fatal(err)
	}
	// Другой сервер с другим ключом за тем же «нулевым» адресом — не смена.
	if err := cb("c.example:2222", zero, k2); err != nil {
		t.Fatalf("второй сервер принят за смену ключа: %v", err)
	}
	if err := cb("b.example:22", zero, k1); err != nil {
		t.Fatal(err)
	}
	var changed *hostkey.ErrChanged
	if err := cb("b.example:22", zero, k2); !errors.As(err, &changed) {
		t.Fatalf("смена ключа не замечена: %v", err)
	}
	data, _ := os.ReadFile(kh)
	if strings.Contains(string(data), "0.0.0.0") {
		t.Fatal("в known_hosts записан нулевой адрес")
	}
}
