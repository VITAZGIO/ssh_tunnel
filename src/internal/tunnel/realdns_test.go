package tunnel

import (
	"context"
	"crypto/tls"

	"net"
	"sync/atomic"
	"testing"
	"time"

	"sshtunnel/internal/core"
	"sshtunnel/internal/events"
	"sshtunnel/internal/routing"
)

// Ответ через сервер запоминается под имя: соединение придёт с адресом, а
// «всегда напрямую» записано именем.
func TestLookupViaServerLearnsNames(t *testing.T) {
	var calls atomic.Int32
	old := lookupViaServer
	lookupViaServer = func(_ context.Context, _ *Tunnel, name string) ([]net.IP, error) {
		calls.Add(1)
		return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("198.18.128.5"), net.ParseIP("2001:db8::1")}, nil
	}
	defer func() { lookupViaServer = old }()

	tun := New(Config{Direct: routing.NewDirectList([]string{".direct.example"})}, events.NewBus())
	ips, err := tun.LookupViaServer("Site.Direct.Example.")
	if err != nil || len(ips) != 1 || ips[0].String() != "93.184.216.34" {
		t.Fatalf("адреса %v, %v: нужен только настоящий IPv4", ips, err)
	}
	if _, err := tun.LookupViaServer("site.direct.example"); err != nil || calls.Load() != 1 {
		t.Fatalf("второй запрос должен взяться из памяти: запросов %d, %v", calls.Load(), err)
	}
	if route, c := tun.decide("", "93.184.216.34:443"); route != core.RouteDirect || c.Name != "site.direct.example" {
		t.Errorf("адрес из ответа: %s, имя %q", route, c.Name)
	}
	if route, _ := tun.decide("", "1.2.3.4:443"); route != core.RouteServer {
		t.Errorf("незнакомый адрес: %s", route)
	}
	// Имя из самого соединения сильнее таблицы.
	if route, _ := tun.decideNamed("", "93.184.216.34:443", "other.example"); route != core.RouteServer {
		t.Errorf("SNI другого сайта на том же адресе: %s", route)
	}

	tun.ApplyHygiene(core.Hygiene{ResetFakeIP: true})
	tun.LookupViaServer("site.direct.example")
	if calls.Load() != 2 {
		t.Error("смена режима DNS должна забыть ответы")
	}
}

func TestSniffWanted(t *testing.T) {
	tun := New(Config{}, events.NewBus())
	if !tun.sniffWanted("1.2.3.4:443") {
		t.Error("без подставных адресов имя ищется")
	}
	for _, target := range []string{"1.2.3.4:22", "198.19.0.4:443", "bad"} {
		if tun.sniffWanted(target) {
			t.Errorf("%s: ждать нечего", target)
		}
	}
	tun.SetDNSMode(true, nil)
	if tun.sniffWanted("1.2.3.4:443") {
		t.Error("с подставными адресами имя приносит адрес")
	}
	tun.SetDNSMode(false, nil)
	tun.SetBypass(false)
	if tun.sniffWanted("1.2.3.4:443") {
		t.Error("без обхода всё напрямую, имя не нужно")
	}
}

// Прочитанное ради имени не теряется: приложение и сайт видят поток целиком.
func TestSniffConnKeepsBytes(t *testing.T) {
	app, us := net.Pipe()
	defer us.Close()
	var sent []byte
	got := make(chan []byte, 1)
	go func() {
		c := tls.Client(&recordConn{Conn: app, out: &sent}, &tls.Config{ServerName: "blocked.example", InsecureSkipVerify: true})
		c.SetDeadline(time.Now().Add(2 * time.Second))
		c.Handshake()
		got <- sent
	}()
	host, head := sniffConn(us, sniffWait)
	if host != "blocked.example" {
		t.Fatalf("имя %q", host)
	}
	app.Close()
	all := <-got
	if len(head) == 0 || len(head) > len(all) || string(head) != string(all[:len(head)]) {
		t.Fatal("прочитанное ради имени не совпадает с началом потока")
	}

	// Сервер говорит первым — приложение молчит, ждём не дольше sniffWait.
	a2, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	start := time.Now()
	if host, head := sniffConn(b2, sniffWait); host != "" || len(head) != 0 || time.Since(start) > 2*sniffWait {
		t.Errorf("молчащее соединение: %q, %d байт, %v", host, len(head), time.Since(start))
	}
}

// recordConn запоминает, что клиент отправил.
type recordConn struct {
	net.Conn
	out *[]byte
}

func (c *recordConn) Write(b []byte) (int, error) {
	*c.out = append(*c.out, b...)
	return c.Conn.Write(b)
}
