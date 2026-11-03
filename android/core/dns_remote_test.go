package core

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// answerA — адреса и TTL из ответа нашего DNS на запрос A.
func answerA(t *testing.T, d *DNS, name string) ([]string, uint32, dnsmessage.RCode) {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	q, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	reply, err := d.Answer(q)
	if err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(reply); err != nil {
		t.Fatal(err)
	}
	var ips []string
	var ttl uint32
	for _, a := range m.Answers {
		if r, ok := a.Body.(*dnsmessage.AResource); ok {
			ips = append(ips, net.IP(r.A[:]).String())
			ttl = a.Header.TTL
		}
	}
	return ips, ttl, m.RCode
}

// Имя «через сервер» без подставных адресов — настоящий адрес от сервера,
// с TTL настоящих ответов; остальное — как раньше.
func TestDNSRemoteRealAddress(t *testing.T) {
	pool, err := NewFakePool("198.18.128.0/17")
	if err != nil {
		t.Fatal(err)
	}
	asked := ""
	d := &DNS{
		Pool:   pool,
		Direct: func(name string) bool { return name == "nas.lan" },
		Local:  func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.10")}, nil },
		Remote: func(name string) bool { return name != "fake.example" },
		RemoteResolve: func(name string) ([]net.IP, error) {
			asked = name
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
	}
	if ips, ttl, _ := answerA(t, d, "example.com"); len(ips) != 1 || ips[0] != "93.184.216.34" || ttl != defaultRealTTL || asked != "example.com" {
		t.Errorf("через сервер: %v, TTL %d, спросили %q", ips, ttl, asked)
	}
	if ips, ttl, _ := answerA(t, d, "fake.example"); len(ips) != 1 || !pool.Contains(ips[0]) || ttl != defaultTTL {
		t.Errorf("подставной: %v, TTL %d", ips, ttl)
	}
	if ips, _, _ := answerA(t, d, "nas.lan"); len(ips) != 1 || ips[0] != "192.168.1.10" {
		t.Errorf("напрямую: %v", ips)
	}

	d.RemoteResolve = nil
	if _, _, rc := answerA(t, d, "example.com"); rc != dnsmessage.RCodeServerFailure {
		t.Errorf("без резолвера через сервер — отказ, а не подставной адрес: %v", rc)
	}
}
