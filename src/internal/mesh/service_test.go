package mesh

import (
	"bufio"
	"net"
	"net/netip"
	"strconv"
	"testing"
)

// Служба внутри программы получает адрес того, кто звонит (его ставит
// meshd), и работает без «Пускать другие устройства к моим службам».
func TestСлужбаЗнаетЗвонящего(t *testing.T) {
	addr, _, _ := startMeshdSTUN(t)
	key := NewKey()
	a, _ := startClientLog(t, addr, Config{Key: key, Name: "a", Via: "127.0.0.1", NoDirect: true})
	b, _ := startClientLog(t, addr, Config{Key: key, Name: "b", Via: "127.0.0.1", NoDirect: true})
	const port = 47999
	got := make(chan Caller, 2)
	Serve(port, func(c net.Conn, from Caller) {
		got <- from
		c.Write([]byte("ok\n"))
		c.Close()
	})
	t.Cleanup(func() { Serve(port, nil) })

	read := func(c net.Conn) string {
		line, _ := bufio.NewReader(c).ReadString('\n')
		c.Close()
		return line
	}
	c, err := a.DialPeer(net.JoinHostPort(b.Status().Self.IP.String(), strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	if read(c) != "ok\n" {
		t.Fatal("служба не ответила")
	}
	if from := <-got; from.IP != a.Status().Self.IP.String() || from.Host != "a" ||
		from.ID == "" || from.ID != PeerID(a.cfg.DeviceID) {
		t.Fatalf("звонящий: %+v", from)
	}
	// Сам к себе — тоже со своим адресом.
	c, err = b.DialPeer(net.JoinHostPort(b.Status().Self.IP.String(), strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	read(c)
	if from := <-got; from.IP != b.Status().Self.IP.String() || from.ID != PeerID(b.cfg.DeviceID) {
		t.Fatalf("сам к себе: %+v", from)
	}
	// id устройств — в общем списке (новый meshd), и свой тоже.
	var aID string
	for _, p := range b.Status().Peers {
		if p.Host == "a" {
			aID = p.ID
		}
	}
	if aID != PeerID(a.cfg.DeviceID) || b.Status().Self.ID != PeerID(b.cfg.DeviceID) {
		t.Fatalf("id в списке: %q, свой %q", aID, b.Status().Self.ID)
	}
}

// Старый meshd не ставит fromId — id звонящего берётся из общего списка по
// адресу (его мог прислать другой, новый, сервер); нет в списке — пусто.
func TestIDЗвонящегоПоСписку(t *testing.T) {
	c := &Client{}
	ip := netip.MustParseAddr("198.19.0.7")
	c.status.Peers = []Peer{{ID: "abc", IP: ip, Host: "x"}}
	if c.idOf("198.19.0.7") != "abc" || c.idOf("198.19.0.8") != "" {
		t.Fatal("idOf")
	}
	// Тот же id, что в cmd/meshd (peerID) и nodeapi.PeerID.
	if got := PeerID("dev-1"); got != "436521497eb1603d3e022e80" {
		t.Fatalf("PeerID: %q", got)
	}
	if PeerID("") != "" || PeerID("a") == PeerID("b") {
		t.Fatal("PeerID")
	}
}
