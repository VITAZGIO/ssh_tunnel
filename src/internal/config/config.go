// Package config — настройки приложения и их хранение на диске, чтобы не
// вводить одну и ту же команду с флагами каждый раз.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Profile — один сервер: всё, что относится именно к нему, а не к программе
// в целом. Их может быть несколько — как вкладки в браузере, — и переключение
// между ними не трогает язык интерфейса или общие галочки автозапуска.
type Profile struct {
	ID string `json:"id"`
	// Name — подпись на вкладке. Либо название выбранного из списка города
	// («Амстердам»), либо то, что человек вписал сам.
	Name string `json:"name"`
	// Flag — эмодзи флага страны у выбранного города. Пусто — показывать
	// вместо флага логотип программы: так помечен сервер с произвольным
	// именем, для которого страну никто не выбирал.
	Flag string `json:"flag,omitempty"`

	Host    string `json:"host"`
	SSHPort int    `json:"sshPort"`
	User    string `json:"user"`
	KeyPath string `json:"keyPath"`

	SocksPort int `json:"socksPort"`
	HTTPPort  int `json:"httpPort"`

	// PoolSize — сколько параллельных SSH-соединений держать. Одно соединение
	// упирается в одно TCP-окно и в один поток шифрования, поэтому несколько
	// заметно поднимают скорость на дальнем канале.
	PoolSize int `json:"poolSize"`

	// FilterMode и FilterApps — какие программы пускать через туннель:
	//   all    — все;
	//   only   — только перечисленные, остальные напрямую;
	//   except — все, кроме перечисленных.
	FilterMode string   `json:"filterMode"`
	FilterApps []string `json:"filterApps"`

	// DirectHosts — адреса, сети и имена, которые всегда идут напрямую,
	// помимо встроенного списка локальных диапазонов. Сюда вписывают чужие
	// сети: mesh-VPN, рабочий VPN, самодельный WireGuard.
	DirectHosts []string `json:"directHosts"`

	// TunnelHosts — обратный список: имена и сети, которые всегда идут через
	// сервер, даже если программа по правилам ходит напрямую или имя попало
	// под «всегда напрямую» (например, поддомен своего домена). Совпадение
	// здесь сильнее обоих правил.
	TunnelHosts []string `json:"tunnelHosts,omitempty"`

	// LocalViaTunnel — вести ли локальную сеть (192.168.x.x, домашние имена)
	// через сервер. По умолчанию false: такие адреса идут напрямую, иначе
	// домашние сервисы становятся недоступны при включённом туннеле.
	LocalViaTunnel bool `json:"localViaTunnel"`

	// Panel/ClientID/DeviceName заполняются только при импорте конфига,
	// выданного веб-панелью на VPS (internal/share, поля версии 2 формата
	// обмена). У сервера, настроенного руками, Panel остаётся пустым — по
	// нему экран настроек решает, показывать ли строку «Этот сервер выдан
	// панелью».
	Panel      string `json:"panel,omitempty"`
	ClientID   string `json:"clientId,omitempty"`
	DeviceName string `json:"deviceName,omitempty"`

	// UDPRelayEnabled — пробрасывать ли UDP (звонки, игры, QUIC) через
	// ретранслятор на этом сервере вместо молчаливого отказа. По умолчанию
	// выключено: пока ретранслятор не установлен на сервере (см. мастер
	// настройки VPS), включать нечего — соединение до него просто не
	// поднимется, и поведение останется прежним.
	UDPRelayEnabled bool `json:"udpRelayEnabled"`

	// Сеть устройств (см. internal/mesh, cmd/meshd): устройства с одним
	// ключом сети на этом сервере видят друг друга по именам «имя.mesh».
	// MeshName — имя этого устройства; пусто — имя компьютера.
	// MeshIncoming — пускать ли к своим службам соединения от других
	// устройств сети.
	MeshEnabled  bool   `json:"meshEnabled,omitempty"`
	MeshKey      string `json:"meshKey,omitempty"`
	MeshName     string `json:"meshName,omitempty"`
	MeshIncoming bool   `json:"meshIncoming,omitempty"`
	// MeshDirectOff — не пробовать прямые соединения между устройствами
	// (см. internal/mesh/p2p.go): всё только через сервер.
	MeshDirectOff bool `json:"meshDirectOff,omitempty"`
	// MeshOnlyThis — сеть устройств только через этот сервер: не держать
	// связь с другими серверами той же сети (см. MeshServers).
	MeshOnlyThis bool `json:"meshOnlyThis,omitempty"`

	// Тонкая настройка соединения (экран «Дополнительно»). Ноль и пустая
	// строка — значения по умолчанию, поэтому старые конфиги не меняются.
	//
	// KeepAliveSec — как часто проверять, жива ли связь с сервером (0 — 20 с).
	// ConnectTimeoutSec — сколько ждать подключения к серверу (0 — 20 с).
	// Cipher — какой шифр предлагать первым: "" (сам выбор), "aes", "chacha".
	// IPVersion — по какому протоколу идти к серверу: "" (любой), "4", "6".
	// ShareLAN — пускать на локальные прокси другие устройства домашней сети.
	KeepAliveSec      int    `json:"keepAliveSec,omitempty"`
	ConnectTimeoutSec int    `json:"connectTimeoutSec,omitempty"`
	Cipher            string `json:"cipher,omitempty"`
	IPVersion         string `json:"ipVersion,omitempty"`
	ShareLAN          bool   `json:"shareLan,omitempty"`

	// ExitVia — «Выход через» (цепочка выхода, ТЗ 8): id других профилей
	// по порядку, от 0 до MaxExitHops. Подключаемся к этому серверу, а в
	// интернет выходим только через последний из списка; он недоступен —
	// интернета нет, через этот сервер трафик не выпускается. Свои цепочки
	// у серверов из списка не действуют — действует цепочка того профиля,
	// к которому подключаемся.
	ExitVia []string `json:"exitVia,omitempty"`
	// ExitT — когда задали цепочку (unix); ExitBy — имя сервера, чья панель
	// её задала (ТЗ 16); пусто — человек на этом устройстве. Правка новее
	// побеждает: панель — подсказка, а не замок (см. ApplyExitOrder).
	ExitT  int64  `json:"exitT,omitempty"`
	ExitBy string `json:"exitBy,omitempty"`
}

// MaxExitHops — сколько серверов можно поставить в «Выход через».
const MaxExitHops = 2

// ExitNeedPrefix — звено цепочки из панели, для которого на устройстве нет
// профиля: в ExitVia лежит "need:<адрес:порт>" вместо id. Туннель с таким
// звеном не стартует и говорит «нужен профиль сервера …»; появится профиль
// с этим адресом — звено станет его id само (normalize).
const ExitNeedPrefix = "need:"

// ServerKey — как сервер профиля называется для других устройств и панелей
// (ТЗ 16): «адрес:порт» SSH, адрес — маленькими буквами. Id профиля у
// каждого устройства свой, а этот ключ одинаков везде, где есть профиль
// этого сервера; те же адрес и порт несёт цепочка в файле обмена (ТЗ 8).
func ServerKey(p Profile) string {
	host := strings.ToLower(strings.TrimSpace(p.Host))
	if host == "" {
		return ""
	}
	port := p.SSHPort
	if port <= 0 {
		port = 22
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// ProfileByKey — профиль с ключом ServerKey == key (первый по списку: два
// профиля одного сервера под разными пользователями — редкость).
func (c Config) ProfileByKey(key string) (Profile, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, p := range c.Profiles {
		if key != "" && ServerKey(p) == key {
			return p, true
		}
	}
	return Profile{}, false
}

// ExitChain — серверы цепочки выхода профиля p по порядку. Профиль, которого
// больше нет в списке (удалили), возвращается пустым, с одним id и
// именем-пометкой: туннель с такой цепочкой не стартует — молча укоротить
// цепочку значило бы выпустить интернет не там, где просили. Так же звено из
// панели без профиля (ExitNeedPrefix): имя — «нужен профиль сервера …».
func (c Config) ExitChain(p Profile) []Profile {
	var out []Profile
	for _, id := range p.ExitVia {
		hop, ok := c.ProfileByID(id)
		if key, need := strings.CutPrefix(id, ExitNeedPrefix); need {
			hop = Profile{ID: id, Name: "нужен профиль сервера " + key}
		} else if !ok {
			hop = Profile{ID: id, Name: "удалённый сервер"}
		}
		out = append(out, hop)
	}
	return out
}

// ExitMissing — адреса звеньев цепочки p, для которых нет профиля.
func (c Config) ExitMissing(p Profile) []string {
	var out []string
	for _, id := range p.ExitVia {
		if key, need := strings.CutPrefix(id, ExitNeedPrefix); need {
			out = append(out, key)
		}
	}
	return out
}

// ExitOrder — цепочка выхода из панели сервера (ТЗ 16): вход Entry, выход
// через Via (ключи ServerKey), время T и имя сервера By.
type ExitOrder struct {
	Entry string
	Via   []string
	T     int64
	By    string
}

// ApplyExitOrder применяет цепочку из панели так, как если бы человек сам
// выставил «Выход через» у профиля Entry, — если она новее того, что там
// есть (ExitT): старый приказ, пришедший позже нового (другая панель,
// переподключение), ничего не меняет. Звено без профиля становится
// ExitNeedPrefix. Возвращает, изменился ли профиль, и что сказать человеку.
func (c *Config) ApplyExitOrder(o ExitOrder) (changed bool, note string, err error) {
	entry, ok := c.ProfileByKey(o.Entry)
	if !ok {
		return false, "", fmt.Errorf("панель сервера %s задала цепочку для сервера %s, а его профиля на этом устройстве нет", o.By, o.Entry)
	}
	if o.T <= entry.ExitT {
		return false, "", nil
	}
	if len(o.Via) > MaxExitHops {
		return false, "", fmt.Errorf("цепочка из панели длиннее %d серверов", MaxExitHops)
	}
	var via, missing []string
	for _, k := range o.Via {
		k = strings.ToLower(strings.TrimSpace(k))
		hop, ok := c.ProfileByKey(k)
		switch {
		case k == "":
			return false, "", errors.New("пустой сервер в цепочке из панели")
		case ok && hop.ID == entry.ID:
			return false, "", errors.New("сервер не может выходить в интернет через самого себя")
		case ok:
			via = append(via, hop.ID)
		default:
			via = append(via, ExitNeedPrefix+k)
			missing = append(missing, k)
		}
	}
	entry.ExitVia, entry.ExitT, entry.ExitBy = via, o.T, o.By
	c.SetProfile(entry)
	if len(missing) > 0 {
		note = "нужен профиль сервера " + strings.Join(missing, ", ") + " — без него интернет через цепочку не пойдёт"
	}
	return true, note, nil
}

// TouchExit — человек на устройстве сам поменял цепочку профиля p: время
// правки не меньше прошлого+1, чтобы она побеждала приказ панели, на который
// отвечает, даже если часы устройства отстают.
func TouchExit(p *Profile, now int64) {
	if now <= p.ExitT {
		now = p.ExitT + 1
	}
	p.ExitT, p.ExitBy = now, ""
}

// Границы тонкой настройки: за ними значения уже вредят, а не помогают.
const (
	MinKeepAliveSec      = 5
	MaxKeepAliveSec      = 300
	MinConnectTimeoutSec = 3
	MaxConnectTimeoutSec = 120
)

// clampOrZero оставляет ноль нулём (значение по умолчанию), остальное
// загоняет в границы.
func clampOrZero(v, lo, hi int) int {
	switch {
	case v <= 0:
		return 0
	case v < lo:
		return lo
	case v > hi:
		return hi
	}
	return v
}

// Config — вся программа целиком: список серверов и настройки, общие для
// всех них (язык интерфейса, автозапуск, системный прокси).
type Config struct {
	// Profiles — серверы, между которыми можно переключаться. Пустым не
	// бывает: normalize() следит, чтобы в нём был хотя бы один.
	Profiles []Profile `json:"profiles"`
	// ActiveProfile — id сервера, который сейчас используется для
	// подключения. Переключение между вкладками в настройках его не меняет —
	// только нажатие «Выбрать этот сервер».
	ActiveProfile string `json:"activeProfile"`

	// SysProxy — прописывать ли системный прокси Windows (реестр).
	SysProxy bool `json:"sysProxy"`
	// SetEnvVars — прописывать ли HTTPS_PROXY/HTTP_PROXY/NO_PROXY в переменные
	// среды пользователя. Нужно для программ на Node.js/Python/Go (Claude Code,
	// npm, pip, curl), которые системный прокси Windows не читают вовсе.
	SetEnvVars bool `json:"setEnvVars"`

	// Verbose — подробный лог (включая закрытие соединений).
	Verbose bool `json:"verbose"`
	// AutoStart — при запуске GUI (Windows) сразу поднимать туннель, без
	// нажатия на кнопку. К консольной версии для Linux отношения не имеет —
	// там за это отвечает AutoConnect.
	AutoStart bool `json:"autoStart"`

	// AutoConnect — соединяться сразу при запуске программы (ssh_tunnel_linux
	// с флагом -web): без этого пришлось бы каждый раз самому нажимать
	// «Подключить» в панели после перезапуска. Указатель, а не bool: nil
	// означает «не задано явно», и тогда действует прежнее поведение —
	// программа подключается сразу, как было до появления этой настройки.
	AutoConnect *bool `json:"autoConnect,omitempty"`

	// StartMode — что делать с туннелем при запуске программы (кнопки в
	// настройках): "on" — включать, "mesh" — включать только сеть устройств
	// (обход блокировок выключен), "off" — не включать, "last" — как было
	// при закрытии (см. LastRunning). Пусто — конфиг старой версии, тогда
	// решают прежние галочки AutoStart (Windows) и AutoConnect (Linux).
	StartMode string `json:"startMode,omitempty"`
	// LastRunning — был ли туннель включён после последнего включения или
	// выключения руками. Остановка при выходе из программы его не меняет,
	// иначе «как при закрытии» всегда означало бы «выключен».
	LastRunning bool `json:"lastRunning,omitempty"`

	// MeshStartMode — то же для сети устройств, отдельно от обхода
	// блокировок: "on", "last" (см. LastMesh) или "off". Туннель
	// поднимается, если нужен хоть кому-то из двух. Пусто — сеть
	// повторяет выбор туннеля, как было до появления настройки.
	MeshStartMode string `json:"meshStartMode,omitempty"`
	// LastMesh — была ли включена сеть устройств при последнем действии
	// человека (для MeshStartMode "last").
	LastMesh bool `json:"lastMesh,omitempty"`

	// UILock — галка «Вход по паролю» (Дополнительно): вход в панель с
	// другого устройства (браузер по локальной сети, Linux) только после
	// пароля. Действует, когда пароль уже задан (UIPassHash); окну самой
	// программы пароль не нужен. На телефоне — отпечаток или PIN телефона.
	UILock bool `json:"uiLock,omitempty"`
	// UIPassHash — bcrypt пароля входа; ставится только через /api/uipass,
	// наружу (GET /api/config) не отдаётся.
	UIPassHash string `json:"uiPassHash,omitempty"`

	// Theme — оформление: "" (оригинал), "dark", "light", "console".
	Theme string `json:"theme,omitempty"`
	// Background — фон главного экрана: "" (ровный цвет), "code" (падающие
	// символы), "author" (сетка логотипов).
	Background string `json:"background,omitempty"`

	// AdBlockEnabled — блокировать рекламу и слежку по DNS. Работает только в
	// режиме VPN (ssh_tunnel_vpn): там DNS всей системы идёт через адаптер.
	// В режиме прокси имена разрешают сами программы — перехватывать нечего.
	// AdBlockSources — откуда брать списки (ссылки или файлы, по одному на
	// строку), AdBlockAllowlist — имена, которые не блокируются никогда.
	// Сам список лежит рядом с настройками, в adblock.list.
	// HotspotDirect — НЕ раздавать VPN на мобильную точку доступа Windows
	// (vpnlayer/hotspot_windows.go). По умолчанию раздаётся.
	HotspotDirect bool `json:"hotspotDirect,omitempty"`

	// FakeIP — галочка «Подставные адреса (fake-IP)» режима VPN: наш DNS
	// отвечает на имена «через сервер» выдуманными адресами 198.18.x.x, как
	// до ТЗ 9. По умолчанию выключена — программы получают настоящие адреса,
	// разрешённые через сервер (core.DecideDNS). RealIPHosts — «всегда
	// настоящий адрес»: имена, которым подставной не выдаётся и с галочкой
	// (программы, которые проверяют адрес перед соединением).
	FakeIP      bool     `json:"fakeIP,omitempty"`
	RealIPHosts []string `json:"realIPHosts,omitempty"`

	AdBlockEnabled   bool   `json:"adBlockEnabled,omitempty"`
	AdBlockSources   string `json:"adBlockSources,omitempty"`
	AdBlockAllowlist string `json:"adBlockAllowlist,omitempty"`

	// Language — язык интерфейса панели: "ru" или "en".
	Language string `json:"language"`
	// ShowServerPicker — показывать ли на главном экране виджет с текущим
	// сервером и быстрым переключением. Имеет смысл только когда серверов
	// больше одного — при одном сервере поле хранится, но панель его прячет.
	ShowServerPicker bool `json:"showServerPicker"`

	// AutoPickFastest — при подключении измерить отклик до SSH-порта каждого
	// сервера и соединяться с самым быстрым вместо обязательно ActiveProfile;
	// плюс переход на следующий по отклику, если выбранный не поднялся или
	// упал (см. app.Start). По умолчанию false: выбор сервера вручную не
	// должен быть неожиданно проигнорирован. Имеет смысл только при
	// нескольких серверах.
	AutoPickFastest bool `json:"autoPickFastest"`

	// MCPEnabled — галочка «Управление нейросетью (MCP)»: пока выключена,
	// `ssh_tunnel mcp` ничего не делает (internal/apicli). По умолчанию
	// выключена: нейросеть получает кнопки только по явному согласию.
	MCPEnabled bool `json:"mcpEnabled,omitempty"`
}

// AutoConnectEnabled — эффективное значение AutoConnect с учётом отсутствия
// настройки в старых конфигах (см. комментарий у поля).
func (c Config) AutoConnectEnabled() bool {
	return c.AutoConnect == nil || *c.AutoConnect
}

// Режимы StartMode.
const (
	StartOn   = "on"
	StartMesh = "mesh"
	StartOff  = "off"
	StartLast = "last"
)

// ConnectOnStart — поднимать ли туннель при запуске программы: ради обхода
// блокировок или ради сети устройств. legacy — ответ прежней галочки этой
// платформы; он действует, пока режим не выбран явно.
func (c Config) ConnectOnStart(legacy bool) bool {
	return c.BypassOnStart(legacy) || c.MeshOnStart(legacy)
}

// BypassOnStart — включать ли при запуске обход блокировок.
func (c Config) BypassOnStart(legacy bool) bool {
	switch c.StartMode {
	case StartOn:
		return true
	case StartOff, StartMesh:
		return false
	case StartLast:
		return c.LastRunning
	}
	return legacy
}

// MeshOnStart — включать ли при запуске сеть устройств. Сама настройка
// сети у сервера здесь не проверяется — это дело того, кто подключает.
func (c Config) MeshOnStart(legacy bool) bool {
	switch c.MeshStartMode {
	case StartOn:
		return true
	case StartOff:
		return false
	case StartLast:
		return c.LastMesh
	}
	if c.StartMode == StartMesh {
		return true
	}
	return c.BypassOnStart(legacy)
}

// Active — сервер, который сейчас используется для подключения. Если
// ActiveProfile почему-то не указывает ни на один известный профиль (файл
// правили руками, id устарел), возвращается первый — так подключаться есть
// куда, а не в никуда.
func (c Config) Active() Profile {
	for _, p := range c.Profiles {
		if p.ID == c.ActiveProfile {
			return p
		}
	}
	if len(c.Profiles) > 0 {
		return c.Profiles[0]
	}
	return defaultProfile(1)
}

// MeshServers — другие серверы сети устройств профиля p (ТЗ 12): профили
// с тем же ключом сети и включённой сетью, кроме самого p и профилей с тем
// же адресом. Список серверов сети — это и есть профили: чтобы добавить
// сервер в сеть, на устройстве заводят (или импортируют) его профиль с тем
// же ключом. Вход на каждый — свой: ключ, пользователь, проверка ключа
// сервера — как у обычного профиля. У p с MeshOnlyThis — пусто.
func (c Config) MeshServers(p Profile) []Profile {
	if !p.MeshEnabled || p.MeshKey == "" || p.MeshOnlyThis {
		return nil
	}
	seen := map[string]bool{strings.ToLower(p.Host) + ":" + strconv.Itoa(p.SSHPort): true}
	var out []Profile
	for _, q := range c.Profiles {
		addr := strings.ToLower(q.Host) + ":" + strconv.Itoa(q.SSHPort)
		if q.ID == p.ID || !q.MeshEnabled || q.MeshKey != p.MeshKey || q.Host == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, q)
	}
	return out
}

// ProfileByID ищет профиль по id.
func (c Config) ProfileByID(id string) (Profile, bool) {
	for _, p := range c.Profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// SetProfile заменяет профиль с тем же id (по ключу p.ID). Профиля с таким id
// нет — ничего не делает: добавление нового сервера идёт через AddProfile, а
// не через подмену несуществующего.
func (c *Config) SetProfile(p Profile) {
	for i := range c.Profiles {
		if c.Profiles[i].ID == p.ID {
			c.Profiles[i] = p
			return
		}
	}
}

// AddProfile добавляет новый сервер и возвращает его.
func (c *Config) AddProfile(name, flag string) Profile {
	p := defaultProfile(len(c.Profiles) + 1)
	if name != "" {
		p.Name = name
	}
	p.Flag = flag
	c.Profiles = append(c.Profiles, p)
	return p
}

// RemoveProfile удаляет сервер. Последний удалить нельзя — программе всегда
// нужен хотя бы один, иначе подключаться попросту не к чему. Если удаляли
// активный, активным становится первый из оставшихся.
func (c *Config) RemoveProfile(id string) bool {
	if len(c.Profiles) <= 1 {
		return false
	}
	out := c.Profiles[:0]
	for _, p := range c.Profiles {
		if p.ID != id {
			out = append(out, p)
		}
	}
	if len(out) == len(c.Profiles) {
		return false // такого id и не было
	}
	c.Profiles = out
	if c.ActiveProfile == id {
		c.ActiveProfile = c.Profiles[0].ID
	}
	return true
}

func Default() Config {
	p := defaultProfile(1)
	return Config{
		Profiles:      []Profile{p},
		ActiveProfile: p.ID,
		SysProxy:      true,
		SetEnvVars:    true,
		Language:      "ru",
	}
}

// NewProfile — новый сервер с разумными значениями по умолчанию и своим id,
// но не привязанный к списку: нужен импорту всех настроек, который собирает
// список серверов заново, а не дописывает по одному (см.
// internal/webui/settingsbundle.go).
func NewProfile(name, flag string) Profile {
	p := defaultProfile(1)
	if name != "" {
		p.Name = name
	}
	p.Flag = flag
	return p
}

func defaultProfile(n int) Profile {
	name := "Сервер " + itoa(n)
	return Profile{
		ID:   newProfileID(),
		Name: name,
		// Не root: у пользователя, заведённого только для туннеля, нет прав
		// ни на что, кроме проброса соединений. Если такого пользователя на
		// сервере нет, его создаст команда из подсказки у поля с ключом.
		User:       "tunnel",
		KeyPath:    DetectKeyPath(),
		SSHPort:    22,
		SocksPort:  1080,
		HTTPPort:   1081,
		PoolSize:   4,
		FilterMode: "all",
	}
}

func newProfileID() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand практически никогда не подводит, но id обязан быть хоть
		// каким-то: при сбое берём то, что успело записаться (нули), это не
		// нарушит уникальность в пределах одного процесса.
	}
	return "p_" + hex.EncodeToString(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Dir — папка с настройками: %APPDATA%\ssh_tunnel на Windows,
// ~/.config/ssh_tunnel на остальных системах.
func Dir() string {
	return filepath.Join(baseDir(), "ssh_tunnel")
}

func baseDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		if home == "" {
			// Служба systemd без User= запускается без $HOME: тогда путь вышел
			// бы относительным («.config» от корня диска), программа не нашла
			// бы своих настроек и падала по кругу. Домашняя папка — из списка
			// пользователей системы.
			if u, err := user.Current(); err == nil {
				home = u.HomeDir
			}
		}
		base = filepath.Join(home, ".config")
	}
	return base
}

// migrateOldDir переносит настройки из папки прежнего названия. Без этого
// после переименования программы человек потерял бы адрес сервера и путь к
// ключу и решил бы, что всё сломалось.
func migrateOldDir() {
	newDir := Dir()
	if _, err := os.Stat(newDir); err == nil {
		return // уже переехали
	}
	// Программа успела дважды сменить имя: vpstunnel -> ssh_tunel -> ssh_tunnel.
	// Настройки надо подобрать от любого из прежних, начиная с самого свежего.
	for _, old := range []string{"ssh_tunel", "vpstunnel"} {
		oldDir := filepath.Join(baseDir(), old)
		if _, err := os.Stat(oldDir); err != nil {
			continue
		}
		if os.Rename(oldDir, newDir) == nil {
			return
		}
	}
}

func Path() string { return filepath.Join(Dir(), "config.json") }

// DeviceID — постоянный id этого устройства для сети устройств: по нему
// сервер узнаёт устройство и выдаёт ему тот же адрес. Хранится отдельным
// файлом, а не в config.json: настройки переносят между устройствами
// (экспорт, перенос всех настроек), а id у каждого устройства должен быть свой.
func DeviceID() string {
	path := filepath.Join(Dir(), "device_id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); len(id) >= 16 {
			return id
		}
	}
	var raw [16]byte
	rand.Read(raw[:])
	id := base64.RawURLEncoding.EncodeToString(raw[:])
	if err := os.MkdirAll(Dir(), 0o700); err == nil {
		os.WriteFile(path, []byte(id+"\n"), 0o600)
	}
	return id
}

// DetectKeyPath ищет ключ SSH в домашней папке ТЕКУЩЕГО пользователя.
//
// Путь не зашит и не угадывается по имени: домашняя папка берётся у системы,
// поэтому на любом компьютере и под любым пользователем он свой. Из
// стандартных имён берётся первое существующее — у людей встречаются и
// ed25519, и ecdsa, и старый rsa.
func DetectKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".ssh")
	names := []string{"id_ed25519", "id_ecdsa", "id_rsa"}
	for _, n := range names {
		p := filepath.Join(dir, n)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	// Ничего нет — показываем путь, по которому ключ появится после создания.
	return filepath.Join(dir, "id_ed25519")
}

func KnownHostsPath() string { return filepath.Join(Dir(), "known_hosts") }

// legacyConfig — формат файла настроек до появления нескольких серверов:
// один плоский набор полей вместо списка профилей. Нужен только для чтения
// старых config.json при первом запуске новой версии.
type legacyConfig struct {
	Host           string   `json:"host"`
	SSHPort        int      `json:"sshPort"`
	User           string   `json:"user"`
	KeyPath        string   `json:"keyPath"`
	SocksPort      int      `json:"socksPort"`
	HTTPPort       int      `json:"httpPort"`
	PoolSize       int      `json:"poolSize"`
	SysProxy       bool     `json:"sysProxy"`
	SetEnvVars     bool     `json:"setEnvVars"`
	FilterMode     string   `json:"filterMode"`
	FilterApps     []string `json:"filterApps"`
	DirectHosts    []string `json:"directHosts"`
	LocalViaTunnel bool     `json:"localViaTunnel"`
	Verbose        bool     `json:"verbose"`
	AutoStart      bool     `json:"autoStart"`
	AutoConnect    *bool    `json:"autoConnect,omitempty"`
}

// migrateLegacy собирает Config из файла старого формата: одного сервера
// хватало на всю программу, теперь он становится первым (и единственным)
// профилем, а настройки самой программы переезжают как были.
func migrateLegacy(data []byte) Config {
	var lc legacyConfig
	_ = json.Unmarshal(data, &lc)
	p := defaultProfile(1)
	p.Host, p.User, p.KeyPath = lc.Host, lc.User, lc.KeyPath
	p.SSHPort, p.SocksPort, p.HTTPPort, p.PoolSize = lc.SSHPort, lc.SocksPort, lc.HTTPPort, lc.PoolSize
	p.FilterMode, p.FilterApps = lc.FilterMode, lc.FilterApps
	p.DirectHosts, p.LocalViaTunnel = lc.DirectHosts, lc.LocalViaTunnel

	return Config{
		Profiles:      []Profile{p},
		ActiveProfile: p.ID,
		SysProxy:      lc.SysProxy,
		SetEnvVars:    lc.SetEnvVars,
		Verbose:       lc.Verbose,
		AutoStart:     lc.AutoStart,
		AutoConnect:   lc.AutoConnect,
		Language:      "ru",
	}
}

// Load читает конфиг с диска. Отсутствие файла — не ошибка: возвращаются
// значения по умолчанию.
func Load() Config {
	migrateOldDir()
	cfg := Default()
	data, err := os.ReadFile(Path())
	if err != nil {
		return cfg
	}

	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return cfg // битый конфиг не должен ронять запуск
	}
	if _, hasProfiles := raw["profiles"]; hasProfiles {
		_ = json.Unmarshal(data, &cfg)
	} else {
		cfg = migrateLegacy(data)
	}
	cfg.normalize()
	return cfg
}

// Parse разбирает снимок настроек (профиль приложения, см. appprofiles) так
// же, как Load разбирает config.json: недостающее — по умолчанию, заведомо
// нерабочее — исправлено.
func Parse(data []byte) (Config, error) {
	cfg := Default()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	cfg.normalize()
	return cfg, nil
}

func (c *Config) Save() error {
	c.normalize()
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), data, 0o600)
}

// normalize чинит заведомо нерабочие значения, которые могли прийти из
// руками правленного JSON или из формы настроек.
func (c *Config) normalize() {
	if len(c.Profiles) == 0 {
		c.Profiles = []Profile{defaultProfile(1)}
	}
	for i := range c.Profiles {
		c.Profiles[i].normalize(i + 1)
	}
	if _, ok := c.ProfileByID(c.ActiveProfile); !ok {
		c.ActiveProfile = c.Profiles[0].ID
	}
	for i := range c.Profiles {
		c.Profiles[i].ExitVia = c.cleanExitVia(c.Profiles[i])
	}
	if c.Language != "en" {
		c.Language = "ru"
	}
	// Прежний режим «Только сеть» — теперь это два отдельных выбора:
	// туннель не включать, сеть устройств включать.
	if c.StartMode == StartMesh {
		c.StartMode, c.MeshStartMode = StartOff, StartOn
	}
	switch c.MeshStartMode {
	case "", StartOn, StartOff, StartLast:
	default:
		c.MeshStartMode = ""
	}
	// На системах, где трогать общесистемные настройки нечем (macOS и прочее),
	// оставлять эти галочки включёнными бессмысленно.
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		c.SysProxy = false
		c.SetEnvVars = false
	}
}

func (p *Profile) normalize(n int) {
	if p.ID == "" {
		p.ID = newProfileID()
	}
	if p.Name == "" {
		p.Name = "Сервер " + itoa(n)
	}
	if p.SSHPort <= 0 || p.SSHPort > 65535 {
		p.SSHPort = 22
	}
	if p.SocksPort <= 0 || p.SocksPort > 65535 {
		p.SocksPort = 1080
	}
	if p.HTTPPort <= 0 || p.HTTPPort > 65535 {
		p.HTTPPort = 1081
	}
	if p.HTTPPort == p.SocksPort { // иначе второй слушатель не поднимется
		p.HTTPPort = p.SocksPort + 1
	}
	if p.PoolSize < 1 {
		p.PoolSize = 1
	}
	if p.PoolSize > 16 {
		p.PoolSize = 16
	}
	if p.User == "" {
		p.User = "root"
	}
	if p.KeyPath == "" {
		p.KeyPath = DetectKeyPath()
	}
	p.MeshKey = strings.TrimSpace(p.MeshKey)
	if p.MeshEnabled && p.MeshKey == "" {
		// Включили сеть устройств, а ключа нет — это первое устройство
		// новой сети. Остальные получат ключ экспортом сервера.
		var raw [24]byte
		rand.Read(raw[:])
		p.MeshKey = base64.RawURLEncoding.EncodeToString(raw[:])
	}
	switch p.FilterMode {
	case "only", "except":
	default:
		p.FilterMode = "all"
	}
	p.KeepAliveSec = clampOrZero(p.KeepAliveSec, MinKeepAliveSec, MaxKeepAliveSec)
	p.ConnectTimeoutSec = clampOrZero(p.ConnectTimeoutSec, MinConnectTimeoutSec, MaxConnectTimeoutSec)
	switch p.Cipher {
	case "aes", "chacha":
	default:
		p.Cipher = ""
	}
	switch p.IPVersion {
	case "4", "6":
	default:
		p.IPVersion = ""
	}
}

// cleanExitVia убирает из «Выход через» пустые id, сам профиль и повторы и
// обрезает до MaxExitHops. Id удалённых профилей остаются (см. ExitChain).
// Звено из панели без профиля (ExitNeedPrefix) становится id профиля, как
// только профиль с этим адресом появился.
func (c Config) cleanExitVia(p Profile) []string {
	var out []string
	seen := map[string]bool{p.ID: true}
	for _, id := range p.ExitVia {
		id = strings.TrimSpace(id)
		if key, need := strings.CutPrefix(id, ExitNeedPrefix); need {
			if hop, ok := c.ProfileByKey(key); ok {
				id = hop.ID
			}
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) == MaxExitHops {
			break
		}
	}
	return out
}

// StampExitEdits — настройки целиком от экрана (ТЗ 16): у профиля, где
// человек поменял цепочку выхода, ставится время правки (TouchExit), и
// панели серверов увидят его значение. Экран открыли до того, как пришла
// цепочка из панели (его ExitT старше), — такая правка не перебивает
// свежую цепочку молча: остаётся цепочка панели, note говорит об этом.
func StampExitEdits(cur Config, next *Config, now int64) (note string) {
	for i := range next.Profiles {
		p := &next.Profiles[i]
		c, ok := cur.ProfileByID(p.ID)
		if !ok {
			continue
		}
		switch {
		case strings.Join(p.ExitVia, "\x00") == strings.Join(c.ExitVia, "\x00"):
			p.ExitT, p.ExitBy = c.ExitT, c.ExitBy
		case p.ExitT < c.ExitT:
			p.ExitVia, p.ExitT, p.ExitBy = c.ExitVia, c.ExitT, c.ExitBy
			who := c.ExitBy
			if who == "" {
				who = "другое окно программы"
			}
			note = "цепочку выхода у «" + p.Name + "» только что поменял(а) " + who + " — твоя правка цепочки не сохранена, проверь и сохрани ещё раз"
		default:
			TouchExit(p, now)
		}
	}
	return note
}
