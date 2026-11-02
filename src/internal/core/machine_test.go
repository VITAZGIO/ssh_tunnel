package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeDriver — платформа для тестов ядра: запоминает, что от неё хотели.
type fakeDriver struct {
	mu          sync.Mutex
	meshAvail   bool
	connectErr  error
	gate        chan struct{} // не nil — Connect ждёт его (или отмены)
	connects    int
	disconnects int
	applies     []Modes
	hygiene     []Hygiene
	applyErr    func(Modes) error
	up          bool
	// system — «состояние системы» после последнего Apply/Disconnect:
	// то, что видно снаружи (маршруты, DNS, прокси).
	system string
}

func (d *fakeDriver) MeshAvailable() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.meshAvail
}

func (d *fakeDriver) Connect(ctx context.Context) error {
	d.mu.Lock()
	d.connects++
	gate, err := d.gate, d.connectErr
	d.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.up = true
	d.mu.Unlock()
	return nil
}

func (d *fakeDriver) Disconnect() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disconnects++
	d.up = false
	d.system = ModeOff
}

func (d *fakeDriver) Apply(m Modes, h Hygiene) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.up {
		return errors.New("Apply без связи")
	}
	if !m.Any() {
		return errors.New("Apply с выключенным режимом")
	}
	if d.applyErr != nil {
		if err := d.applyErr(m); err != nil {
			return err
		}
	}
	d.applies = append(d.applies, m)
	d.hygiene = append(d.hygiene, h)
	// Платформа строит систему по плану целиком — то, что было до этого,
	// роли не играет. Это и проверяется: после любого перехода система
	// совпадает с чистым запуском.
	d.system = m.Name()
	return nil
}

func (d *fakeDriver) Link() (string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.up {
		return "connected", "203.0.113.1"
	}
	return "stopped", ""
}

func (d *fakeDriver) snapshot() (system string, connects, disconnects int, up bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.system, d.connects, d.disconnects, d.up
}

func newTestMachine(t *testing.T, d *fakeDriver, opt Options) *Machine {
	t.Helper()
	m := NewMachine(d, opt)
	t.Cleanup(m.Shutdown)
	return m
}

func idle(t *testing.T, m *Machine) State {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.WaitIdle(ctx); err != nil {
		t.Fatalf("ядро не пришло в покой: %v", err)
	}
	return m.State()
}

func ptr(b bool) *bool { return &b }

var allModes = []Modes{{}, {Mesh: true}, {Bypass: true}, {Bypass: true, Mesh: true}}

// Все переходы между четырьмя режимами, каждой кнопкой по отдельности и в
// обоих порядках: вторая кнопка не дёргается, а система после перехода
// такая же, как после чистого запуска в новом режиме.
func TestВсеПереходыМеждуРежимами(t *testing.T) {
	for _, from := range allModes {
		for _, to := range allModes {
			for _, tunnelFirst := range []bool{true, false} {
				name := fmt.Sprintf("%s→%s/круглая-первой=%v", from.Name(), to.Name(), tunnelFirst)
				t.Run(name, func(t *testing.T) {
					d := &fakeDriver{meshAvail: true}
					m := newTestMachine(t, d, Options{})
					m.Set(ptr(from.Bypass), ptr(from.Mesh))
					st := idle(t, m)
					if st.Mode != from.Name() {
						t.Fatalf("исходный режим %q, ожидался %q", st.Mode, from.Name())
					}

					steps := []func(){
						func() {
							if from.Bypass == to.Bypass {
								return
							}
							before := m.State().Mesh
							m.Set(ptr(to.Bypass), nil)
							after := idle(t, m)
							if after.Mesh.Want != before.Want || after.Mesh.Phase != before.Phase {
								t.Errorf("круглая кнопка тронула сеть устройств: было %+v, стало %+v", before, after.Mesh)
							}
						},
						func() {
							if from.Mesh == to.Mesh {
								return
							}
							before := m.State().Tunnel
							m.Set(nil, ptr(to.Mesh))
							after := idle(t, m)
							if after.Tunnel.Want != before.Want || after.Tunnel.Phase != before.Phase {
								t.Errorf("«Сеть устройств» тронула круглую: было %+v, стало %+v", before, after.Tunnel)
							}
						},
					}
					if !tunnelFirst {
						steps[0], steps[1] = steps[1], steps[0]
					}
					for _, s := range steps {
						s()
					}

					st = idle(t, m)
					if st.Mode != to.Name() {
						t.Fatalf("режим %q, ожидался %q", st.Mode, to.Name())
					}
					system, _, _, up := d.snapshot()
					if up != to.Any() {
						t.Errorf("связь с сервером %v, ожидалась %v", up, to.Any())
					}

					// Чистый запуск сразу в целевом режиме.
					clean := &fakeDriver{meshAvail: true}
					mc := newTestMachine(t, clean, Options{})
					mc.Set(ptr(to.Bypass), ptr(to.Mesh))
					idle(t, mc)
					cleanSystem, _, _, _ := clean.snapshot()
					if to.Any() && system != cleanSystem {
						t.Errorf("после перехода система %q, после чистого запуска %q", system, cleanSystem)
					}
					if !to.Any() && system != ModeOff && system != "" {
						t.Errorf("в «Выключено» осталось %q", system)
					}
					if st.Tunnel.Phase != phaseFor(to.Bypass) || st.Mesh.Phase != phaseFor(to.Mesh) {
						t.Errorf("фазы кнопок %s/%s, ожидались %s/%s", st.Tunnel.Phase, st.Mesh.Phase, phaseFor(to.Bypass), phaseFor(to.Mesh))
					}
				})
			}
		}
	}
}

func phaseFor(on bool) string {
	if on {
		return PhaseOn
	}
	return PhaseOff
}

// Нажатие во время «Подключаюсь…» отменяет подключение, ничего не висит и
// ошибкой это не считается.
func TestНажатиеВоВремяПодключенияОтменяет(t *testing.T) {
	d := &fakeDriver{gate: make(chan struct{})}
	m := newTestMachine(t, d, Options{})
	m.Set(ptr(true), nil)

	waitFor(t, func() bool { return m.State().Tunnel.Phase == PhaseConnecting && m.State().Link == "connecting" })
	m.Set(ptr(false), nil)
	st := idle(t, m)
	if st.Tunnel.Phase != PhaseOff || st.Tunnel.Error != "" {
		t.Fatalf("после отмены кнопка %+v, ожидалась выключенной без ошибки", st.Tunnel)
	}
	if _, _, _, up := d.snapshot(); up {
		t.Fatal("отменённое подключение поднялось")
	}
	if st.Link != "stopped" {
		t.Errorf("связь %q после отмены", st.Link)
	}
}

// Отмена круглой во время подключения не трогает сеть устройств: связь
// нужна ей, подключение продолжается.
func TestОтменаОднойКнопкиНеТрогаетДругую(t *testing.T) {
	gate := make(chan struct{})
	d := &fakeDriver{gate: gate, meshAvail: true}
	m := newTestMachine(t, d, Options{})
	m.Set(ptr(true), ptr(true))
	waitFor(t, func() bool { return m.State().Link == "connecting" })

	m.Set(ptr(false), nil)
	waitFor(t, func() bool { return !m.State().Tunnel.Want })
	if st := m.State(); st.Mesh.Phase != PhaseConnecting {
		t.Fatalf("сеть устройств %+v, должна продолжать подключаться", st.Mesh)
	}
	close(gate)
	st := idle(t, m)
	if st.Mode != ModeMesh || st.Mesh.Phase != PhaseOn || st.Tunnel.Phase != PhaseOff {
		t.Fatalf("режим %q, кнопки %+v / %+v; ожидалось «только сеть устройств»", st.Mode, st.Tunnel, st.Mesh)
	}
	if _, connects, _, _ := d.snapshot(); connects != 1 {
		t.Errorf("подключений %d, ожидалось одно", connects)
	}
}

// Не подключились — ждавшие кнопки гаснут с ошибкой и её кодом.
func TestОшибкаПодключенияГаситКнопки(t *testing.T) {
	d := &fakeDriver{meshAvail: true, connectErr: errors.New("сервер не отвечает")}
	m := newTestMachine(t, d, Options{ErrorKind: func(error) string { return "no_response" }})
	m.Set(ptr(true), ptr(true))
	st := idle(t, m)
	for name, b := range map[string]Button{"круглая": st.Tunnel, "сеть": st.Mesh} {
		if b.Want || b.Phase != PhaseError || b.Error == "" || b.ErrorKind != "no_response" {
			t.Errorf("%s: %+v, ожидалась ошибка", name, b)
		}
	}
	if m.LastError() == nil {
		t.Error("LastError пуст")
	}
	// Новое нажатие стирает ошибку этой кнопки.
	d.mu.Lock()
	d.connectErr = nil
	d.mu.Unlock()
	m.Set(ptr(true), nil)
	st = idle(t, m)
	if st.Tunnel.Phase != PhaseOn || st.Mesh.Phase != PhaseError {
		t.Errorf("после повтора %+v / %+v", st.Tunnel, st.Mesh)
	}
}

func TestСетьБезНастройкиСервера(t *testing.T) {
	d := &fakeDriver{}
	m := newTestMachine(t, d, Options{})
	if err := m.Set(nil, ptr(true)); !errors.Is(err, ErrNoMesh) {
		t.Fatalf("ошибка %v, ожидалась ErrNoMesh", err)
	}
	st := idle(t, m)
	if st.Mesh.Want || st.Mesh.Phase != PhaseError {
		t.Errorf("кнопка %+v", st.Mesh)
	}
	if _, connects, _, _ := d.snapshot(); connects != 0 {
		t.Error("без сети устройств подключаться было незачем")
	}
}

// Выбор человека запоминается, восстановление при запуске — нет.
func TestЗапоминаниеКнопок(t *testing.T) {
	var saved []Want
	var mu sync.Mutex
	d := &fakeDriver{meshAvail: true}
	m := newTestMachine(t, d, Options{Save: func(w Want) {
		mu.Lock()
		saved = append(saved, w)
		mu.Unlock()
	}})
	m.Restore(Want{Tunnel: true})
	idle(t, m)
	m.Set(nil, ptr(true))
	idle(t, m)
	m.Set(ptr(false), nil)
	idle(t, m)
	mu.Lock()
	defer mu.Unlock()
	want := []Want{{Tunnel: true, Mesh: true}, {Mesh: true}}
	if fmt.Sprint(saved) != fmt.Sprint(want) {
		t.Fatalf("запомнено %v, ожидалось %v", saved, want)
	}
}

// Restore без сети устройств у сервера просто не включает её.
func TestВосстановлениеБезСети(t *testing.T) {
	d := &fakeDriver{}
	m := newTestMachine(t, d, Options{})
	m.Restore(Want{Tunnel: true, Mesh: true})
	st := idle(t, m)
	if st.Mode != ModeBypass || st.Mesh.Phase != PhaseOff {
		t.Fatalf("режим %q, сеть %+v", st.Mode, st.Mesh)
	}
}

func TestПереподключениеСохраняетКнопки(t *testing.T) {
	d := &fakeDriver{meshAvail: true}
	m := newTestMachine(t, d, Options{})
	m.Set(ptr(false), ptr(true))
	idle(t, m)
	m.Restart()
	st := idle(t, m)
	_, connects, disconnects, up := d.snapshot()
	if connects != 2 || disconnects != 1 || !up {
		t.Fatalf("подключений %d, отключений %d, связь %v", connects, disconnects, up)
	}
	if st.Mode != ModeMesh {
		t.Errorf("режим %q после переподключения", st.Mode)
	}
}

// Смена сети или правил — тот же режим применяется заново целиком, а
// гигиена сводится к пересчёту решений.
func TestПовторноеПрименение(t *testing.T) {
	d := &fakeDriver{}
	m := newTestMachine(t, d, Options{})
	m.Set(ptr(true), nil)
	idle(t, m)
	m.Reapply()
	idle(t, m)
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.applies) != 2 {
		t.Fatalf("Apply вызван %d раз, ожидалось 2", len(d.applies))
	}
	h := d.hygiene[1]
	if !h.Reevaluate || h.ResetFakeIP || h.CloseStackInternet || h.CloseMesh {
		t.Errorf("гигиена повторного применения %+v", h)
	}
}

// Не встало то, что включали, — гаснет только эта кнопка, остальное
// применяется заново целиком.
func TestОшибкаПримененияГаситТолькоСвоюКнопку(t *testing.T) {
	d := &fakeDriver{meshAvail: true}
	d.applyErr = func(m Modes) error {
		if m.Mesh {
			return errors.New("правило DNS не встало")
		}
		return nil
	}
	m := newTestMachine(t, d, Options{})
	m.Set(ptr(true), nil)
	idle(t, m)
	m.Set(nil, ptr(true))
	st := idle(t, m)
	if st.Mode != ModeBypass || st.Tunnel.Phase != PhaseOn || st.Mesh.Phase != PhaseError {
		t.Fatalf("режим %q, кнопки %+v / %+v", st.Mode, st.Tunnel, st.Mesh)
	}
}

func TestГигиенаПереходов(t *testing.T) {
	cases := []struct {
		prev, next Modes
		want       Hygiene
	}{
		{Modes{Bypass: true}, Modes{Bypass: true}, Hygiene{Reevaluate: true}},
		{Modes{Bypass: true, Mesh: true}, Modes{Mesh: true}, Hygiene{Reevaluate: true, ResetFakeIP: true, FlushDNS: true, CloseStackInternet: true}},
		{Modes{Mesh: true}, Modes{Bypass: true, Mesh: true}, Hygiene{Reevaluate: true, ResetFakeIP: true, FlushDNS: true}},
		{Modes{Bypass: true, Mesh: true}, Modes{Bypass: true}, Hygiene{Reevaluate: true, FlushDNS: true, CloseMesh: true}},
		{Modes{Bypass: true}, Modes{Bypass: true, Mesh: true}, Hygiene{Reevaluate: true, FlushDNS: true}},
	}
	for _, c := range cases {
		if got := Transition(c.prev, c.next); got != c.want {
			t.Errorf("%s→%s: %+v, ожидалось %+v", c.prev.Name(), c.next.Name(), got, c.want)
		}
	}
}

func TestShutdownСнимаетВсё(t *testing.T) {
	d := &fakeDriver{gate: make(chan struct{})}
	m := NewMachine(d, Options{})
	m.Set(ptr(true), nil)
	waitFor(t, func() bool { return m.State().Link == "connecting" })
	m.Shutdown()
	if _, _, _, up := d.snapshot(); up {
		t.Fatal("после Shutdown связь осталась")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("не дождались")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
