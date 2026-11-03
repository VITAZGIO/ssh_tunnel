package mesh

import (
	"bufio"
	"net"
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
	if from := <-got; from.IP != a.Status().Self.IP.String() || from.Host != "a" {
		t.Fatalf("звонящий: %+v", from)
	}
	// Сам к себе — тоже со своим адресом.
	c, err = b.DialPeer(net.JoinHostPort(b.Status().Self.IP.String(), strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	read(c)
	if from := <-got; from.IP != b.Status().Self.IP.String() {
		t.Fatalf("сам к себе: %+v", from)
	}
}
