package clipshare

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// Буфер Windows. Смену видно по GetClipboardSequenceNumber — без окна и без
// чтения содержимого. Служба (SYSTEM, сеанс 0) буфера пользователя не
// видит — там localBoard == nil, наблюдает окно (WatchRemote).

var (
	user32            = windows.NewLazySystemDLL("user32.dll")
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	shell32           = windows.NewLazySystemDLL("shell32.dll")
	pOpenClipboard    = user32.NewProc("OpenClipboard")
	pCloseClipboard   = user32.NewProc("CloseClipboard")
	pEmptyClipboard   = user32.NewProc("EmptyClipboard")
	pGetClipboardData = user32.NewProc("GetClipboardData")
	pSetClipboardData = user32.NewProc("SetClipboardData")
	pIsFormat         = user32.NewProc("IsClipboardFormatAvailable")
	pSeq              = user32.NewProc("GetClipboardSequenceNumber")
	pRegisterFormat   = user32.NewProc("RegisterClipboardFormatW")
	pGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	pGlobalLock       = kernel32.NewProc("GlobalLock")
	pGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	pGlobalSize       = kernel32.NewProc("GlobalSize")
	pGlobalFree       = kernel32.NewProc("GlobalFree")
	pDragQueryFile    = shell32.NewProc("DragQueryFileW")
)

const (
	cfUnicodeText = 13
	cfDIB         = 8
	cfHDrop       = 15
	gmemMoveable  = 0x0002
)

func localBoard() Board {
	if is, err := svc.IsWindowsService(); err != nil || is {
		return nil
	}
	return &winBoard{}
}

type winBoard struct{}

var cfPNG = func() uintptr {
	n, _ := windows.UTF16PtrFromString("PNG")
	r, _, _ := pRegisterFormat.Call(uintptr(unsafe.Pointer(n)))
	return r
}()

func (winBoard) Token() string {
	r, _, _ := pSeq.Call()
	return strconv.FormatUint(uint64(r), 10)
}

// open — буфер бывает занят другой программой: несколько попыток.
func open() bool {
	for i := 0; i < 10; i++ {
		if r, _, _ := pOpenClipboard.Call(0); r != 0 {
			return true
		}
		time.Sleep(30 * time.Millisecond)
	}
	return false
}

func isFormat(f uintptr) bool {
	r, _, _ := pIsFormat.Call(f)
	return r != 0
}

// ptr — адрес памяти, которую отдала система (GlobalLock): она не Go и
// сборщиком не двигается.
func ptr(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }

// global — копия содержимого формата f (буфер открыт).
func global(f uintptr) []byte {
	h, _, _ := pGetClipboardData.Call(f)
	if h == 0 {
		return nil
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		return nil
	}
	defer pGlobalUnlock.Call(h)
	n, _, _ := pGlobalSize.Call(h)
	return append([]byte{}, unsafe.Slice((*byte)(ptr(p)), n)...)
}

func (winBoard) Read() (Clip, bool) {
	if !open() {
		return Clip{}, false
	}
	defer pCloseClipboard.Call()
	switch {
	case isFormat(cfHDrop):
		h, _, _ := pGetClipboardData.Call(cfHDrop)
		if h == 0 {
			return Clip{}, false
		}
		n, _, _ := pDragQueryFile.Call(h, 0xFFFFFFFF, 0, 0)
		if n != 1 {
			return Clip{}, false // несколько файлов — это «Файлы устройства»
		}
		buf := make([]uint16, windows.MAX_LONG_PATH)
		l, _, _ := pDragQueryFile.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		path := windows.UTF16ToString(buf[:l])
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() {
			return Clip{}, false
		}
		c := Clip{Kind: KindFile, Name: filepath.Base(path)}
		if st.Size() > MaxBytes {
			return c, true
		}
		if c.Data, err = os.ReadFile(path); err != nil {
			return Clip{}, false
		}
		return c, true
	case isFormat(cfUnicodeText):
		b := global(cfUnicodeText)
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(b[i*2:])
		}
		s := windows.UTF16ToString(u)
		if s == "" {
			return Clip{}, false
		}
		return Clip{Kind: KindText, Data: []byte(s)}, true
	case cfPNG != 0 && isFormat(cfPNG):
		if b := global(cfPNG); len(b) > 0 {
			return Clip{Kind: KindImage, Mime: "image/png", Data: b}, true
		}
	case isFormat(cfDIB):
		if p, err := dibToPNG(global(cfDIB)); err == nil {
			return Clip{Kind: KindImage, Mime: "image/png", Data: p}, true
		}
	}
	return Clip{}, false
}

// put — положить байты b форматом f (буфер открыт и очищен). Система
// забирает память себе; не взяла — освобождаем.
func put(f uintptr, b []byte) error {
	h, _, _ := pGlobalAlloc.Call(gmemMoveable, uintptr(len(b)))
	if h == 0 {
		return errors.New("нет памяти")
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return errors.New("нет памяти")
	}
	copy(unsafe.Slice((*byte)(ptr(p)), len(b)), b)
	pGlobalUnlock.Call(h)
	if r, _, err := pSetClipboardData.Call(f, h); r == 0 {
		pGlobalFree.Call(h)
		return err
	}
	return nil
}

func utf16z(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, (len(u)+1)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[i*2:], c)
	}
	return b
}

func (winBoard) Write(c Clip) error {
	if !open() {
		return errors.New("буфер занят")
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	switch c.Kind {
	case KindText:
		return put(cfUnicodeText, utf16z(string(c.Data)))
	case KindImage:
		if cfPNG != 0 {
			_ = put(cfPNG, c.Data)
		}
		dib, err := pngToDIB(c.Data)
		if err != nil {
			return err
		}
		return put(cfDIB, dib)
	case KindFile:
		// DROPFILES: смещение списка (20), точка, «не клиентская», «UTF-16»;
		// потом пути, каждый с нулём, и ещё один ноль.
		hdr := make([]byte, 20)
		binary.LittleEndian.PutUint32(hdr[0:], 20)
		binary.LittleEndian.PutUint32(hdr[16:], 1)
		return put(cfHDrop, append(append(hdr, utf16z(c.Path)...), 0, 0))
	}
	return nil
}
