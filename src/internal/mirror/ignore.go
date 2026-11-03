package mirror

import (
	"path"
	"strings"
)

// DefaultIgnore — что не синхронизируется никогда: постоянно меняющийся
// мусор редакторов и систем, чужие синхронизаторы, наши временные файлы.
// Шаблон без «/» сверяется с именем файла или папки, с «/» — с путём от
// корня папки.
var DefaultIgnore = []string{
	".obsidian/workspace*.json",             // раскладка окон Obsidian: своя на каждом устройстве
	"*.sync-conflict-*",                     // конфликты Syncthing
	".stfolder", ".stversions", ".stignore", // служебное Syncthing
	".DS_Store", "Thumbs.db", "desktop.ini",
	"~$*", ".~lock.*#", "*.swp", "*.swx", "*~", ".#*", "*.tmp", "*.crdownload",
	"*.sshtunnel-part", // докачка «Файлов устройства»
	tmpPrefix + "*",    // наши временные файлы
}

// Ignore — исключения папки: DefaultIgnore + свои шаблоны сервера.
type Ignore struct{ pats []string }

// NewIgnore — DefaultIgnore и extra.
func NewIgnore(extra []string) *Ignore {
	ig := &Ignore{pats: append([]string(nil), DefaultIgnore...)}
	for _, p := range extra {
		if p = strings.TrimSpace(strings.ReplaceAll(p, `\`, "/")); p != "" && !strings.HasPrefix(p, "#") {
			ig.pats = append(ig.pats, strings.Trim(p, "/"))
		}
	}
	return ig
}

// Match — исключён ли rel (путь через «/»). Исключённая папка исключает
// всё внутри.
func (ig *Ignore) Match(rel string, dir bool) bool {
	if ig == nil {
		ig = NewIgnore(nil)
	}
	parts := strings.Split(rel, "/")
	for _, p := range ig.pats {
		if strings.Contains(p, "/") {
			// Путь: сам путь или любая его родительская папка.
			for i := len(parts); i > 0; i-- {
				if ok, _ := path.Match(p, strings.Join(parts[:i], "/")); ok {
					return true
				}
			}
			continue
		}
		for _, s := range parts {
			if ok, _ := path.Match(p, s); ok {
				return true
			}
		}
	}
	return false
}
