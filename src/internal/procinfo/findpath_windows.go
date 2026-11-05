//go:build windows

package procinfo

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	pathMu   sync.Mutex
	pathSeen = map[string]string{} // имя в нижнем регистре -> путь
	pathScan time.Time
)

// PathByName — путь к программе по имени файла («chrome.exe»): из запущенных
// сейчас или виденных раньше, иначе из System32. Нужен для значков в «Расходе
// трафика» — учёт хранит только имя.
func PathByName(name string) string {
	key := strings.ToLower(filepath.Base(name))
	if key == "" || key == "." {
		return ""
	}
	if key == "ssh_tunnel" || key == "ssh_tunnel.exe" || key == "ssh_tunnel_vpn.exe" {
		if exe, err := os.Executable(); err == nil {
			return exe
		}
	}
	pathMu.Lock()
	defer pathMu.Unlock()
	if p, ok := pathSeen[key]; ok {
		return p
	}
	if time.Since(pathScan) > 10*time.Second {
		pathScan = time.Now()
		for _, p := range List() {
			if p.Path != "" {
				pathSeen[strings.ToLower(p.Name)] = p.Path
			}
		}
		if p, ok := pathSeen[key]; ok {
			return p
		}
	}
	win := os.Getenv("SystemRoot")
	if win == "" {
		win = `C:\Windows`
	}
	for _, d := range []string{filepath.Join(win, "System32"), win, filepath.Join(win, "SysWOW64")} {
		p := filepath.Join(d, key)
		if _, err := os.Stat(p); err == nil {
			pathSeen[key] = p
			return p
		}
	}
	return ""
}
