package sshapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// agentPath — путь агента для ssh.exe. OpenSSH для Windows понимает канал
// в IdentityAgent только с прямыми слешами (//./pipe/…): обратные он
// разбирает как экранирование (проверено на 9.5p1, 02.11.2026).
func agentPath(a agentEndpoint) string {
	return strings.ReplaceAll(a.Path(), `\`, "/")
}

// findSSH — ssh.exe из OpenSSH для Windows (встроен в Windows 10/11).
// Чужой ssh из PATH (Git Bash, MSYS) каналы Windows не понимает.
func findSSH() (string, bool) {
	p := filepath.Join(os.Getenv("SystemRoot"), "System32", "OpenSSH", "ssh.exe")
	if _, err := os.Stat(p); err == nil {
		return p, true
	}
	return "", false
}

// launch — Windows Terminal, если есть, иначе PowerShell в своём окне.
func launch(title, sshBin string, args []string) error {
	if wt, err := exec.LookPath("wt.exe"); err == nil {
		// «;» у wt — разделитель команд; в наших аргументах его нет.
		a := append([]string{"new-tab", "--title", title, sshBin}, args...)
		return exec.Command(wt, a...).Start()
	}
	q := make([]string, 0, len(args)+1)
	for _, s := range append([]string{sshBin}, args...) {
		q = append(q, "'"+strings.ReplaceAll(s, "'", "''")+"'")
	}
	ps := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	cmd := exec.Command(ps, "-NoExit", "-NoLogo", "-Command", "& "+strings.Join(q, " "))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010} // CREATE_NEW_CONSOLE
	return cmd.Start()
}
