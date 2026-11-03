//go:build !windows && (!linux || android)

package updater

import "errors"

// OwnUnit — службы systemd здесь нет.
func OwnUnit() string { return "" }

// CanInstall — здесь ставить себя не умеем (телефон ставит APK сам).
func CanInstall() bool { return false }

// Install — не поддерживается.
func Install(newFile, exe string, stop func()) error {
	return errors.New("обновление одной кнопкой здесь не поддерживается")
}
