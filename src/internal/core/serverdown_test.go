package core

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// localDriver — платформа, у которой местная часть поднимается всегда, а
// сервер то есть, то нет (ТЗ 6: сеть устройств без сервера).
type localDriver struct {
	fakeDriver
	smu        sync.Mutex
	serverDown error // не nil — Connect поднимает местное и сообщает это
	bypassErr  error // не nil — Apply с обходом не проходит (сервера нет)
	meshState  string
	meshDirect int
	gone       bool // сервер пропал после подключения (ServerLost)
}

func (d *localDriver) Connect(ctx context.Context) error {
	if err := d.fakeDriver.Connect(ctx); err != nil {
		return err
	}
	d.smu.Lock()
	defer d.smu.Unlock()
	if d.serverDown != nil {
		return &ServerDownError{Err: d.serverDown}
	}
	return nil
}

func (d *localDriver) Apply(m Modes, h Hygiene) error {
	d.smu.Lock()
	berr := d.bypassErr
	d.smu.Unlock()
	if m.Bypass && berr != nil {
		return berr
	}
	return d.fakeDriver.Apply(m, h)
}

// Link — связь как у настоящей платформы: местная часть поднята, а сервера
// нет (serverDown или gone) — «переподключение», а не «на связи».
func (d *localDriver) Link() (string, string) {
	st, detail := d.fakeDriver.Link()
	d.smu.Lock()
	defer d.smu.Unlock()
	if st == "connected" && (d.serverDown != nil || d.gone) {
		return "reconnecting", detail
	}
	return st, detail
}

func (d *localDriver) MeshLink() (string, int) {
	d.smu.Lock()
	defer d.smu.Unlock()
	return d.meshState, d.meshDirect
}

func newLocalMachine(t *testing.T, d *localDriver) *Machine {
	t.Helper()
	m := NewMachine(d, Options{})
	t.Cleanup(m.Shutdown)
	return m
}

func waitIdle(t *testing.T, m *Machine) State {
	t.Helper()
	if err := m.WaitIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m.State()
}

// Сервера нет, обе кнопки хотят включиться: круглая гаснет с ошибкой
// сервера, «Сеть устройств» включена, связь для ядра поднята, режим — только
// сеть устройств.
func TestСерверНедоступенСетьУстройствРаботает(t *testing.T) {
	down := errors.New("сервер не отвечает")
	d := &localDriver{fakeDriver: fakeDriver{meshAvail: true}, serverDown: down, bypassErr: down,
		meshState: "local", meshDirect: 2}
	m := newLocalMachine(t, d)
	m.Set(ptr(true), ptr(true))
	st := waitIdle(t, m)
	if st.Tunnel.Phase != PhaseError || st.Tunnel.Error != down.Error() {
		t.Fatalf("круглая: %+v", st.Tunnel)
	}
	if st.Mesh.Phase != PhaseOn {
		t.Fatalf("сеть устройств: %+v", st.Mesh)
	}
	if st.Mode != ModeMesh {
		t.Fatalf("режим %q", st.Mode)
	}
	if !m.LinkUp() {
		t.Fatal("местная часть поднята, а связи для ядра нет")
	}
	if st.MeshLink != "local" || st.MeshDirect != 2 {
		t.Fatalf("MeshLink=%q MeshDirect=%d", st.MeshLink, st.MeshDirect)
	}
	if !errors.Is(m.LastError(), down) {
		t.Fatalf("LastError=%v", m.LastError())
	}
	if sys, _, _, _ := d.snapshot(); sys != ModeMesh {
		t.Fatalf("система %q", sys)
	}

	// Сервер всё ещё не отвечает: круглую включили — гаснет с ошибкой, сеть
	// не трогается.
	m.Set(ptr(true), nil)
	st = waitIdle(t, m)
	if st.Tunnel.Phase != PhaseError || st.Mesh.Phase != PhaseOn {
		t.Fatalf("при мёртвом сервере: круглая %+v, сеть %+v", st.Tunnel, st.Mesh)
	}

	// Сервер вернулся — круглая включается без переподключения.
	d.smu.Lock()
	d.bypassErr = nil
	d.smu.Unlock()
	_, connectsBefore, _, _ := d.snapshot()
	m.Set(ptr(true), nil)
	st = waitIdle(t, m)
	if st.Tunnel.Phase != PhaseOn || st.Mesh.Phase != PhaseOn || st.Mode != ModeBypassMesh {
		t.Fatalf("после возврата сервера: %+v", st)
	}
	if _, c, _, _ := d.snapshot(); c != connectsBefore {
		t.Fatal("ради обхода переподключались")
	}

	// Кнопку сети выключили — и связь, и режим снимаются как обычно.
	m.Set(ptr(false), ptr(false))
	st = waitIdle(t, m)
	if st.Mesh.Phase != PhaseOff || m.LinkUp() {
		t.Fatalf("после выключения: %+v, связь %v", st.Mesh, m.LinkUp())
	}
}

// Только сеть устройств, сервера нет: никакой ошибки, кнопка включена.
func TestТолькоСетьБезСервера(t *testing.T) {
	down := errors.New("сервер не отвечает")
	d := &localDriver{fakeDriver: fakeDriver{meshAvail: true}, serverDown: down, meshState: "error"}
	m := newLocalMachine(t, d)
	m.Set(nil, ptr(true))
	st := waitIdle(t, m)
	if st.Mesh.Phase != PhaseOn || st.Tunnel.Phase != PhaseOff || st.Tunnel.Error != "" {
		t.Fatalf("%+v", st)
	}
	if st.MeshLink != "error" {
		t.Fatalf("MeshLink=%q", st.MeshLink)
	}
}

// Сервер пропал насовсем, но местная часть жива: круглая гаснет, сеть
// остаётся, режим применяется заново без обхода.
func TestServerLostОставляетСеть(t *testing.T) {
	d := &localDriver{fakeDriver: fakeDriver{meshAvail: true}}
	m := newLocalMachine(t, d)
	m.Set(ptr(true), ptr(true))
	if st := waitIdle(t, m); st.Mode != ModeBypassMesh {
		t.Fatalf("режим %q", st.Mode)
	}
	lost := errors.New("запасной не поднялся")
	d.smu.Lock()
	d.gone = true
	d.smu.Unlock()
	m.ServerLost(lost)
	st := waitIdle(t, m)
	if st.Tunnel.Phase != PhaseError || st.Mesh.Phase != PhaseOn || st.Mode != ModeMesh {
		t.Fatalf("%+v", st)
	}
	if sys, _, disc, _ := d.snapshot(); sys != ModeMesh || disc != 0 {
		t.Fatalf("система %q, снятий связи %d", sys, disc)
	}
}
