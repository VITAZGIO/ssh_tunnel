//go:build windows

package updater

import (
	"os"
	"os/exec"
	"syscall"
)

// OwnUnit — службы systemd здесь нет.
func OwnUnit() string { return "" }

// CanInstall — умеет ли эта сборка поставить новую версию сама. VPN-версия:
// ядро живёт в службе Windows, его файл меняет только установщик службы.
func CanInstall() bool { return !VPN }

// Install ставит newFile на место exe и перезапускает программу: работающий
// файл Windows даёт только переименовать (в «.old», его уберёт CleanOld при
// следующем запуске). Новая копия стартует через пару секунд — когда эта
// уже выйдет (stop) и не помешает ей как «уже запущенная».
func Install(newFile, exe string, stop func()) error {
	os.Remove(exe + ".old")
	if err := os.Rename(exe, exe+".old"); err != nil {
		return err
	}
	if err := os.Rename(newFile, exe); err != nil {
		os.Rename(exe+".old", exe)
		return err
	}
	cmd := exec.Command("cmd", "/c", `ping -n 3 127.0.0.1 >nul & start "" "`+exe+`"`)
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		stop()
		os.Exit(0)
	}()
	return nil
}
