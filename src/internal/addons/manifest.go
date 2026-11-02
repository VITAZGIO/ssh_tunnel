// Package addons — «Дополнительные приложения»: каталог, что добавлено на
// этом устройстве, звёздочки, выключатели фоновых, запуск их модулей ядра.
//
// Одно на все платформы: окно ПК/Linux берёт его через internal/webui,
// Android — через internal/uihost (android/core/mobile). Список добавленного
// свой на каждом устройстве и хранится у ядра в addons.json, экран только
// рисует его и шлёт просьбы (docs/API.md, раздел «Дополнительные приложения»).
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
	// Permissions — что приложению можно (для своих приложений, ТЗ 8). Пока
	// пусто у всех.
	Permissions []string `json:"permissions"`
	// Page — страница приложения внутри uiassets.
	Page string `json:"page"`
	// Platforms — где работает (runtime.GOOS: windows, linux, android).
	// Пусто — везде.
	Platforms []string `json:"platforms,omitempty"`
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
	return out
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
