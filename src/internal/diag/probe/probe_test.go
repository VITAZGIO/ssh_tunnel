package probe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestPublicАдреса(t *testing.T) {
	for s, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2a00:1450::1": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.0.1": false, "169.254.1.1": false,
		"100.64.0.1": false, "198.19.0.1": false, "198.18.0.1": false, "0.0.0.0": false, "224.0.0.1": false,
		"::1": false, "fd00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false, "255.255.255.255": false,
	} {
		if got := Public(netip.MustParseAddr(s)); got != want {
			t.Errorf("Public(%s) = %v", s, got)
		}
	}
}

func TestСерверОтвергаетДоСети(t *testing.T) {
	ctx := context.Background()
	for _, site := range []string{"localhost", "router", "nas.local", "pc.mesh", "x.home.arpa", "127.0.0.1", "[fd00::1]", "10.0.0.1:443"} {
		r, err := CheckFromServer(ctx, site, "ru", ServerOptions{})
		if err != nil || r.Verdict != "server_refused" {
			t.Fatalf("%s: %v %+v", site, err, r)
		}
	}
	if _, err := CheckFromServer(ctx, "example.com:8080", "ru", ServerOptions{}); !errors.Is(err, ErrPort) {
		t.Fatalf("порт 8080: %v", err)
	}
	if ServerPhrase("server_fail", "en") == ServerPhrase("server_fail", "ru") {
		t.Fatal("нет английской фразы")
	}
}

// fakeDNS — DNS, который на любой вопрос A отвечает адресом ip.
func fakeDNS(t *testing.T, ip [4]byte) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			if n < 12 {
				continue
			}
			// Конец вопроса: имя (метки) + тип и класс.
			i := 12
			for i < n && q[i] != 0 {
				i += int(q[i]) + 1
			}
			i += 5
			if i > n {
				continue
			}
			qtype := uint16(q[i-4])<<8 | uint16(q[i-3])
			resp := append([]byte{}, q[:i]...)
			resp[2], resp[3] = 0x81, 0x80 // ответ, рекурсия
			if qtype == 1 {
				resp[6], resp[7] = 0, 1 // один ответ
				resp = append(resp, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, ip[0], ip[1], ip[2], ip[3])
			} else {
				resp[6], resp[7] = 0, 0
			}
			resp[8], resp[9], resp[10], resp[11] = 0, 0, 0, 0
			pc.WriteTo(resp, addr)
		}
	}()
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
}

// Имя в интернете, которое разрешается в адрес своей сети, — отказ до
// соединения (так через сервер ходили бы в его внутреннюю сеть).
func TestСерверОтвергаетИмяВЛокальныйАдрес(t *testing.T) {
	dialed := false
	o := ServerOptions{Resolver: fakeDNS(t, [4]byte{10, 0, 0, 5}),
		Dial: func(context.Context, string, string) (net.Conn, error) { dialed = true; return nil, errors.New("нельзя") }}
	r, err := CheckFromServer(context.Background(), "intranet.example.com", "ru", o)
	if err != nil || r.Verdict != "server_refused" || dialed {
		t.Fatalf("имя → 10.0.0.5: %v %+v (соединялись: %v)", err, r, dialed)
	}
	// Публичный адрес — соединение идёт на него, а не на имя.
	var target string
	o = ServerOptions{Resolver: fakeDNS(t, [4]byte{93, 184, 216, 34}),
		Dial: func(_ context.Context, _, addr string) (net.Conn, error) { target = addr; return nil, errors.New("нет сети в тесте") }}
	r, _ = CheckFromServer(context.Background(), "example.com", "ru", o)
	if target != "93.184.216.34:443" || r.Verdict != "server_fail" {
		t.Fatalf("соединение на проверенный адрес: %q %+v", target, r)
	}
}
