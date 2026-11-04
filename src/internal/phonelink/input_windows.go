package phonelink

// ПК на Windows: мышь, клавиши и текст с телефона — через SendInput (как
// будто нажали на этом компьютере), уведомления и «Найти» — у часов.

import (
	"errors"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32     = syscall.NewLazyDLL("user32.dll")
	pSendInput = user32.NewProc("SendInput")
	pMsgBeep   = user32.NewProc("MessageBeep")
	inputSize  = unsafe.Sizeof(input{})
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseMove      = 0x0001
	mouseLeftDown  = 0x0002
	mouseLeftUp    = 0x0004
	mouseRightDown = 0x0008
	mouseRightUp   = 0x0010
	mouseMidDown   = 0x0020
	mouseMidUp     = 0x0040
	mouseWheel     = 0x0800

	keyUp      = 0x0002
	keyUnicode = 0x0004
)

// input — INPUT из WinUser.h (64 бит: тип, выравнивание, объединение 32 байта).
type input struct {
	typ  uint32
	_    uint32
	data [32]byte
}

type mouseInput struct {
	dx, dy    int32
	mouseData uint32
	flags     uint32
	time      uint32
	_         uint32
	extra     uintptr
}

type keybdInput struct {
	vk, scan uint16
	flags    uint32
	time     uint32
	_        uint32
	extra    uintptr
}

func send(in ...input) error {
	if len(in) == 0 {
		return nil
	}
	n, _, err := pSendInput.Call(uintptr(len(in)), uintptr(unsafe.Pointer(&in[0])), inputSize)
	if int(n) != len(in) {
		return errors.New("Windows не приняла ввод: " + err.Error())
	}
	return nil
}

func mouse(flags uint32, dx, dy int32, data uint32) input {
	in := input{typ: inputMouse}
	*(*mouseInput)(unsafe.Pointer(&in.data[0])) = mouseInput{dx: dx, dy: dy, mouseData: data, flags: flags}
	return in
}

func key(vk, scan uint16, flags uint32) input {
	in := input{typ: inputKeyboard}
	*(*keybdInput)(unsafe.Pointer(&in.data[0])) = keybdInput{vk: vk, scan: scan, flags: flags}
	return in
}

var mediaVK = map[string]uint16{
	"playpause": 0xB3, "next": 0xB0, "prev": 0xB1, "stop": 0xB2,
	"volup": 0xAF, "voldown": 0xAE, "mute": 0xAD,
}

func inject(e Ev) error {
	switch e.Kind {
	case "media":
		vk, ok := mediaVK[e.Key]
		if !ok {
			return errors.New("нет такой кнопки")
		}
		return send(key(vk, 0, 0), key(vk, 0, keyUp))
	case "mouse":
		var in []input
		if e.DX != 0 || e.DY != 0 {
			in = append(in, mouse(mouseMove, int32(e.DX), int32(e.DY), 0))
		}
		switch e.Btn {
		case "left":
			in = append(in, mouse(mouseLeftDown, 0, 0, 0), mouse(mouseLeftUp, 0, 0, 0))
		case "right":
			in = append(in, mouse(mouseRightDown, 0, 0, 0), mouse(mouseRightUp, 0, 0, 0))
		case "middle":
			in = append(in, mouse(mouseMidDown, 0, 0, 0), mouse(mouseMidUp, 0, 0, 0))
		case "down":
			in = append(in, mouse(mouseLeftDown, 0, 0, 0))
		case "up":
			in = append(in, mouse(mouseLeftUp, 0, 0, 0))
		}
		if e.Scroll != 0 {
			in = append(in, mouse(mouseWheel, 0, 0, uint32(int32(e.Scroll*120))))
		}
		return send(in...)
	case "text":
		var in []input
		for _, r := range e.Text {
			switch r {
			case '\n':
				in = append(in, key(0x0D, 0, 0), key(0x0D, 0, keyUp))
			case '\b':
				in = append(in, key(0x08, 0, 0), key(0x08, 0, keyUp))
			default:
				for _, u := range utf16(r) {
					in = append(in, key(0, u, keyUnicode), key(0, u, keyUnicode|keyUp))
				}
			}
		}
		return send(in...)
	}
	return nil
}

func utf16(r rune) []uint16 {
	if r < 0x10000 {
		return []uint16{uint16(r)}
	}
	r -= 0x10000
	return []uint16{uint16(0xD800 + (r >> 10)), uint16(0xDC00 + (r & 0x3FF))}
}

func showNotif(e Ev) {
	title := e.Title
	if e.App != "" {
		title = e.App + ": " + title
	}
	notify(e.From+" · "+title, e.Text)
}

// ring — «Найти компьютер»: уведомление и звуковой сигнал 10 раз.
func ring(from string) {
	notify("Ищут этот компьютер", "С устройства «"+from+"» нажали «Найти».")
	go func() {
		for i := 0; i < 10; i++ {
			pMsgBeep.Call(0x30) // MB_ICONEXCLAMATION
			time.Sleep(700 * time.Millisecond)
		}
	}()
}
