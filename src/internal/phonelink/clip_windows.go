package phonelink

// Фото с телефона → буфер обмена Windows → Ctrl+V. Кладём сразу два вида:
// CF_DIB (понимают все программы) и «PNG» (браузеры, Telegram — без потери
// прозрачности). Работает и в сеансе удалённого стола: программа сидит в том
// же сеансе пользователя, Ctrl+V попадает в окно, где курсор.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	pOpenClipboard    = user32.NewProc("OpenClipboard")
	pCloseClipboard   = user32.NewProc("CloseClipboard")
	pEmptyClipboard   = user32.NewProc("EmptyClipboard")
	pSetClipboardData = user32.NewProc("SetClipboardData")
	pRegisterFormat   = user32.NewProc("RegisterClipboardFormatW")
	pGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	pGlobalLock       = kernel32.NewProc("GlobalLock")
	pGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	pGlobalFree       = kernel32.NewProc("GlobalFree")
	pMoveMemory       = kernel32.NewProc("RtlMoveMemory")
)

const (
	cfDIB         = 8
	gmemMoveable  = 0x0002
	vkControl     = 0x11
	vkV           = 0x56
	maxClipPixels = 40 << 20
)

func pasteImage(data []byte) error {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return errors.New("не картинка (нужен JPEG или PNG)")
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || w*h > maxClipPixels {
		return errors.New("картинка слишком большая")
	}
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)

	// DIB: BITMAPINFOHEADER + строки снизу вверх, BGRA.
	dib := make([]byte, 40+w*h*4)
	binary.LittleEndian.PutUint32(dib[0:], 40)
	binary.LittleEndian.PutUint32(dib[4:], uint32(int32(w)))
	binary.LittleEndian.PutUint32(dib[8:], uint32(int32(h)))
	binary.LittleEndian.PutUint16(dib[12:], 1)  // planes
	binary.LittleEndian.PutUint16(dib[14:], 32) // bpp
	binary.LittleEndian.PutUint32(dib[20:], uint32(w*h*4))
	px := dib[40:]
	for y := 0; y < h; y++ {
		src := rgba.Pix[y*rgba.Stride : y*rgba.Stride+w*4]
		dst := px[(h-1-y)*w*4:]
		for x := 0; x < w; x++ {
			dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = src[x*4+2], src[x*4+1], src[x*4], src[x*4+3]
		}
	}
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, rgba)

	if err := setClipboard(dib, pngBuf.Bytes()); err != nil {
		return err
	}
	time.Sleep(80 * time.Millisecond)
	return send(key(vkControl, 0, 0), key(vkV, 0, 0), key(vkV, 0, keyUp), key(vkControl, 0, keyUp))
}

func setClipboard(dib, pngData []byte) error {
	// Буфер обмена привязан к потоку, который его открыл.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	opened := false
	for i := 0; i < 10; i++ { // другая программа может держать буфер миг
		if r, _, _ := pOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !opened {
		return errors.New("буфер обмена занят другой программой")
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	if err := putClip(cfDIB, dib); err != nil {
		return err
	}
	if len(pngData) > 0 {
		name, _ := syscall.UTF16PtrFromString("PNG")
		if f, _, _ := pRegisterFormat.Call(uintptr(unsafe.Pointer(name))); f != 0 {
			_ = putClip(f, pngData)
		}
	}
	return nil
}

func putClip(format uintptr, data []byte) error {
	hMem, _, _ := pGlobalAlloc.Call(gmemMoveable, uintptr(len(data)))
	if hMem == 0 {
		return errors.New("не хватило памяти под картинку")
	}
	p, _, _ := pGlobalLock.Call(hMem)
	if p == 0 {
		pGlobalFree.Call(hMem)
		return errors.New("не хватило памяти под картинку")
	}
	pMoveMemory.Call(p, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)))
	pGlobalUnlock.Call(hMem)
	if r, _, _ := pSetClipboardData.Call(format, hMem); r == 0 {
		pGlobalFree.Call(hMem) // при успехе память принадлежит буферу
		return errors.New("Windows не взяла картинку в буфер обмена")
	}
	return nil
}
