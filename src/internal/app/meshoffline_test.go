package app

// ТЗ 6: сеть устройств поднимается и тогда, когда сервер с самого старта
// недоступен; круглая кнопка при этом честно гаснет с ошибкой сервера.

import (
	"net"
	"testing"
	"time"

	"sshtunnel/internal/config"
	"sshtunnel/internal/core"
	"sshtunnel/internal/events"
)

// offlineApp — сервер по адресу addr (никто не слушает), сеть устройств
// включена; layer nil — режим прокси.
func offlineApp(t *testing.T, addr string, layer *fakeNetLayer) *App {
	t.Helper()
	keyPath, _ := testKeyPath(t)
	p := profileAt(t, "main", addr, keyPath)
	p.MeshEnabled, p.MeshKey = true, "ключ-сети"
	p.ConnectTimeoutSec = 2
	a := New(config.Config{Profiles: []config.Profile{p}, ActiveProfile: p.ID})
	if layer != nil {
		a.SetNetLayer(layer)
	}
	t.Cleanup(a.StopNow)
	return a
}

func checkMeshWithoutServer(t *testing.T, a *App) {
	t.Helper()
	st := a.CoreState()
	if st.Tunnel.Phase != core.PhaseError || st.Tunnel.Error == "" {
		t.Fatalf("круглая без сервера: %+v", st.Tunnel)
	}
	if st.Mesh.Phase != core.PhaseOn {
		t.Fatalf("сеть устройств без сервера: %+v", st.Mesh)
	}
	if st.Mode != core.ModeMesh {
		t.Fatalf("режим %q", st.Mode)
	}
	if st.MeshLink == "" {
		t.Fatal("состояние сети устройств не попало в core.State")
	}
	if a.Mesh() == nil {
		t.Fatal("клиента сети устройств нет")
	}
	if !a.Running() {
		t.Fatal("местный туннель не поднят")
	}
	if s := a.MeshStatus().State; s == "off" || s == "stopped" {
		t.Fatalf("MeshStatus %q", s)
	}
}

func TestСетьУстройствБезСервераVPN(t *testing.T) {
	layer := &fakeNetLayer{}
	a := offlineApp(t, closedPort(t), layer)
	if err := a.WantWait(on(), on()); err == nil {
		t.Fatal("сервера нет, а обход включился без ошибки")
	}
	checkMeshWithoutServer(t, a)
	if b, ok := layer.lastBypass(); !ok || b {
		t.Fatalf("адаптер: применён=%v весь трафик=%v, ждали «только сеть»", ok, b)
	}

	// Кнопку сети выключили — клиент гаснет, связь снимается.
	if err := a.WantWait(nil, off()); err != nil {
		t.Fatal(err)
	}
	if a.Mesh() != nil || a.Running() {
		t.Fatalf("после выключения: клиент %v, туннель %v", a.Mesh() != nil, a.Running())
	}
}

func TestСетьУстройствБезСервераПрокси(t *testing.T) {
	a := offlineApp(t, closedPort(t), nil)
	if err := a.WantWait(nil, on()); err != nil {
		t.Fatalf("только сеть без сервера: %v", err)
	}
	st := a.CoreState()
	if st.Mesh.Phase != core.PhaseOn || st.Tunnel.Phase != core.PhaseOff || st.Link != events.StateReconnecting {
		t.Fatalf("%+v", st)
	}
	if a.Mesh() == nil {
		t.Fatal("клиента сети устройств нет")
	}
}

// Сервер появился позже: тот же клиент сети, круглая включается без
// переподключения и без второго клиента.
func TestСерверПоявилсяПозже(t *testing.T) {
	old := applyServerWait
	applyServerWait = 10 * time.Second
	t.Cleanup(func() { applyServerWait = old })

	keyPath, pub := testKeyPath(t)
	addr := closedPort(t)
	p := profileAt(t, "main", addr, keyPath)
	p.MeshEnabled, p.MeshKey = true, "ключ-сети"
	p.ConnectTimeoutSec = 2
	layer := &fakeNetLayer{}
	a := New(config.Config{Profiles: []config.Profile{p}, ActiveProfile: p.ID})
	a.SetNetLayer(layer)
	t.Cleanup(a.StopNow)

	if err := a.WantWait(on(), on()); err == nil {
		t.Fatal("сервера нет, а обход включился")
	}
	checkMeshWithoutServer(t, a)
	client := a.Mesh()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("порт %s успели занять: %v", addr, err)
	}
	serveFakeSSH(t, ln, fakeSSHConfig(t, pub, true))

	if err := a.WantWait(on(), nil); err != nil {
		t.Fatalf("сервер появился, а обход не включился: %v", err)
	}
	st := a.CoreState()
	if st.Tunnel.Phase != core.PhaseOn || st.Mesh.Phase != core.PhaseOn || st.Mode != core.ModeBypassMesh {
		t.Fatalf("%+v", st)
	}
	if a.Mesh() != client {
		t.Fatal("клиент сети пересоздан")
	}
}
