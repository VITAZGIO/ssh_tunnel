//go:build windows

package diag

import (
	"os/exec"
	"strconv"

	"golang.org/x/sys/windows"
)

// ShareFile — «Поделиться»: файл в буфер обмена (вставляется в мессенджер
// или письмо как вложение) и окно Проводника с выделенным файлом.
func ShareFile(path string) error {
	if err := hideWindow(exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"Set-Clipboard -LiteralPath "+psQuote(path))).Run(); err != nil {
		return err
	}
	return exec.Command("explorer.exe", "/select,", path).Start()
}

func osVersion() string {
	v := windows.RtlGetVersion()
	return "Windows " + strconv.Itoa(int(v.MajorVersion)) + "." + strconv.Itoa(int(v.MinorVersion)) + "." + strconv.Itoa(int(v.BuildNumber))
}
