package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"sshtunnel/internal/meshsvc"
)

func demoCall(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: не JSON (%d): %s", method, path, rec.Code, rec.Body.String())
	}
	return rec.Code, out
}

// Каждая ручка, которую вызывает страница, в режиме показа отвечает JSON, а
// не 404: список берётся из самой страницы.
func TestDemoОтвечаетНаВсеРучкиСтраницы(t *testing.T) {
	page, err := assets.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`api\('(/api/[a-z/-]+)`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(page), -1) {
		seen[strings.TrimSuffix(m[1], "/")] = true
	}
	// Ручки, собранные из кусков ('/api/clients/' + action).
	for _, a := range []string{"freeze", "unfreeze", "disconnect"} {
		seen["/api/clients/"+a] = true
	}
	delete(seen, "/api/clients") // ниже отдельно — GET
	if len(seen) < 25 {
		t.Fatalf("подозрительно мало ручек на странице: %v", seen)
	}
	h := NewDemoHandler("test")
	for path := range seen {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req := httptest.NewRequest(method, path, strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound && !strings.Contains(rec.Body.String(), "client_not_found") {
				t.Errorf("%s %s: 404", method, path)
			}
		}
	}
	if code, out := demoCall(t, h, http.MethodGet, "/api/session", nil); code != 200 || out["demo"] != true || out["authenticated"] != true {
		t.Fatalf("сессия показа: %d %v", code, out)
	}
	if _, out := demoCall(t, h, http.MethodGet, "/api/clients", nil); len(out["clients"].([]any)) < 5 {
		t.Fatalf("мало клиентов: %v", out)
	}
}

func TestDemoСостояниеСети(t *testing.T) {
	d := &demoBackend{returning: map[string]demoReturn{}}
	d.seed()
	st := d.state()
	if len(st.Networks) != 3 || len(st.Servers) != 2 {
		t.Fatalf("сети %d, серверы %d", len(st.Networks), len(st.Servers))
	}
	var empty, shared bool
	users := map[string]int{}
	for _, n := range st.Networks {
		if len(n.Devices) == 0 && n.EmptySince > 0 {
			empty = true
		}
		for _, x := range n.Devices {
			users[x.User]++
		}
	}
	for u, c := range users {
		if c > 1 && strings.HasPrefix(u, "tun_") {
			shared = true
		}
	}
	if !empty || !shared {
		t.Fatalf("нет пустой сети (%v) или общего пользователя (%v)", empty, shared)
	}
}

func TestDemoЖурналИПерезапуск(t *testing.T) {
	h := NewDemoHandler("test")
	_, out := demoCall(t, h, http.MethodGet, "/api/mesh/events?after=0&limit=5", nil)
	if n := len(out["events"].([]any)); n != 5 {
		t.Fatalf("limit=5: %d", n)
	}
	_, out = demoCall(t, h, http.MethodGet, "/api/mesh/events?after=0", nil)
	last := int64(out["seq"].(float64))
	if last < 10 {
		t.Fatalf("seq %d", last)
	}
	_, out = demoCall(t, h, http.MethodGet, fmt.Sprintf("/api/mesh/events?after=%d", last), nil)
	if len(out["events"].([]any)) != 0 {
		t.Fatalf("после последнего — пусто: %v", out)
	}
	demoCall(t, h, http.MethodPost, "/api/demo/restart", nil)
	_, out = demoCall(t, h, http.MethodGet, fmt.Sprintf("/api/mesh/events?after=%d", last), nil)
	if int64(out["seq"].(float64)) >= last {
		t.Fatalf("после перезапуска seq должен стать меньше %d: %v", last, out)
	}
}

func TestDemoПроверкаДоступа(t *testing.T) {
	h := NewDemoHandler("test")
	const home = "9f3c2a71d04e8b65"
	cases := []struct {
		from, to string
		port     int
		allowed  bool
		reason   string
	}{
		{"e1a04f", "7c22d9", 0, true, "rule"},         // моё → моё
		{"c4f0e2", "e1a04f", 47835, false, "no rule"}, // гость → ПК
		{"b07d33", "55aa01", 22, true, "rule"},        // «работа» → «работа» на 22
		{"b07d33", "55aa01", 445, false, "no rule"},
		{"e1a04f", "nope", 0, false, "unknown device"},
	}
	for _, c := range cases {
		code, out := demoCall(t, h, http.MethodPost, "/api/mesh/check", map[string]any{"net": home, "from": c.from, "to": c.to, "port": c.port})
		if code != 200 || out["allowed"] != c.allowed || out["reason"] != c.reason {
			t.Errorf("%s → %s:%d: %d %v", c.from, c.to, c.port, code, out)
		}
	}
	demoCall(t, h, http.MethodGet, "/api/demo/policy?preset=off", nil)
	_, out := demoCall(t, h, http.MethodPost, "/api/mesh/check", map[string]any{"net": home, "from": "c4f0e2", "to": "e1a04f"})
	if out["reason"] != "policy off" || out["allowed"] != true {
		t.Fatalf("выключено: %v", out)
	}
}

func TestDemoПраваТеЖеПроверки(t *testing.T) {
	h := NewDemoHandler("test")
	rules := make([]AccessRule, maxAccessRules+1)
	for i := range rules {
		rules[i] = AccessRule{From: "*", To: "*"}
	}
	if code, _ := demoCall(t, h, http.MethodPost, "/api/access", AccessSettings{Enabled: true, Rules: rules}); code != 400 {
		t.Fatalf("больше %d правил должно отклоняться: %d", maxAccessRules, code)
	}
	// Группу, в которой ещё есть устройства, убрать нельзя.
	if code, out := demoCall(t, h, http.MethodPost, "/api/access", AccessSettings{Enabled: true}); code != 400 {
		t.Fatalf("убрали группы с участниками: %d %v", code, out)
	}
	// Членство — только в заведённых группах.
	if code, _ := demoCall(t, h, http.MethodPost, "/api/clients/groups", map[string]any{"id": demoUser(1), "groups": []string{"нет-такой"}}); code != 400 {
		t.Fatalf("неизвестная группа принята: %d", code)
	}
}

func TestDemoСтарыйMeshdИУдалениеСети(t *testing.T) {
	h := NewDemoHandler("test")
	demoCall(t, h, http.MethodGet, "/api/demo/oldmeshd?on=1", nil)
	code, out := demoCall(t, h, http.MethodPost, "/api/mesh/net/rename", map[string]string{"net": "9f3c2a71d04e8b65", "name": "x"})
	if code != 400 || !strings.HasPrefix(out["error"].(string), "эта версия meshd не умеет этого") {
		t.Fatalf("старый meshd: %d %v", code, out)
	}
	demoCall(t, h, http.MethodPost, "/api/mesh/reinstall", map[string]any{})
	if code, _ := demoCall(t, h, http.MethodPost, "/api/mesh/net/delete", map[string]string{"net": "41be09c7aa13f2d0"}); code != 200 {
		t.Fatalf("удаление: %d", code)
	}
	_, st := demoCall(t, h, http.MethodGet, "/api/mesh/state", nil)
	if n := len(st["networks"].([]any)); n != 2 {
		t.Fatalf("сетей после удаления: %d", n)
	}
	var ev meshsvc.Event
	_, out = demoCall(t, h, http.MethodGet, "/api/mesh/events?after=0&limit=1000", nil)
	all := out["events"].([]any)
	b, _ := json.Marshal(all[len(all)-1])
	json.Unmarshal(b, &ev)
	if ev.Type != "net-delete" {
		t.Fatalf("последнее событие: %+v", ev)
	}
}
