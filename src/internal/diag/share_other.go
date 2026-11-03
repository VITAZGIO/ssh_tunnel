//go:build !windows

package diag

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ShareFile — «Поделиться»: открыть папку с файлом (файловый менеджер), чтобы
// перетащить его в мессенджер или письмо.
func ShareFile(path string) error {
	return exec.Command("xdg-open", filepath.Dir(path)).Start()
}

func osVersion() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(l, "PRETTY_NAME="), `"`)
		}
	}
	return ""
}
