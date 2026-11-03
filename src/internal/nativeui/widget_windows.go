//go:build windows

package nativeui

import (
	"unsafe"

	"github.com/jchv/go-webview2"
)

// Виджет на рабочем столе (ТЗ 7, календарь): окно без рамки «на обоях» —
// всегда под остальными окнами, без значка в панели задач, место
// запоминается. Таскается за страницу: она зовёт sshtunnelDrag() по нажатию
// мыши на своём заголовке.

var (
	pReleaseCapture = user32.NewProc("ReleaseCapture")
	pSendMessage    = user32.NewProc("SendMessageW")
)

const (
	gwlExStyle = ^uintptr(19) // -20

	wsPopup        = 0x80000000
	wsVisible      = 0x10000000
	wsClipChildren = 0x02000000
	wsExToolWindow = 0x00000080
	wsExAppWindow  = 0x00040000

	hwndBottom = 1

	wmWindowPosChanging = 0x0046
	wmExitSizeMove      = 0x0232
	wmNCLButtonDown     = 0x00A1
	htCaption           = 2

	swpNoSize     = 0x0001
	swpNoActivate = 0x0010
)

// windowPos — WINDOWPOS: нужно только поле «после какого окна».
type windowPos struct {
	hwnd, insertAfter uintptr
	x, y, cx, cy      int32
	flags             uint32
}

// makeWidget превращает отдельное окно в виджет.
func makeWidget(hwnd uintptr, view webview2.WebView) {
	pSetWindowLongPtr.Call(hwnd, gwlStyle, wsPopup|wsVisible|wsClipChildren)
	ex, _, _ := pGetWindowLongPtr.Call(hwnd, gwlExStyle)
	ex = (ex | wsExToolWindow) &^ wsExAppWindow
	pSetWindowLongPtr.Call(hwnd, gwlExStyle, ex)
	pSetWindowPos.Call(hwnd, hwndBottom, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate|swpFrameChanged)
	_ = view.Bind("sshtunnelDrag", func() {
		pReleaseCapture.Call()
		pSendMessage.Call(hwnd, wmNCLButtonDown, htCaption, 0)
	})
}

// widgetMsg — держать виджет под всеми окнами (даже после щелчка по нему).
func widgetMsg(msg, lp uintptr) {
	if msg == wmWindowPosChanging && lp != 0 {
		wp := (*windowPos)(unsafe.Pointer(lp))
		wp.insertAfter = hwndBottom
		wp.flags &^= swpNoZOrder
	}
}
