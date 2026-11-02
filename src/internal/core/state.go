// Package core — ядро ssh_tunnel 2.0, общее для Windows, Linux и Android.
//
// Здесь живут три вещи, и больше нигде их быть не должно:
//
//   - две кнопки главного экрана и их состояние (Machine): круглая — туннель,
//     «Сеть устройств» — связь со своими устройствами. Кнопки независимы:
//     каждая меняет только своё;
//   - одно место применения состояния (Machine → Driver.Apply): любое
//     изменение — кнопка, правило, смена сети, пробуждение — приводит систему
//     к полному плану режима целиком, а не правит её кусочками;
//   - одно решение «куда» для каждого соединения (Decide): через сервер,
//     напрямую, в сеть устройств или никуда.
//
// Платформа (vpnlayer на Windows и Linux, служба на Android, системный прокси
// в обычной версии) только исполняет: поднимает адаптер, ставит маршруты и
// DNS, привязывает сокеты к настоящей сети, говорит, какая программа открыла
// соединение. Решать «куда» ей нельзя — см. CLAUDE.md в корне репозитория.
package core

// Фазы кнопки — то, что видит человек.
const (
	PhaseOff        = "off"        // выключена
	PhaseConnecting = "connecting" // включена, но ещё не работает: «Подключаюсь…»
	PhaseOn         = "on"         // включена и работает
	PhaseError      = "error"      // выключилась сама из-за ошибки (текст в Error)
)

// Четыре режима — сочетания двух кнопок.
const (
	ModeOff        = "off"         // «Выключено»: ничего нашего, трафик как без программы
	ModeMesh       = "mesh"        // «Только сеть устройств»: свои устройства, интернет напрямую
	ModeBypass     = "bypass"      // «Обход»: интернет по правилам, устройств нет
	ModeBypassMesh = "bypass+mesh" // «Обход + сеть устройств»: всё
)

// Modes — что включено на самом деле. Bypass — круглая кнопка (интернет по
// правилам через сервер), Mesh — сеть устройств.
type Modes struct {
	Bypass bool `json:"bypass"`
	Mesh   bool `json:"mesh"`
}

// Name — имя режима (одна из констант Mode*).
func (m Modes) Name() string {
	switch {
	case m.Bypass && m.Mesh:
		return ModeBypassMesh
	case m.Bypass:
		return ModeBypass
	case m.Mesh:
		return ModeMesh
	}
	return ModeOff
}

// Any — включено хоть что-то, значит нужна связь с сервером.
func (m Modes) Any() bool { return m.Bypass || m.Mesh }

// Want — чего хочет человек. Только это и запоминается между запусками.
type Want struct {
	Tunnel bool `json:"tunnel"`
	Mesh   bool `json:"mesh"`
}

// Button — состояние одной кнопки для экрана.
type Button struct {
	Want      bool   `json:"want"`
	Phase     string `json:"phase"`
	Error     string `json:"error,omitempty"`
	ErrorKind string `json:"errorKind,omitempty"`
}

// Состояния модуля на «Диагностике».
const (
	ModuleOK    = "ok"    // зелёный
	ModuleWarn  = "warn"  // жёлтый: работает, но не полностью
	ModuleError = "error" // красный
	ModuleOff   = "off"   // серый: выключен или не нужен в этом режиме
)

// Module — статус одной части программы (реклама, домены, UDP, точка
// доступа, сеть устройств, прямые соединения): цвет и короткий текст.
type Module struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Text  string `json:"text,omitempty"`
}

// State — всё, что нужно экрану, чтобы нарисовать главное. Интерфейс сам
// ничего не хранит: показывает это и шлёт обратно только «хочу».
type State struct {
	Tunnel Button `json:"tunnel"`
	Mesh   Button `json:"mesh"`
	// MeshAvailable — у сервера настроена сеть устройств; без неё кнопки
	// «Сеть устройств» на экране нет.
	MeshAvailable bool `json:"meshAvailable"`
	// Mode — режим, который действует прямо сейчас (Mode*).
	Mode string `json:"mode"`
	// Link — связь с сервером: stopped, connecting, connected, reconnecting,
	// error (см. events.State*). Detail — подробность (адрес сервера).
	Link   string `json:"link"`
	Detail string `json:"detail,omitempty"`
	// Modules — статусы модулей для «Диагностики», по ID.
	Modules []Module `json:"modules,omitempty"`
	// Seq растёт с каждым изменением: экран отбрасывает устаревшее.
	Seq uint64 `json:"seq"`
}
