package app

import (
	"testing"

	"sshtunnel/internal/config"
	"sshtunnel/internal/core"
)

// meshVPNApp — приложение в режиме VPN с сервером, у которого включена сеть
// устройств. Сам meshd подставной сервер не умеет — клиенту сети это не
// мешает существовать, а здесь проверяются только кнопки.
func meshVPNApp(t *testing.T, layer *fakeNetLayer, withMesh bool) *App {
	t.Helper()
	keyPath, pub := testKeyPath(t)
	addr := newFakeSSHServer(t, pub, true)
	p := profileAt(t, "main", addr, keyPath)
	if withMesh {
		p.MeshEnabled, p.MeshKey = true, "ключ-сети"
	}
	a := New(config.Config{Profiles: []config.Profile{p}, ActiveProfile: p.ID})
	a.SetNetLayer(layer)
	t.Cleanup(a.StopNow)
	return a
}

func wantModes(t *testing.T, a *App, bypass, mesh bool) {
	t.Helper()
	m := a.Modes()
	if m.Bypass != bypass || m.Mesh != mesh {
		t.Fatalf("кнопки: обход=%v сеть=%v, ожидалось обход=%v сеть=%v", m.Bypass, m.Mesh, bypass, mesh)
	}
}

// Круглая кнопка выключает только обход: туннель держится ради сети
// устройств, адаптер получает режим «только сеть», а не снимается.
func TestBypassOffKeepsMesh(t *testing.T) {
	layer := &fakeNetLayer{}
	a := meshVPNApp(t, layer, true)
	if err := a.WantWait(on(), on()); err != nil {
		t.Fatal(err)
	}
	if b, ok := layer.lastBypass(); !ok || !b {
		t.Fatalf("при подключении адаптер должен получить «весь трафик», получил %v (было ли: %v)", b, ok)
	}
	wantModes(t, a, true, true)

	if err := a.SetBypass(false); err != nil {
		t.Fatal(err)
	}
	if !a.Running() {
		t.Fatal("туннель погас, хотя сеть устройств включена")
	}
	if b, _ := layer.lastBypass(); b {
		t.Fatal("адаптер не переведён в режим «только сеть устройств»")
	}
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if tun.Bypass() {
		t.Fatal("туннель всё ещё ведёт сайты через сервер")
	}
	wantModes(t, a, false, true)

	// И обратно — без переподключения.
	if err := a.SetBypass(true); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	same := a.tun == tun
	a.mu.Unlock()
	if !same || !tun.Bypass() {
		t.Fatal("обход должен включаться на том же туннеле")
	}
	wantModes(t, a, true, true)
}

// Каждая кнопка помнит своё: выключили обе — туннель гаснет целиком, а
// следующее включение круглой не включает сеть устройств.
func TestMeshOffThenBypassOffStops(t *testing.T) {
	layer := &fakeNetLayer{}
	a := meshVPNApp(t, layer, true)
	if err := a.WantWait(on(), on()); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMesh(false); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if !a.Running() || !tun.MeshPaused() {
		t.Fatal("сеть устройств должна выключиться, а туннель — остаться")
	}
	wantModes(t, a, true, false)

	if err := a.SetBypass(false); err != nil {
		t.Fatal(err)
	}
	if a.Running() {
		t.Fatal("обе кнопки выключены, а туннель работает")
	}
	wantModes(t, a, false, false)

	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	wantModes(t, a, true, false)
}

// Сеть устройств включается отдельно: туннель поднимается без обхода, а
// круглая кнопка потом её не трогает.
func TestMeshOnlyStart(t *testing.T) {
	layer := &fakeNetLayer{}
	a := meshVPNApp(t, layer, true)
	if err := a.SetMesh(true); err != nil {
		t.Fatal(err)
	}
	if b, _ := layer.lastBypass(); b {
		t.Fatal("адаптер поднят в режиме «весь трафик», а просили только сеть")
	}
	wantModes(t, a, false, true)

	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	wantModes(t, a, true, true)
	if err := a.SetBypass(false); err != nil {
		t.Fatal(err)
	}
	wantModes(t, a, false, true)

	if err := a.SetMesh(false); err != nil {
		t.Fatal(err)
	}
	if a.Running() {
		t.Fatal("сеть выключена при выключенном обходе — туннель должен погаснуть")
	}
}

// Без сети устройств круглая кнопка работает как обычно, а «Сеть
// устройств» честно отказывает.
func TestNoMeshButtons(t *testing.T) {
	layer := &fakeNetLayer{}
	a := meshVPNApp(t, layer, false)
	if err := a.SetMesh(true); err == nil {
		t.Fatal("ожидалась ошибка: сеть устройств не настроена")
	}
	if a.Running() {
		t.Fatal("туннель поднялся без сети устройств")
	}
	if err := a.SetBypass(true); err != nil {
		t.Fatal(err)
	}
	wantModes(t, a, true, false)
	if err := a.SetBypass(false); err != nil {
		t.Fatal(err)
	}
	if a.Running() {
		t.Fatal("без сети устройств выключение обхода должно останавливать туннель")
	}
}

// Смена режима доходит до слоя VPN вместе с гигиеной перехода: выключили
// обход — сброс подставных адресов и закрытие соединений из адаптера.
func TestПереключениеПередаётГигиену(t *testing.T) {
	layer := &fakeNetLayer{}
	a := meshVPNApp(t, layer, true)
	if err := a.WantWait(on(), on()); err != nil {
		t.Fatal(err)
	}
	if err := a.SetBypass(false); err != nil {
		t.Fatal(err)
	}
	layer.mu.Lock()
	h := layer.hygiene[len(layer.hygiene)-1]
	layer.mu.Unlock()
	want := core.Transition(core.Modes{Bypass: true, Mesh: true}, core.Modes{Mesh: true})
	if h != want {
		t.Fatalf("гигиена %+v, ожидалась %+v", h, want)
	}
	if !h.ResetFakeIP || !h.CloseStackInternet || !h.FlushDNS {
		t.Fatalf("выключение обхода без сброса: %+v", h)
	}
}
