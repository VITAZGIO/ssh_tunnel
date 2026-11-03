package diag

import (
	"os"
	"path/filepath"
	"strings"
)

// «Помирить» обход DPI (zapret: winws/nfqws/tpws): у него есть списки
// исключений — --ipset-exclude=файл (адреса) и --hostlist-exclude=файл
// (имена). Дописываем туда сервер VPN, и обход перестаёт трогать SSH до
// сервера. Без такого параметра мирить нечем.

// exclusionFile — файл исключений из командной строки и что в нём: «ip» или
// «host».
func exclusionFile(args, dir string) (string, string) {
	for _, a := range splitArgs(args) {
		for _, p := range []struct{ opt, kind string }{
			{"--ipset-exclude=", "ip"}, {"--hostlist-exclude=", "host"},
		} {
			if strings.HasPrefix(a, p.opt) {
				f := strings.Trim(strings.TrimPrefix(a, p.opt), `"`)
				if !filepath.IsAbs(f) && dir != "" {
					f = filepath.Join(dir, f)
				}
				return f, p.kind
			}
		}
	}
	return "", ""
}

func canReconcile(u Undo) bool {
	if u.Kind != "dpi" {
		return false
	}
	f, _ := exclusionFile(u.Args, u.Dir)
	return f != ""
}

// reconcile дописывает адреса сервера (или его имя) в список исключений.
func reconcile(u *Undo, serverHost string, serverIPs []string) error {
	f, kind := exclusionFile(u.Args, u.Dir)
	if f == "" {
		return errNoReconcile
	}
	add := serverIPs
	if kind == "host" {
		add = []string{serverHost}
	}
	old, _ := os.ReadFile(f)
	have := map[string]bool{}
	for _, l := range strings.Split(string(old), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var lines []string
	for _, a := range add {
		if a != "" && !have[a] {
			lines = append(lines, a)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	if err := appendLines(f, lines); err != nil {
		return err
	}
	u.Excluded, u.ExclFile = append(u.Excluded, lines...), f
	return nil
}
