package webui

import (
	"testing"

	"sshtunnel/internal/config"
	"sshtunnel/internal/share"
)

func TestЦепочкаПереноситсяСсылками(t *testing.T) {
	cfg := config.Default()
	b := cfg.AddProfile("Лондон", "")
	b.Host, b.SSHPort, b.User = "lon.example", 22, "tunnel"
	cfg.SetProfile(b)
	a := cfg.Profiles[0]
	a.Host, a.ExitVia = "fra.example", []string{b.ID}
	cfg.SetProfile(a)

	refs := exportExitRefs(cfg, cfg.Profiles[0])
	if len(refs) != 1 || refs[0].Host != "lon.example" {
		t.Fatalf("ссылки %+v", refs)
	}
	// У получателя Лондон есть под другим id — находится по адресу.
	other := []config.Profile{{ID: "p_x", Host: "LON.example", SSHPort: 22, User: "tunnel"}}
	ids, missing := resolveExitRefs(other, "p_new", refs)
	if len(ids) != 1 || ids[0] != "p_x" || missing != nil {
		t.Fatalf("сопоставление %v %v", ids, missing)
	}
	// Нет — цепочка не ставится совсем, и об этом говорится.
	ids, missing = resolveExitRefs(nil, "p_new", refs)
	if ids != nil || len(missing) != 1 || exitImportNote(missing) == "" {
		t.Fatalf("без сервера: %v %v", ids, missing)
	}
	// Ключей в ссылке нет по построению.
	_ = share.ExitRef{}
}
