package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sshtunnel/internal/app"
	"sshtunnel/internal/config"
)

// Правка цепочки выхода через «Сохранить» (POST /api/config) получает время
// и считается правкой человека (ТЗ 16): она новее цепочки из панели, и
// панели серверов это увидят. Сохранение без правки цепочки её не трогает.
func TestСохранениеСтавитВремяПравкиЦепочки(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	cfg := withHost("fra.example")
	b := cfg.AddProfile("Лондон", "")
	b.Host = "lon.example"
	cfg.SetProfile(b)
	cfg.ApplyExitOrder(config.ExitOrder{Entry: "fra.example:22", Via: []string{"lon.example:22"}, T: 100, By: "Франкфурт"})
	a := app.New(cfg)
	s, err := NewOn(a, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	save := func(c config.Config) config.Config {
		t.Helper()
		body, _ := json.Marshal(c)
		rec := httptest.NewRecorder()
		s.handleConfig(rec, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(string(body))))
		var resp struct {
			Config config.Config `json:"config"`
			Error  string        `json:"error"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Error != "" {
			t.Fatal(resp.Error)
		}
		return resp.Config
	}
	got := save(a.Config())
	if p := got.Active(); p.ExitT != 100 || p.ExitBy != "Франкфурт" {
		t.Fatalf("сохранение без правки цепочки её тронуло: %+v", p)
	}
	edit := a.Config()
	edit.Profiles = append([]config.Profile(nil), edit.Profiles...)
	p := edit.Active()
	p.ExitVia = nil
	edit.SetProfile(p)
	got = save(edit)
	if p := got.Active(); len(p.ExitVia) != 0 || p.ExitT <= 100 || p.ExitBy != "" {
		t.Fatalf("правка человека: %+v", p)
	}
}
