package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sshtunnel/internal/app"
)

// TestRules — правила меняются по одному, повтор не дублируется, остальные
// настройки не трогаются. Туннель не поднимается.
func TestRules(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	s, err := NewOn(app.New(withHost("203.0.113.10")), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	do := func(body string) rulesResp {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handleRules(rec, httptest.NewRequest(http.MethodPost, "/api/rules", strings.NewReader(body)))
		var r struct {
			Rules rulesResp `json:"rules"`
			Error string    `json:"error"`
		}
		json.Unmarshal(rec.Body.Bytes(), &r)
		if r.Error != "" {
			t.Fatalf("%s: %s", body, r.Error)
		}
		return r.Rules
	}
	do(`{"op":"add","list":"direct","value":"youtube.com"}`)
	r := do(`{"op":"add","list":"direct","value":"YouTube.com"}`)
	if len(r.DirectHosts) != 1 {
		t.Fatalf("повтор задублировался: %v", r.DirectHosts)
	}
	r = do(`{"op":"add","list":"apps","value":"C:\\Program Files\\App\\app.exe"}`)
	if len(r.FilterApps) != 1 || r.FilterApps[0] != `C:\Program Files\App\app.exe` {
		t.Fatalf("путь с пробелами: %v", r.FilterApps)
	}
	r = do(`{"op":"remove","list":"direct","value":"youtube.com"}`)
	if len(r.DirectHosts) != 0 {
		t.Fatalf("не убралось: %v", r.DirectHosts)
	}
	if r = do(`{"filterMode":"only"}`); r.FilterMode != "only" {
		t.Fatalf("filterMode: %v", r.FilterMode)
	}
	if !s.app.Config().SysProxy {
		t.Fatal("правила задели другие настройки")
	}

	// DNS режима VPN (ТЗ 9): общий для всех серверов.
	if r = do(`{"op":"add","list":"realip","value":".deepseek.com"}`); len(r.RealIPHosts) != 1 || r.FakeIP {
		t.Fatalf("realip: %+v", r)
	}
	if r = do(`{"fakeIP":true}`); !r.FakeIP || !s.app.Config().FakeIP || len(r.RealIPHosts) != 1 {
		t.Fatalf("fakeIP: %+v", r)
	}
	if r = do(`{"fakeIP":false}`); r.FakeIP {
		t.Fatal("fakeIP не выключился")
	}
}

// TestAppKeyGuard — ключ программы с Origin: null (страница в песочнице) не
// проходит; ключ своего приложения проходит только по разрешениям.
func TestAppKeyGuard(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	s, err := NewOn(app.New(withHost("203.0.113.10")), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ok := s.guard(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })

	r := httptest.NewRequest("GET", "/api/state", nil)
	r.Header.Set("Origin", "null")
	r.Header.Set("X-Token", s.token)
	rec := httptest.NewRecorder()
	ok(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ключ программы из песочницы: %d", rec.Code)
	}

	r = httptest.NewRequest("GET", "/api/state", nil)
	r.Header.Set("Origin", "null")
	r.Header.Set("Authorization", "Bearer app_nosuch")
	rec = httptest.NewRecorder()
	ok(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("чужой ключ приложения: %d", rec.Code)
	}
}
