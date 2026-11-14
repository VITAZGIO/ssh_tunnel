package tunnel

// MeshHost — клиент сети устройств, который живёт отдельно от пула SSH.
//
// Раньше клиент жил ровно столько, сколько пул: не поднялось первое
// SSH-соединение — не было и сети устройств, хотя сам клиент давно умеет
// работать без сервера (кэш на диске, прямые соединения, маяки в локальной
// сети). Теперь его время жизни задаёт кнопка «Сеть устройств»: владелец
// (App на ПК) зовёт Start, когда кнопка включена, и Stop, когда выключена.
// Пул к нему только подключается и отключается (Attach/Detach) — через шов
// ServerDial, по которому клиент ходит к meshd на сервере.
//
// Серверов сети может быть несколько (ТЗ 12): основной путь даёт туннель
// (Attach), остальные — лёгкие SSH-соединения только для сети устройств
// (AttachExtra, см. meshlinks.go). Клиент держит связь со всеми сразу.

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"sshtunnel/internal/mesh"
)

// ServerDial — как клиент сети устройств достаёт до meshd на сервере:
// соединение через SSH-пул. Пула нет или он сейчас без живых соединений —
// сразу ошибка, без ожидания: клиент сам повторит попытку со своей паузой.
type ServerDial func(network, addr string) (net.Conn, error)

// ErrNoServer — сейчас нет связи с сервером (пул не поднят или
// переподключается).
var ErrNoServer = errors.New("нет связи с сервером")

// meshIdentity — то, от чего зависит, тот же это клиент или нужен новый:
// сеть, устройство и всё, что клиент отдаёт серверу при входе. Via (адрес
// сервера) тоже: по нему идёт проверка NAT, и после перехода на запасной
// сервер клиент честно пересоздаётся (прежний сначала останавливается).
type meshIdentity struct {
	key, device, name, addr, mode, platform, os, version, hostname, stateDir, via string
	incoming, noDirect, noNAT                                                     bool
}

func identityOf(c *mesh.Config) meshIdentity {
	return meshIdentity{
		key: c.Key, device: c.DeviceID, name: c.Name, addr: c.Addr, mode: c.Mode,
		platform: c.Platform, os: c.OS, version: c.AppVersion, hostname: c.Hostname, stateDir: c.StateDir, via: c.Via,
		incoming: c.AllowIncoming, noDirect: c.NoDirect, noNAT: c.NoNATProbe,
	}
}

// MeshHost держит не больше одного клиента сети устройств.
type MeshHost struct {
	log func(level, text string)

	mu     sync.Mutex
	client *mesh.Client
	id     meshIdentity
	cancel context.CancelFunc
	done   chan struct{}

	// server — текущий путь к meshd; owner — кто его дал (туннель): Detach
	// от прежнего туннеля не должен снять путь, который уже дал новый.
	server ServerDial
	owner  any
	// extras — дополнительные серверы сети: ключ → путь.
	extras map[string]extraServer
}

type extraServer struct {
	via  string
	dial ServerDial
}

// NewMeshHost готовит пустого хозяина. log получает строки журнала ("info",
// "warn"); nil — молчать.
func NewMeshHost(log func(level, text string)) *MeshHost {
	if log == nil {
		log = func(string, string) {}
	}
	return &MeshHost{log: log}
}

// Start запускает клиента с настройками cfg. Клиент с теми же настройками уже
// работает — ничего не происходит (второй клиент на ту же сеть не появится
// никогда); настройки другие (другая сеть, имя, папка) — прежний
// останавливается и запускается новый. Возвращает работающего клиента.
func (h *MeshHost) Start(cfg mesh.Config) *mesh.Client {
	id := identityOf(&cfg)
	h.mu.Lock()
	if h.client != nil && h.id == id {
		c := h.client
		h.mu.Unlock()
		return c
	}
	h.mu.Unlock()
	h.Stop()

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil {
		// Пока останавливали прежнего, другой вызов уже запустил клиента.
		return h.client
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := mesh.New(cfg, h.dial, h.log)
	for key, e := range h.extras {
		c.AddServer(key, e.via, h.extraDial(key))
	}
	done := make(chan struct{})
	h.client, h.id, h.cancel, h.done = c, id, cancel, done
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	return c
}

// meshStopWait — сколько ждать, пока клиент закроет свои соединения.
// Опоздавший ничего не сломает: контекст отменён, он выйдет сам.
const meshStopWait = 5 * time.Second

// Stop останавливает клиента и ждёт, пока он выйдет. Безопасно звать
// сколько угодно раз.
func (h *MeshHost) Stop() {
	h.mu.Lock()
	cancel, done := h.cancel, h.done
	h.client, h.cancel, h.done = nil, nil, nil
	h.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(meshStopWait):
		h.log("warn", "Сеть устройств: клиент не остановился вовремя")
	}
}

// Client — работающий клиент; nil — сеть устройств выключена.
func (h *MeshHost) Client() *mesh.Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.client
}

// Running — клиент запущен.
func (h *MeshHost) Running() bool { return h.Client() != nil }

// Attach даёт клиенту путь к серверу. owner — кто его дал (обычно туннель).
func (h *MeshHost) Attach(owner any, d ServerDial) {
	h.mu.Lock()
	h.server, h.owner = d, owner
	h.mu.Unlock()
}

// Detach снимает путь к серверу, если его дал owner. Путь, который уже дал
// другой (новый туннель после перехода на запасной сервер), не трогается.
func (h *MeshHost) Detach(owner any) {
	h.mu.Lock()
	if h.owner == owner {
		h.server, h.owner = nil, nil
	}
	h.mu.Unlock()
}

// AttachExtra даёт клиенту путь к ещё одному серверу сети (key — как его
// назвать, обычно адрес из профиля; via — адрес сервера снаружи).
func (h *MeshHost) AttachExtra(key, via string, d ServerDial) {
	h.mu.Lock()
	if h.extras == nil {
		h.extras = map[string]extraServer{}
	}
	_, had := h.extras[key]
	h.extras[key] = extraServer{via: via, dial: d}
	c := h.client
	h.mu.Unlock()
	if c != nil && !had {
		c.AddServer(key, via, h.extraDial(key))
	}
}

// DetachExtra убирает дополнительный сервер.
func (h *MeshHost) DetachExtra(key string) {
	h.mu.Lock()
	_, had := h.extras[key]
	delete(h.extras, key)
	c := h.client
	h.mu.Unlock()
	if c != nil && had {
		c.RemoveServer(key)
	}
}

// Extras — ключи дополнительных серверов.
func (h *MeshHost) Extras() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.extras))
	for k := range h.extras {
		out = append(out, k)
	}
	return out
}

func (h *MeshHost) extraDial(key string) mesh.Dialer {
	return func(network, addr string) (net.Conn, error) {
		h.mu.Lock()
		e, ok := h.extras[key]
		h.mu.Unlock()
		if !ok || e.dial == nil {
			return nil, ErrNoServer
		}
		return e.dial(network, addr)
	}
}

// dial — шов, которым пользуется клиент: текущий путь или ErrNoServer.
func (h *MeshHost) dial(network, addr string) (net.Conn, error) {
	h.mu.Lock()
	d := h.server
	h.mu.Unlock()
	if d == nil {
		return nil, ErrNoServer
	}
	return d(network, addr)
}
