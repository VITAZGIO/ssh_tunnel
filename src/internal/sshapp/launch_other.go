//go:build !windows

package sshapp

import (
	"os"
	"os/exec"
	"runtime"
)

func agentPath(a agentEndpoint) string { return a.Path() }

func findSSH() (string, bool) {
	if runtime.GOOS == "android" {
		return "", false
	}
	p, err := exec.LookPath("ssh")
	return p, err == nil
}

// launch — первый найденный терминал рабочего стола. Нет рабочего стола
// или терминала — ErrNoTerminal: страница откроет встроенный.
func launch(title, sshBin string, args []string) error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return ErrNoTerminal
	}
	cmdline := append([]string{sshBin}, args...)
	for _, t := range []struct {
		bin  string
		flag []string
	}{
		{"x-terminal-emulator", []string{"-e"}},
		{"gnome-terminal", []string{"--title", title, "--"}},
		{"konsole", []string{"-e"}},
		{"xfce4-terminal", []string{"-x"}},
		{"xterm", []string{"-T", title, "-e"}},
	} {
		if p, err := exec.LookPath(t.bin); err == nil {
			return exec.Command(p, append(t.flag, cmdline...)...).Start()
		}
	}
	return ErrNoTerminal
}
