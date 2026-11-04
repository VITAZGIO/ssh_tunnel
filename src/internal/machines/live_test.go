package machines

import (
	"os"
	"testing"
)

// NETDATA=http://192.168.1.201:19999 go test -run Live -v
func TestLiveNetdata(t *testing.T) {
	u := os.Getenv("NETDATA")
	if u == "" {
		t.Skip("NETDATA не задан")
	}
	r, err := readNetdata(Machine{URL: u})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", r)
	if r.MemTotal == 0 || r.Uptime == 0 {
		t.Fatal("пустые показания")
	}
}
