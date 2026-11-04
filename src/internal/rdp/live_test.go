package rdp

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"sshtunnel/internal/addons"
)

// Живая проверка с настоящим guacd: RDP_RELAY=192.168.1.201:4822 go test -run Live.
func TestLiveRelay(t *testing.T) {
	relay := os.Getenv("RDP_RELAY")
	if relay == "" {
		t.Skip("RDP_RELAY не задан")
	}
	if err := checkRelay(relay); err != nil {
		t.Fatal(err)
	}
	addons.New(t.TempDir())
	_ = saveConf(conf{Relay: relay, Hosts: []Host{{ID: "x", Name: "нет такой", Host: "192.0.2.1"}}})
	w := httptest.NewRecorder()
	handleOpen(w, httptest.NewRequest("POST", "/api/rdp/open", strings.NewReader(`{"id":"x","w":800,"h":600}`)))
	if !strings.Contains(w.Body.String(), "sid") {
		t.Fatalf("open: %s", w.Body.String())
	}
	sid := strings.Split(strings.Split(w.Body.String(), `"sid":"`)[1], `"`)[0]
	r := httptest.NewRecorder()
	handleRead(r, httptest.NewRequest("GET", "/api/rdp/read?sid="+sid, nil))
	body := r.Body.String()
	t.Logf("guacd ответил: %.200s", body)
	if !strings.HasSuffix(body, ";") {
		t.Fatalf("read не целыми инструкциями: %q", body)
	}
}
