package share

import (
	"strings"
	"testing"
)

func TestLinkRoundTrip(t *testing.T) {
	in := Doc{
		Name: "Телефон", Host: "89.0.0.1", SSHPort: 22, User: "tun_abc",
		SocksPort: 5555, FilterMode: "apps", FilterApps: []string{"x.exe"},
		KeyIncluded: true, KeyContents: "-----BEGIN OPENSSH PRIVATE KEY-----\nAAA\n-----END OPENSSH PRIVATE KEY-----\n",
		Panel: "https://p.example:47823", ClientID: "c1", DeviceName: "Телефон",
	}
	link, err := BuildLink(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, LinkPrefix+"v=1&d=") {
		t.Fatalf("неожиданное начало ссылки: %s", link)
	}
	// Лишнего в ссылке нет: фильтр и порты прокси не переносятся.
	if strings.Contains(link, "x.exe") {
		t.Fatal("в ссылку попал список приложений")
	}
	out, err := Parse([]byte("  " + link + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Host != in.Host || out.SSHPort != 22 || out.User != in.User ||
		out.KeyContents != in.KeyContents || !out.KeyIncluded ||
		out.Panel != in.Panel || out.ClientID != in.ClientID ||
		out.DeviceName != in.DeviceName || out.Name != in.Name {
		t.Fatalf("после разбора не то: %+v", out)
	}
	if out.SocksPort != 1080 || out.HTTPPort != 1081 || out.PoolSize != 4 || out.FilterMode != "all" {
		t.Fatalf("значения по умолчанию не проставлены: %+v", out)
	}
}

func TestLinkPortAndCase(t *testing.T) {
	link, err := BuildLink(Doc{Host: "h", SSHPort: 2222, User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse([]byte(strings.Replace(link, "sshtunnel://", "SSHTUNNEL://", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if out.SSHPort != 2222 || out.KeyIncluded || out.Name != "h" {
		t.Fatalf("после разбора не то: %+v", out)
	}
}

func TestLinkBad(t *testing.T) {
	for _, s := range []string{
		"sshtunnel://import?v=1&d=%%%",
		"sshtunnel://import?v=1",
		"sshtunnel://import?v=9&d=e30",
		"sshtunnel://other?v=1&d=e30",
		"sshtunnel://import?v=1&d=e30", // {} — нет адреса
	} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("ожидалась ошибка для %q", s)
		}
	}
	if _, err := BuildLink(Doc{}); err == nil {
		t.Error("ожидалась ошибка без адреса")
	}
}
