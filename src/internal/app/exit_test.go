package app

import (
	"testing"

	"sshtunnel/internal/config"
)

func TestАвтовыборНеМеняетСерверСЦепочкой(t *testing.T) {
	cfg := config.Default()
	b := cfg.AddProfile("Лондон", "")
	b.Host = "192.0.2.2"
	cfg.SetProfile(b)
	a := cfg.Profiles[0]
	a.Host, a.ExitVia = "192.0.2.1", []string{b.ID}
	cfg.SetProfile(a)
	cfg.AutoPickFastest = true
	got := (&App{}).connectCandidates(cfg)
	if len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("с цепочкой кандидаты %v", got)
	}
	hops := exitHops(cfg, cfg.Active())
	if len(hops) != 1 || hops[0].Host != "192.0.2.2" || hops[0].Name != "Лондон" {
		t.Fatalf("серверы цепочки %+v", hops)
	}
	// Смена сервера в цепочке — повод переподключиться.
	cfg2 := cfg
	cfg2.Profiles = append([]config.Profile(nil), cfg.Profiles...)
	b.Host = "192.0.2.3"
	cfg2.SetProfile(b)
	if !connectionSettingsChanged(cfg, cfg2) {
		t.Fatal("смена адреса сервера цепочки не переподключает")
	}
}
