//go:build linux && !android

package updater

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CanInstall — на Linux программа ставит новую версию сама.
func CanInstall() bool { return true }

// Install ставит newFile на место exe и перезапускает программу. Под
// systemd — временной службой: она ждёт, пока мы ответим, ставит файл и
// перезапускает нашу службу. Запущенная руками — подменяет файл
// (переименование поверх работающей программы Linux разрешает) и
// перезапускает себя тем же процессом.
func Install(newFile, exe string, stop func()) error {
	if os.Getenv("INVOCATION_ID") != "" {
		unit := ownUnit()
		if unit == "" {
			return errors.New("не понял, какой службой я запущен — обнови вручную")
		}
		path, err := exec.LookPath("systemd-run")
		if err != nil {
			return errors.New("нет systemd-run — обнови вручную")
		}
		name := "ssh_tunnel_update_" + strconv.FormatInt(time.Now().UnixNano(), 36)
		script := `sleep 1; install -m 755 "$1" "$2" && rm -f "$1" && systemctl restart "$3"`
		out, err := exec.Command(path, "--quiet", "--collect", "--unit="+name,
			"--", "/bin/sh", "-c", script, "sh", newFile, exe, unit).CombinedOutput()
		if err != nil {
			return fmt.Errorf("не запустилась установка: %v %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := os.Rename(newFile, exe); err != nil {
		return fmt.Errorf("не удалось подменить файл: %w", err)
	}
	go func() {
		time.Sleep(time.Second)
		stop()
		syscall.Exec(exe, os.Args, os.Environ())
	}()
	return nil
}

// OwnUnit — служба systemd, в которой мы работаем («ssh_tunnel.service»).
func OwnUnit() string { return ownUnit() }

func ownUnit() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.LastIndex(line, "/")
		if u := line[i+1:]; i >= 0 && strings.HasSuffix(u, ".service") {
			return u
		}
	}
	return ""
}
