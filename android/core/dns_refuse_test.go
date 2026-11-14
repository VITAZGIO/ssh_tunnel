package core

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func queryA(t *testing.T, name string) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	q, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func rcode(t *testing.T, reply []byte) dnsmessage.RCode {
	t.Helper()
	var p dnsmessage.Parser
	h, err := p.Start(reply)
	if err != nil {
		t.Fatal(err)
	}
	return h.RCode
}

// Refuse — отказ без адреса и без вопроса к резолверам; имена сети
// устройств (Static) отвечаются как обычно.
func TestDNSRefuse(t *testing.T) {
	asked := false
	d := &DNS{
		Direct: func(string) bool { return true },
		Local:  func(string) ([]net.IP, error) { asked = true; return []net.IP{net.IPv4(1, 2, 3, 4)}, nil },
		Refuse: func(string) bool { return true },
		Static: func(name string) ([]net.IP, bool) {
			if name == "pc.mesh" {
				return []net.IP{net.IPv4(198, 19, 0, 5)}, true
			}
			return nil, false
		},
	}
	r, err := d.Answer(queryA(t, "example.org"))
	if err != nil {
		t.Fatal(err)
	}
	if rcode(t, r) != dnsmessage.RCodeRefused || asked {
		t.Fatalf("ожидался отказ без вопроса к резолверу: %v, спрошен=%v", rcode(t, r), asked)
	}
	r, _ = d.Answer(queryA(t, "pc.mesh"))
	if rcode(t, r) != dnsmessage.RCodeSuccess {
		t.Fatalf("имя сети устройств: %v", rcode(t, r))
	}
}
