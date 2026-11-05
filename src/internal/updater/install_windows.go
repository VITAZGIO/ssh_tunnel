//go:build windows

package updater

import (
	"os"
	"os/exec"
	"syscall"
)

// OwnUnit — службы systemd здесь нет.
func OwnUnit() string { return "" }

// VPNInstall — как ставит себя VPN-версия (её задаёт main по режиму: ядро в
// службе — служба меняет свой файл в Program Files и перезапускается через
// SCM; ядро в окне с правами администратора — как обычная версия). nil — не
// умеет (окно службы: ставит служба, запросы и так идут к ней).
var VPNInstall func(newFile, exe string, stop func()) error

// CanInstall — умеет ли эта сборка поставить новую версию сама.
func CanInstall() bool { return !VPN || VPNInstall != nil }

// Install ставит newFile на место exe и перезапускает программу: работающий
// файл Windows даёт только переименовать (в «.old», его уберёт CleanOld при
// следующем запуске). Новая копия стартует через пару секунд — когда эта
// уже выйдет (stop) и не помешает ей как «уже запущенная».
func Install(newFile, exe string, stop func()) error {
	if VPN && VPNInstall != nil {
		return VPNInstall(newFile, exe, stop)
	}
	return InstallPlain(newFile, exe, stop)
}

// Swap — работающий exe в «.old», новый на его место.
func Swap(newFile, exe string) error {
	os.Remove(exe + ".old")
	if err := os.Rename(exe, exe+".old"); err != nil {
		return err
	}
	if err := os.Rename(newFile, exe); err != nil {
		os.Rename(exe+".old", exe)
		return err
	}
	return nil
}

// InstallPlain — подменить файл и запустить новую копию, когда эта выйдет.
func InstallPlain(newFile, exe string, stop func()) error {
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
