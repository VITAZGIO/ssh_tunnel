package core

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// Driver — то, чем ядро управляет на конкретной платформе. Реализуют app.App
// (Windows, Linux: прокси или VPN) и mobile (Android). Решать «куда» драйверу
// нельзя: он только исполняет.
type Driver interface {
	// MeshAvailable — у текущего сервера настроена сеть устройств.
	MeshAvailable() bool
	// Connect поднимает связь с сервером: пул SSH и всё, что на нём едет.
	// Блокирует до успеха, ошибки или отмены ctx. После успеха ctx живёт,
	// пока связь не снимут (Disconnect), — на нём можно строить то, что
	// подключилось. Режим здесь не включается — это дело Apply.
	Connect(ctx context.Context) error
	// Disconnect снимает связь и всё, что на ней держится: адаптер, маршруты,
	// DNS, системный прокси. После него система — как без программы.
	// Вызывать можно сколько угодно раз.
	Disconnect()
	// Apply приводит систему к режиму m целиком, каким бы ни было прежнее
	// состояние: полный набор маршрутов и DNS, а не «добавь/убери кусочек».
	// h — гигиена перехода (см. Transition). Зовётся только при поднятой
	// связи и только с m.Any().
	Apply(m Modes, h Hygiene) error
	// Link — состояние связи с сервером для экрана (events.State*) и
	// подробность (адрес сервера).
	Link() (state, detail string)
}

// ErrNoMesh — кнопку «Сеть устройств» нажали, а у сервера сети нет.
var ErrNoMesh = errors.New("сеть устройств не настроена для этого сервера: включи её в настройках сервера")

// Options — необязательные связи Machine с остальной программой.
type Options struct {
	// Save запоминает выбор человека (обе кнопки) — для восстановления после
	// перезапуска. Зовётся только на действия человека.
	Save func(Want)
	// Changed — что-то поменялось в State: пора разослать экранам.
	Changed func()
	// ErrorKind — стабильный код причины ошибки подключения (auth,
	// no_response…), по нему экран выбирает текст на своём языке.
	ErrorKind func(error) string
}

// failure — почему кнопка выключилась сама.
type failure struct {
	err  error
	kind string
}

// Machine — две кнопки и единственное место, где желание человека
// превращается в состояние системы.
//
// Все переходы делает одна горутина (reconcile), по очереди: сколько бы
// кнопок ни нажали и откуда бы ни пришла смена сети, Apply не вызывается
// одновременно с самим собой и с Connect/Disconnect в разном порядке.
type Machine struct {
	drv Driver
	opt Options

	mu   sync.Mutex
	want Want
	errT *failure // почему выключилась круглая
	errM *failure // почему выключилась «Сеть устройств»

	linkUp  bool               // связь с сервером поднята
	linking bool               // идёт подключение
	cancel  context.CancelFunc // отменяет идущее подключение
	// linkCancel — контекст поднятой связи: платформа может жить на нём
	// (туннель строится от контекста подключения), поэтому он отменяется не
	// после Connect, а только когда связь снимают.
	linkCancel context.CancelFunc
	lastErr    error          // чем кончилась последняя попытка подключения
	connWG     sync.WaitGroup // горутина подключения (см. Shutdown)

	applied    Modes // что применено к системе
	hasApplied bool
	reapply    bool // применить заново, даже если режим тот же
	restart    bool // переподключиться (сменился сервер)

	pending  bool   // reconcile запрошен и ещё не прошёл
	stepping bool   // reconcile идёт прямо сейчас
	seq      uint64 // растёт с каждым изменением
	humanSeq uint64 // растёт с каждым действием человека
	modules  map[string]Module

	changed chan struct{} // закрывается на каждое изменение (см. WaitIdle)
	kick    chan struct{}
	quit    chan struct{}
	done    chan struct{}
}

// NewMachine запускает ядро. Обе кнопки выключены, пока не придёт Set или
// Restore.
func NewMachine(drv Driver, opt Options) *Machine {
	m := &Machine{
		drv:     drv,
		opt:     opt,
		modules: map[string]Module{},
		changed: make(chan struct{}),
		kick:    make(chan struct{}, 1),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go m.run()
	return m
}

// Set — человек нажал кнопку (или прислал «хочу» через API). nil — эту
// кнопку не трогать. Выбор запоминается (Options.Save).
func (m *Machine) Set(tunnel, mesh *bool) error {
	avail := m.drv.MeshAvailable()
	m.mu.Lock()
	var err error
	if mesh != nil && *mesh && !avail {
		m.errM = m.failureLocked(ErrNoMesh)
		mesh, err = nil, ErrNoMesh
	}
	if tunnel != nil {
		m.want.Tunnel = *tunnel
		m.errT = nil
	}
	if mesh != nil {
		m.want.Mesh = *mesh
		m.errM = nil
	}
	m.humanSeq++
	w := m.want
	m.mu.Unlock()
	if m.opt.Save != nil {
		m.opt.Save(w)
	}
	m.poke()
	return err
}

// Restore включает кнопки так, как решили настройки запуска (или как было
// при закрытии). Не запоминается и действием человека не считается. Сети
// устройств у сервера нет — эта кнопка просто не включается.
func (m *Machine) Restore(w Want) {
	if w.Mesh && !m.drv.MeshAvailable() {
		w.Mesh = false
	}
	m.mu.Lock()
	m.want = w
	m.errT, m.errM = nil, nil
	m.mu.Unlock()
	m.poke()
}

// Restart переподключается к серверу, сохраняя кнопки: сменился сервер или
// настройки соединения. Выключенное остаётся выключенным.
func (m *Machine) Restart() {
	m.mu.Lock()
	m.restart = true
	m.mu.Unlock()
	m.poke()
}

// Reapply применяет текущий режим заново, целиком: сменилась сеть, система
// проснулась, поменялись правила. Без смены режима гигиена сводится к
// пересчёту решений для живых соединений.
func (m *Machine) Reapply() {
	m.mu.Lock()
	m.reapply = true
	m.mu.Unlock()
	m.poke()
}

// LinkLost — связь с сервером пропала насовсем (не поднялся и запасной
// сервер). Включённые кнопки гаснут с этой ошибкой, а платформа снимает
// всё: интернет не должен остаться в мёртвом туннеле.
func (m *Machine) LinkLost(err error) {
	m.mu.Lock()
	if !m.linkUp {
		m.mu.Unlock()
		return
	}
	f := m.failureLocked(err)
	if m.want.Tunnel {
		m.want.Tunnel, m.errT = false, f
	}
	if m.want.Mesh {
		m.want.Mesh, m.errM = false, f
	}
	m.lastErr = err
	m.mu.Unlock()
	m.poke()
}

// Want — чего сейчас хочет человек.
func (m *Machine) Want() Want {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.want
}

// Applied — какой режим применён к системе; ok=false — никакой (связи нет).
func (m *Machine) Applied() (Modes, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applied, m.hasApplied
}

// LinkUp — связь с сервером поднята.
func (m *Machine) LinkUp() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.linkUp
}

// Busy — идёт подключение или переход ещё не применён.
func (m *Machine) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending || m.stepping || m.linking
}

// LastError — чем кончилась последняя попытка подключения (nil — успехом
// или её отменили).
func (m *Machine) LastError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// HumanSeq растёт с каждым нажатием человека: автозапуск по нему понимает,
// что решение уже принял человек, и перестаёт вмешиваться.
func (m *Machine) HumanSeq() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.humanSeq
}

// SetModule сообщает статус модуля для «Диагностики».
func (m *Machine) SetModule(mod Module) {
	m.mu.Lock()
	if old, ok := m.modules[mod.ID]; ok && old == mod {
		m.mu.Unlock()
		return
	}
	m.modules[mod.ID] = mod
	m.mu.Unlock()
	m.broadcast()
}

// State — всё для экрана.
func (m *Machine) State() State {
	link, detail := m.drv.Link()
	avail := m.drv.MeshAvailable()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := State{MeshAvailable: avail, Detail: detail, Seq: m.seq, Mode: ModeOff}
	switch {
	case m.linking:
		st.Link = "connecting"
	case m.linkUp:
		st.Link = link
	default:
		st.Link = "stopped"
		st.Detail = ""
	}
	if m.hasApplied {
		st.Mode = m.applied.Name()
	}
	st.Tunnel = button(m.want.Tunnel, m.hasApplied && m.applied.Bypass, m.errT)
	st.Mesh = button(m.want.Mesh, m.hasApplied && m.applied.Mesh, m.errM)
	ids := make([]string, 0, len(m.modules))
	for id := range m.modules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		st.Modules = append(st.Modules, m.modules[id])
	}
	return st
}

func button(want, on bool, f *failure) Button {
	b := Button{Want: want, Phase: PhaseOff}
	switch {
	case want && on:
		b.Phase = PhaseOn
	case want:
		b.Phase = PhaseConnecting
	case f != nil:
		b.Phase, b.Error, b.ErrorKind = PhaseError, f.err.Error(), f.kind
	}
	return b
}

// WaitIdle ждёт, пока ядро придёт в покой: подключение закончилось, режим
// применён. Нужен тем, кому результат нужен сразу (автозапуск, консоль,
// тесты); экран вместо этого слушает события.
func (m *Machine) WaitIdle(ctx context.Context) error {
	for {
		m.mu.Lock()
		idle := !m.pending && !m.stepping && !m.linking
		ch := m.changed
		m.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-m.done:
			return errors.New("ядро остановлено")
		}
	}
}

// Shutdown выключает всё (выбор человека при этом не запоминается) и
// останавливает ядро. Для выхода из программы.
func (m *Machine) Shutdown() {
	m.mu.Lock()
	m.want = Want{}
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	select {
	case <-m.quit:
	default:
		close(m.quit)
	}
	<-m.done
	// Подключение, отменённое выше, могло успеть подняться: дождаться его,
	// чтобы снять и его тоже.
	m.connWG.Wait()
	m.mu.Lock()
	up, linkCancel := m.linkUp, m.linkCancel
	m.linkUp, m.hasApplied, m.linkCancel = false, false, nil
	m.mu.Unlock()
	if up {
		m.drv.Disconnect()
	}
	if linkCancel != nil {
		linkCancel()
	}
	m.broadcast()
}

func (m *Machine) poke() {
	m.mu.Lock()
	m.pending = true
	m.mu.Unlock()
	select {
	case m.kick <- struct{}{}:
	default:
	}
	m.broadcast()
}

// broadcast будит WaitIdle и сообщает экранам об изменении.
func (m *Machine) broadcast() {
	m.mu.Lock()
	m.seq++
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
	if m.opt.Changed != nil {
		m.opt.Changed()
	}
}

func (m *Machine) failureLocked(err error) *failure {
	f := &failure{err: err}
	if m.opt.ErrorKind != nil {
		f.kind = m.opt.ErrorKind(err)
	}
	return f
}

func (m *Machine) run() {
	defer close(m.done)
	for {
		select {
		case <-m.quit:
			return
		case <-m.kick:
		}
		select {
		case <-m.quit:
			return
		default:
		}
		m.reconcile()
	}
}

// reconcile — один шаг от того, что есть, к тому, чего хотят. Пока он идёт,
// ядро не считается пришедшим в покой (см. WaitIdle).
func (m *Machine) reconcile() {
	m.mu.Lock()
	m.pending, m.stepping = false, true
	m.mu.Unlock()
	m.step()
	m.mu.Lock()
	m.stepping = false
	m.mu.Unlock()
	m.broadcast()
}

func (m *Machine) step() {
	avail := m.drv.MeshAvailable()

	m.mu.Lock()
	if m.want.Mesh && !avail {
		// Сеть устройств пропала у сервера (поменяли настройки) — кнопка
		// гаснет сама и говорит почему.
		m.want.Mesh = false
		m.errM = m.failureLocked(ErrNoMesh)
	}
	w := m.want
	need := w.Tunnel || w.Mesh

	if m.restart || !need {
		if m.linking {
			// Отмена идущего подключения; горутина подключения разбудит
			// снова, когда закончит.
			m.cancel()
			m.mu.Unlock()
			m.broadcast()
			return
		}
		up, linkCancel := m.linkUp, m.linkCancel
		m.linkUp, m.hasApplied, m.restart, m.linkCancel = false, false, false, nil
		m.mu.Unlock()
		if up {
			m.drv.Disconnect()
		}
		if linkCancel != nil {
			linkCancel()
		}
		if !need {
			m.broadcast()
			return
		}
		m.mu.Lock()
	}

	if !m.linkUp {
		if !m.linking {
			m.startConnectLocked()
		}
		m.mu.Unlock()
		m.broadcast()
		return
	}

	modes := Modes{Bypass: w.Tunnel, Mesh: w.Mesh}
	if m.hasApplied && m.applied == modes && !m.reapply {
		m.mu.Unlock()
		m.broadcast()
		return
	}
	prev, had := m.applied, m.hasApplied
	m.reapply = false
	m.mu.Unlock()

	h := Transition(prev, modes)
	if !had {
		// Первое применение на свежей связи: система сейчас как без нас —
		// снимать и пересчитывать нечего, но DNS-кэш сбросить надо.
		h = Hygiene{FlushDNS: true}
	}
	err := m.drv.Apply(modes, h)

	m.mu.Lock()
	if err == nil {
		m.applied, m.hasApplied = modes, true
		m.mu.Unlock()
		m.broadcast()
		return
	}
	f := m.failureLocked(err)
	changedB := !had || prev.Bypass != modes.Bypass
	changedM := !had || prev.Mesh != modes.Mesh
	turnedOff := false
	if modes.Bypass && changedB {
		m.want.Tunnel, m.errT, turnedOff = false, f, true
	}
	if modes.Mesh && changedM {
		m.want.Mesh, m.errM, turnedOff = false, f, true
	}
	if turnedOff {
		// Что применилось наполовину, неизвестно: следующий шаг применит
		// оставшийся режим целиком (или снимет всё).
		m.hasApplied = false
		m.mu.Unlock()
		m.poke()
		return
	}
	// Повторное применение того же режима не удалось (сменилась сеть, а
	// адаптер не перенастроился) — держим то, что было, и показываем ошибку.
	m.modules["apply"] = Module{ID: "apply", State: ModuleError, Text: err.Error()}
	m.mu.Unlock()
	m.broadcast()
}

// startConnectLocked запускает подключение в отдельной горутине: оно бывает
// долгим, а нажатие во время него должно срабатывать сразу (отмена).
func (m *Machine) startConnectLocked() {
	ctx, cancel := context.WithCancel(context.Background())
	m.linking, m.cancel = true, cancel
	m.lastErr = nil
	delete(m.modules, "apply")
	m.connWG.Add(1)
	go func() {
		defer m.connWG.Done()
		err := m.drv.Connect(ctx)
		cancelled := ctx.Err() != nil
		if err != nil {
			cancel()
		}

		m.mu.Lock()
		m.linking, m.cancel = false, nil
		switch {
		case err == nil:
			// Поднялась, даже если отмену нажали в последний миг: следующий
			// шаг снимет её, если она больше никому не нужна.
			m.linkUp, m.linkCancel = true, cancel
		case cancelled:
			// Отменили — это не ошибка подключения.
		default:
			m.lastErr = err
			f := m.failureLocked(err)
			if m.want.Tunnel {
				m.want.Tunnel, m.errT = false, f
			}
			if m.want.Mesh {
				m.want.Mesh, m.errM = false, f
			}
		}
		m.mu.Unlock()
		m.poke()
	}()
}
