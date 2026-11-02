package app

import (
	"errors"
	"sync"
	"testing"

	"sshtunnel/internal/config"
	"sshtunnel/internal/core"
	"sshtunnel/internal/tunnel"
)

// fakeNetLayer — слой VPN для тестов: запоминает, какие режимы ему
// применяли (PlanLayer) и сколько раз снимали.
type fakeNetLayer struct {
	mu       sync.Mutex
	prepared int
	applied  []core.Modes
	hygiene  []core.Hygiene
	tuns     []*tunnel.Tunnel
	detached int
	applyErr error
}

func (f *fakeNetLayer) Apply(tun *tunnel.Tunnel, _ config.Profile, m core.Modes, h core.Hygiene) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, m)
	f.hygiene = append(f.hygiene, h)
	f.tuns = append(f.tuns, tun)
	return nil
}

// lastBypass — что было в последнем применённом режиме у круглой кнопки.
func (f *fakeNetLayer) lastBypass() (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.applied) == 0 {
		return false, false
	}
	return f.applied[len(f.applied)-1].Bypass, true
}

func (f *fakeNetLayer) Prepare(cfg *tunnel.Config) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared++
	cfg.SocksAddr, cfg.HTTPAddr = "", ""
}

func (f *fakeNetLayer) Detach() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detached++
}

func vpnApp(t *testing.T, layer *fakeNetLayer) *App {
	t.Helper()
	keyPath, pub := testKeyPath(t)
	addr := newFakeSSHServer(t, pub, true)
	p := profileAt(t, "main", addr, keyPath)
	cfg := config.Config{Profiles: []config.Profile{p}, ActiveProfile: p.ID, SysProxy: true}
	a := New(cfg)
	a.SetNetLayer(layer)
	t.Cleanup(a.StopNow)
	return a
}

// В режиме VPN системный прокси не трогается, режим применяется к адаптеру
// целиком на подключённом туннеле, а «Выключить» снимает его сразу, без
// слива.
func TestVPNРежимПоднимаетИСнимаетАдаптер(t *testing.T) {
	layer := &fakeNetLayer{}
	a := vpnApp(t, layer)

	if err := a.Start(); err != nil {
		t.Fatalf("подключение: %v", err)
	}
	layer.mu.Lock()
	prepared, applied := layer.prepared, len(layer.applied)
	layer.mu.Unlock()
	if prepared == 0 || applied != 1 {
		t.Fatalf("Prepare=%d Apply=%d, ожидалось хотя бы 1 и ровно 1", prepared, applied)
	}
	a.mu.Lock()
	sysOn, cfg := a.sysOn, a.tun.Config()
	a.mu.Unlock()
	if sysOn {
		t.Error("в режиме VPN системный прокси включаться не должен")
	}
	if cfg.SocksAddr != "" || cfg.HTTPAddr != "" {
		t.Errorf("локальные прокси должны быть отключены: %q %q", cfg.SocksAddr, cfg.HTTPAddr)
	}

	a.Stop()
	layer.mu.Lock()
	detached := layer.detached
	layer.mu.Unlock()
	if detached == 0 {
		t.Error("после «Выключить» адаптер должен сниматься")
	}
	a.mu.Lock()
	draining := a.draining
	a.mu.Unlock()
	if draining != nil {
		t.Error("в режиме VPN слива быть не должно")
	}
}

// Не встал адаптер — кнопка гаснет с ошибкой, туннель не висит.
func TestVPNОшибкаАдаптераОстанавливаетТуннель(t *testing.T) {
	layer := &fakeNetLayer{applyErr: errors.New("нет драйвера")}
	a := vpnApp(t, layer)

	if err := a.Start(); err == nil {
		t.Fatal("ожидалась ошибка подключения")
	}
	if a.Running() {
		t.Error("туннель не должен считаться запущенным")
	}
	if st := a.CoreState(); st.Tunnel.Phase != core.PhaseError || st.Tunnel.Error == "" {
		t.Errorf("кнопка %+v, ожидалась ошибка с текстом", st.Tunnel)
	}
}
