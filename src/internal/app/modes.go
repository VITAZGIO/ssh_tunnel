package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"sshtunnel/internal/config"
	"sshtunnel/internal/core"
	"sshtunnel/internal/events"
	"sshtunnel/internal/tunnel"
)

// Две кнопки главного экрана живут в ядре (core.Machine) и независимы:
// круглая — туннель (интернет по правилам через сервер), «Сеть устройств» —
// связь со своими устройствами. Здесь — только то, как настольная версия
// исполняет решения ядра: поднимает связь с сервером, применяет режим целиком
// (системный прокси или адаптер VPN) и снимает всё.

// ModeLayer — старый слой VPN, который умел только переключать «весь трафик»
// и «только сеть устройств». Новый слой реализует PlanLayer.
type ModeLayer interface {
	SetBypass(on bool)
}

// PlanLayer — слой VPN, который принимает режим целиком (core.PlanVPN) и
// гигиену перехода: так состояние после переключения равно чистому запуску.
type PlanLayer interface {
	Apply(tun *tunnel.Tunnel, p config.Profile, m core.Modes, h core.Hygiene) error
}

// sysProxyArgs — с чем включать системный прокси (адреса локальных прокси
// подключённого сервера).
type sysProxyArgs struct {
	cfg                 config.Config
	p                   config.Profile
	httpAddr, socksAddr string
}

// errNoMesh — кнопку «Сеть устройств» нажали, а сети у сервера нет.
var errNoMesh = core.ErrNoMesh

// meshConfigured — у активного сервера включена сеть устройств.
func (a *App) meshConfigured() bool {
	p := a.Config().Active()
	return p.MeshEnabled && p.MeshKey != ""
}

// Modes — что сейчас включено на самом деле (старый ответ /api/status).
type Modes struct {
	Bypass         bool `json:"bypass"`
	Mesh           bool `json:"mesh"`
	MeshConfigured bool `json:"meshConfigured"`
}

func (a *App) Modes() Modes {
	m, ok := a.machine.Applied()
	out := Modes{MeshConfigured: a.meshConfigured()}
	if ok {
		out.Bypass, out.Mesh = m.Bypass, m.Mesh
	}
	return out
}

// CoreState — всё для экрана: кнопки, режим, связь, модули.
func (a *App) CoreState() core.State { return a.machine.State() }

// Want — человек нажал кнопку (или прислал «хочу» через API). nil — эту
// кнопку не трогать. Возвращается сразу: ход подключения виден в состоянии
// и событиях. Ошибка — только если просьбу нельзя выполнить вовсе (сети
// устройств у сервера нет).
func (a *App) Want(tunnelOn, meshOn *bool) error { return a.machine.Set(tunnelOn, meshOn) }

// WantWait — то же, но ждёт, пока ядро придёт в покой, и возвращает ошибку
// подключения, если включаемое не поднялось. Для консоли, трея и тестов.
func (a *App) WantWait(tunnelOn, meshOn *bool) error {
	if err := a.machine.Set(tunnelOn, meshOn); err != nil {
		return err
	}
	return a.settle(tunnelOn, meshOn)
}

// settle ждёт покоя ядра и сообщает, почему не включилось то, что просили.
func (a *App) settle(tunnelOn, meshOn *bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := a.machine.WaitIdle(ctx); err != nil {
		return err
	}
	st := a.machine.State()
	for _, c := range []struct {
		asked *bool
		b     core.Button
	}{{tunnelOn, st.Tunnel}, {meshOn, st.Mesh}} {
		if c.asked != nil && *c.asked && c.b.Phase == core.PhaseError {
			if err := a.machine.LastError(); err != nil {
				return err
			}
			return errors.New(c.b.Error)
		}
	}
	return nil
}

func on() *bool  { t := true; return &t }
func off() *bool { f := false; return &f }

// SetBypass — круглая кнопка (старое имя, оставлено для API и тестов).
func (a *App) SetBypass(v bool) error { return a.WantWait(&v, nil) }

// SetMesh — кнопка «Сеть устройств» (старое имя).
func (a *App) SetMesh(v bool) error { return a.WantWait(nil, &v) }

// ToggleTunnel — значок в трее: включает или выключает только круглую
// кнопку, сеть устройств не трогает.
func (a *App) ToggleTunnel() {
	v := !a.machine.Want().Tunnel
	if err := a.WantWait(&v, nil); err != nil {
		a.Bus.Errorf("%v", err)
	}
}

// saveWant запоминает выбор человека для режима запуска «как при закрытии».
func (a *App) saveWant(w core.Want) {
	a.mu.Lock()
	if a.cfg.LastRunning == w.Tunnel && a.cfg.LastMesh == w.Mesh {
		a.mu.Unlock()
		return
	}
	a.cfg.LastRunning, a.cfg.LastMesh = w.Tunnel, w.Mesh
	cfg := a.cfg
	a.mu.Unlock()
	if err := cfg.Save(); err != nil {
		a.Bus.Errorf("не удалось запомнить состояние кнопок: %v", err)
	}
}

// publishState рассылает экранам новое состояние ядра — только если оно
// правда поменялось (номер изменения не в счёт).
func (a *App) publishState() {
	st := a.machine.State()
	seq := st.Seq
	st.Seq = 0
	b, _ := json.Marshal(st)
	a.stateMu.Lock()
	same := string(b) == a.lastState
	a.lastState = string(b)
	a.stateMu.Unlock()
	if same {
		return
	}
	st.Seq = seq
	a.Bus.Publish(events.Event{Kind: events.KindCore, Core: st})
}

// errorKind — стабильный код причины ошибки подключения для экрана.
func errorKind(err error) string {
	var ce *tunnel.ConnError
	if errors.As(err, &ce) {
		return string(ce.Kind)
	}
	return ""
}

// driver — App со стороны ядра (core.Driver). Отдельным типом, чтобы эти
// методы не путались с публичными: снаружи кнопки нажимают через Want.
type driver App

func (d *driver) app() *App { return (*App)(d) }

func (d *driver) MeshAvailable() bool { return d.app().meshConfigured() }

func (d *driver) Connect(ctx context.Context) error { return d.app().startLink(ctx) }

func (d *driver) Disconnect() {
	a := d.app()
	a.stop(!a.hardStop.Load())
}

func (d *driver) Link() (string, string) {
	a := d.app()
	a.mu.Lock()
	tun, detail := a.tun, a.linkDetail
	a.mu.Unlock()
	if tun == nil {
		return events.StateStopped, ""
	}
	return tun.State(), detail
}

// Apply приводит систему к режиму m целиком: адаптер VPN с полным планом
// маршрутов и DNS — или системный прокси в обычной версии. Затем гигиена:
// соединения, открытые по правилам прежнего режима, закрываются.
func (d *driver) Apply(m core.Modes, h core.Hygiene) error {
	a := d.app()
	a.mu.Lock()
	prev := a.modes
	tun, netLayer := a.tun, a.net
	p, _ := a.cfg.ProfileByID(a.effectiveProfile)
	a.mu.Unlock()
	if tun == nil {
		return errors.New("нет связи с сервером")
	}

	tun.SetBypass(m.Bypass)
	tun.SetMeshPaused(!m.Mesh)
	switch layer := netLayer.(type) {
	case nil:
		a.applySysProxy(m.Bypass)
	case PlanLayer:
		if err := layer.Apply(tun, p, m, h); err != nil {
			return err
		}
	default:
		if ml, ok := layer.(ModeLayer); ok {
			ml.SetBypass(m.Bypass)
		}
	}
	tun.ApplyHygiene(h)

	a.mu.Lock()
	a.modes = m
	a.mu.Unlock()
	a.logModeChange(prev, m)
	return nil
}

// logModeChange пишет в журнал, что включилось и что выключилось.
func (a *App) logModeChange(prev, m core.Modes) {
	if prev == m {
		return
	}
	switch {
	case m.Bypass && m.Mesh:
		a.Bus.Infof("Режим: обход + сеть устройств")
	case m.Bypass:
		a.Bus.Infof("Режим: обход (сеть устройств выключена)")
	case m.Mesh:
		a.Bus.Infof("Режим: только сеть устройств — сайты идут напрямую")
	}
}

// applySysProxy включает или снимает системный прокси обычной версии. Адреса
// берутся у подключённого сервера; после перехода на запасной они могут
// смениться — тогда прокси прописывается заново.
func (a *App) applySysProxy(want bool) {
	a.mu.Lock()
	args, sysOn, applied := a.sysProxyArgs, a.sysOn, a.sysApplied
	a.mu.Unlock()
	switch {
	case want && args != nil && (!sysOn || applied != args):
		a.enableSysProxy(args)
	case !want && sysOn:
		a.disableSysProxy()
	}
}

// disableSysProxy снимает системный прокси, не трогая туннель.
func (a *App) disableSysProxy() {
	if err := a.sys.Disable(); err != nil {
		a.Bus.Errorf("Не удалось вернуть настройки прокси: %v. Проверь: Параметры → Сеть и Интернет → Прокси-сервер", err)
		return
	}
	a.mu.Lock()
	a.sysOn, a.sysApplied = false, nil
	a.mu.Unlock()
	a.Bus.Infof("Системный прокси выключен")
}
