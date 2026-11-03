//go:build windows

package filedialog

import (
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32             = windows.NewLazySystemDLL("shell32.dll")
	ole32               = windows.NewLazySystemDLL("ole32.dll")
	procBrowseForFolder = shell32.NewProc("SHBrowseForFolderW")
	procPathFromIDList  = shell32.NewProc("SHGetPathFromIDListW")
	procCoInitializeEx  = ole32.NewProc("CoInitializeEx")
	procCoUninitialize  = ole32.NewProc("CoUninitialize")
	procCoTaskMemFree   = ole32.NewProc("CoTaskMemFree")
)

type browseInfo struct {
	Owner       uintptr
	Root        uintptr
	DisplayName *uint16
	Title       *uint16
	Flags       uint32
	Fn          uintptr
	LParam      uintptr
	Image       int32
}

const (
	bifReturnOnlyFS = 0x00000001
	bifNewDialog    = 0x00000040 // проводник с кнопкой «Создать папку»
	coinitApartment = 0x2
)

// PickFolder показывает системный диалог выбора папки и возвращает полный путь.
func PickFolder(title string) (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Диалог нового вида живёт на COM-потоке.
	procCoInitializeEx.Call(0, coinitApartment)
	defer procCoUninitialize.Call()

	name := make([]uint16, windows.MAX_PATH)
	t, _ := windows.UTF16PtrFromString(title)
	bi := browseInfo{DisplayName: &name[0], Title: t, Flags: bifReturnOnlyFS | bifNewDialog}
	pidl, _, _ := procBrowseForFolder.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return "", ErrCancelled
	}
	defer procCoTaskMemFree.Call(pidl)
	buf := make([]uint16, windows.MAX_PATH*4)
	if ok, _, _ := procPathFromIDList.Call(pidl, uintptr(unsafe.Pointer(&buf[0]))); ok == 0 {
		return "", errors.New("эту папку нельзя выбрать: она не на диске")
	}
	return windows.UTF16ToString(buf), nil
}
