package panel

import (
	"testing"

	"sshtunnel/internal/nodeapi"
)

// ТЗ 15: порты трёх новых плагинов — в «Открыть порты плагинов» и в
// проверке прав; предупреждения блокировки — по её состоянию.
func TestПортыИПредупрежденияТЗ15(t *testing.T) {
	cfg := nodeapi.Config{Machines: true, Adblock: true, Sitecheck: true}
	if got := portList(pluginPorts(cfg)); got != "47844,47845,47846" {
		t.Fatalf("порты: %s", got)
	}
	if got := fixServerPorts(true, "22,47835", cfg); got != "22,47835,47844,47845,47846" {
		t.Fatalf("исправление: %s", got)
	}
	ws := pluginWarnings(MeshRoleMain, true, "22", AccessSettings{}, nil, cfg)
	if len(ws) != 1 || ws[0].Code != WarnPortsBlocked || ws[0].Ports != "47844,47845,47846" {
		t.Fatalf("закрытые порты: %+v", ws)
	}
	st := &nodeapi.State{Config: cfg, Adblock: &nodeapi.AdblockState{}}
	if ws := stateWarnings(st); len(ws) != 1 || ws[0].Code != WarnAdblockNoSources {
		t.Fatalf("без источников: %+v", ws)
	}
	st.Adblock.Sources, st.Adblock.Error = []string{"https://x"}, "ни один источник не скачался"
	if ws := stateWarnings(st); len(ws) != 1 || ws[0].Code != WarnAdblockFailed || ws[0].Detail == "" {
		t.Fatalf("не скачался: %+v", ws)
	}
	st.Config.Adblock = false
	if ws := stateWarnings(st); len(ws) != 0 {
		t.Fatalf("выключенный плагин: %+v", ws)
	}
}
