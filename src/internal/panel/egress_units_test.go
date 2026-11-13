package panel

import (
	"strings"
	"testing"
)

// Таймер отката не должен срабатывать сразу при включении: OnBootSec на
// давно загруженном сервере уже в прошлом (нашёл стенд test/netns).
func TestEgressRollbackTimerOnlyActive(t *testing.T) {
	txt := egressRollbackTimerText()
	if strings.Contains(txt, "OnBootSec") || strings.Contains(txt, "OnStartupSec") {
		t.Fatalf("таймер отката с отсчётом от загрузки сработает сразу:\n%s", txt)
	}
	if !strings.Contains(txt, "OnActiveSec=60") || !strings.Contains(txt, "WantedBy=timers.target") {
		t.Fatalf("таймер отката:\n%s", txt)
	}
}
