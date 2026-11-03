// Package addons — «Плагины»: каталог, что добавлено на
// этом устройстве, звёздочки, выключатели фоновых, запуск их модулей ядра.
//
// Одно на все платформы: окно ПК/Linux берёт его через internal/webui,
// Android — через internal/uihost (android/core/mobile). Список добавленного
// свой на каждом устройстве и хранится у ядра в addons.json, экран только
// рисует его и шлёт просьбы (docs/API.md, раздел «Плагины»).
package addons

import (
	"runtime"

	"sshtunnel/internal/updater"
)

// Manifest — описание приложения, одно на все платформы.
type Manifest struct {
	ID   string            `json:"id"`
	Name map[string]string `json:"name"` // по языкам: ru, en
	// Icon — значок из uiassets/icons без «.svg» (Material Symbols).
	Icon string `json:"icon"`
	// Hint — краткая подсказка при первом открытии, 2–3 строки, по языкам.
	Hint map[string]string `json:"hint"`
	// Book — глава Книги про приложение (файл app-<id>.md рядом с главами).
	Book string `json:"book"`
	// Background — работает в фоне: на плитке выключатель, модуль ядра
	// работает, пока он включён.
	Background bool `json:"background"`
	// Window — на ПК и Linux сразу открывать в отдельном окне.
	Window bool `json:"window"`
	// Permissions — что приложению можно (своим приложениям, custom.go).
	// У приложений каталога пусто: они часть программы.
	Permissions []string `json:"permissions"`
	// Page — страница приложения внутри uiassets.
	Page string `json:"page"`
	// Platforms — где работает (runtime.GOOS: windows, linux, android).
	// Пусто — везде.
	Platforms []string `json:"platforms,omitempty"`
	// Custom — своё приложение из файла (custom.go): на плитке пометка «не
	// из каталога», значок — IconURL (файл из архива), а не Icon.
	Custom  bool   `json:"custom,omitempty"`
	IconURL string `json:"iconUrl,omitempty"`
	// Dev — образец для проверки каркаса: в каталоге только в сборке для
	// разработки (версия «dev»), в релизе не показывается.
	Dev bool `json:"-"`
}

// catalog — все приложения, которые знает программа. Новое приложение —
// строка здесь, страница uiassets/addons/<id>.html и глава Книги
// app-<id>.md; модуль ядра, если нужен, — Register.
var catalog = []Manifest{
	{
		ID:   "sample",
		Name: map[string]string{"ru": "Образец", "en": "Sample"},
		Icon: "science",
		Hint: map[string]string{
			"ru": "Пустое приложение для проверки каркаса: добавить, убрать, звёздочка, выключатель, отдельное окно.",
			"en": "An empty app for testing the framework: add, remove, star, switch, separate window.",
		},
		Book:        "app-sample",
		Background:  true,
		Page:        "addons/sample.html",
		Permissions: []string{},
		Dev:         true,
	},
	{
		ID:   "calendar",
		Name: map[string]string{"ru": "Календарь", "en": "Calendar"},
		Icon: "calendar_month",
		Hint: map[string]string{
			"ru": "Календари Nextcloud без DAVx5: сверка раз в 15 минут через сеть устройств. Нужны адрес Nextcloud, логин и пароль приложения. На телефоне события видны в системном календаре.",
			"en": "Nextcloud calendars without DAVx5: synced every 15 minutes over the device network. Needs the Nextcloud address, login and an app password. On the phone events show up in the system calendar.",
		},
		Book:        "app-calendar",
		Background:  true,
		Page:        "addons/calendar.html",
		Permissions: []string{},
	},
	// ТЗ 4: устройства сети (internal/netmap, internal/sshapp, internal/devfiles).
	{
		ID:   "netmap",
		Name: map[string]string{"ru": "Карта сети", "en": "Network map"},
		Icon: "hub",
		Hint: map[string]string{
			"ru": "Устройства сети: кто в сети, напрямую (сплошная линия) или через сервер (пунктир), задержка. Нажми на устройство — SSH и файлы. Имена меняются в панели главного сервера.",
			"en": "Devices of your network: who is online, direct (solid line) or via the server (dashed), latency. Tap a device for SSH and files. Names are changed in the main server's panel.",
		},
		Book:        "app-netmap",
		Window:      true,
		Page:        "addons/netmap.html",
		Permissions: []string{},
	},
	{
		ID:   "ssh",
		Name: map[string]string{"ru": "SSH", "en": "SSH"},
		Icon: "terminal",
		Hint: map[string]string{
			"ru": "Свои серверы: вход по ключу, который создаёт программа. «Установить ключ» — пароль один раз. На ПК открывается терминал, на телефоне — встроенный.",
			"en": "Your servers: sign-in with a key the app creates. «Install key» asks for the password once. A terminal opens on a PC, a built-in one on a phone.",
		},
		Book:        "app-ssh",
		Page:        "addons/ssh.html",
		Permissions: []string{},
	},
	{
		ID:   "files",
		Name: map[string]string{"ru": "Файлы устройства", "en": "Device files"},
		Icon: "folder_shared",
		Hint: map[string]string{
			"ru": "Файлы других устройств сети: смотреть, скачать, загрузить. Другим видны только папки, которые ты сам отметил на этом устройстве.",
			"en": "Files of your other devices: browse, download, upload. Others see only the folders you marked on this device.",
		},
		Book:        "app-files",
		Window:      true,
		Page:        "addons/files.html",
		Permissions: []string{},
	},
	// ТЗ 6: зеркальные папки (internal/mirror).
	{
		ID:   "mirror",
		Name: map[string]string{"ru": "Зеркальные папки", "en": "Mirrored folders"},
		Icon: "sync",
		Hint: map[string]string{
			"ru": "Одна папка на всех устройствах: сервер (всегда включённое устройство) раздаёт, остальные подключаются. Работает по сети устройств и без туннеля. Конфликты и удаления не теряют файлы: версии и корзина на сервере 30 дней.",
			"en": "One folder on all devices: a server (an always-on device) shares it, the others connect. Works over the device network even without the tunnel. Conflicts and deletions never lose files: versions and trash are kept on the server for 30 days.",
		},
		Book:        "app-mirror",
		Background:  true,
		Page:        "addons/mirror.html",
		Permissions: []string{},
	},
}

// showDev — показывать ли образцы. В релизе версия проставлена сборкой.
var showDev = func() bool { return updater.Version == "dev" }

// goos — платформа; на Android runtime.GOOS = "android".
var goos = runtime.GOOS

// Catalog — приложения, которые видны в каталоге этой сборки.
func Catalog() []Manifest {
	out := make([]Manifest, 0, len(catalog))
	for _, m := range catalog {
		if m.Dev && !showDev() {
			continue
		}
		if m.Permissions == nil {
			m.Permissions = []string{}
		}
		out = append(out, m)
	}
	return append(out, customCatalog()...)
}

// Find — описание приложения из каталога этой сборки.
func Find(id string) (Manifest, bool) {
	for _, m := range Catalog() {
		if m.ID == id {
			return m, true
		}
	}
	return Manifest{}, false
}

// Unavailable — почему приложение не работает на этом устройстве ("" —
// работает). Такое показывается серым с причиной и не запускается.
func Unavailable(m Manifest, lang string) string {
	if len(m.Platforms) == 0 {
		return ""
	}
	for _, p := range m.Platforms {
		if p == goos {
			return ""
		}
	}
	if lang == "en" {
		return "Not available on this device"
	}
	return "Не работает на этом устройстве"
}

// BookChapters — главы Книги, которые приносят приложения каталога.
func BookChapters() []string {
	var out []string
	for _, m := range Catalog() {
		if m.Book != "" {
			out = append(out, m.Book)
		}
	}
	return out
}
