//go:build windows

package nativeui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

// Rect — место и размер окна на экране (в пикселях экрана).
type Rect struct{ X, Y, W, H int }

// WindowOptions — отдельное окно (Книга, плагин).
type WindowOptions struct {
	Key   string // чьё окно: второе с тем же ключом не открывается, показывается первое
	Title string
	URL   string
	// Place — где было окно в прошлый раз; нулевой — по центру, 820×640.
	Place Rect
	// OnClose — где окно было, когда его закрыли (запомнить на следующий раз).
	OnClose  func(Rect)
	DataPath string
	// Widget — виджет на рабочем столе (widget_windows.go): без рамки, под
	// всеми окнами, без значка в панели задач; место запоминается и после
	// перетаскивания.
	Widget bool
}

var (
	extraMu sync.Mutex
	extra   = map[string]uintptr{} // ключ → окно
)

var (
	pGetWindowRect    = user32.NewProc("GetWindowRect")
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")
)

const smCYFullScreen = 17

// tallWindows — окна, которым по умолчанию нужна высота больше обычной, чтобы
// пустая страница помещалась без прокрутки (ключ окна → высота).
var tallWindows = map[string]int{"traffic": 960}

// OpenWindow открывает отдельное окно со своей страницей. Каждое окно живёт
// на своём потоке со своим циклом сообщений: оно не зависит от главного
// (главное прячется в трей, а отдельные остаются) и закрывается своим
// крестиком. Выход из программы закрывает всё.
func OpenWindow(o WindowOptions) error {
	extraMu.Lock()
	if h, ok := extra[o.Key]; ok {
		extraMu.Unlock()
		pShowWindow.Call(h, swShow)
		pShowWindow.Call(h, swRestore)
		pSetForegroundWin.Call(h)
		return nil
	}
	extraMu.Unlock()

	errc := make(chan error, 1)
	go func() {
		// Окно и его сообщения — на одном потоке ОС, как у главного.
		runtime.LockOSThread()
		// Поток не отпускаем: на нём остаются COM и WebView2 закрытого окна, и Go
		// не должен запускать там другой код — горутина выходит, поток завершается.

		w, h := 820, 640
		if th, ok := tallWindows[o.Key]; ok {
			w, h = 840, th
		}
		if o.Widget {
			w, h = 320, 420
		}
		// Не выше экрана (без панели задач).
		if sh, _, _ := pGetSystemMetrics.Call(smCYFullScreen); sh > 0 && h > int(sh) {
			h = int(sh)
		}
		if o.Place.W > 0 && o.Place.H > 0 {
			w, h = o.Place.W, o.Place.H
		}
		// Окно сразу на своём месте: библиотека показывает его по центру экрана
		// ещё до загрузки WebView2, и оно прыгало бы туда, где было (или к
		// главному окну), только через пару секунд.
		unhook := placeOnCreate(o.Place, w, h)
		// Только одна попытка: при неудаче newView закрыл бы процессы WebView2
		// с той же папкой данных — а это процессы главного окна.
		view, why := tryView(webview2.WebViewOptions{
			AutoFocus: true,
			DataPath:  o.DataPath,
			WindowOptions: webview2.WindowOptions{
				Title:  o.Title,
				Width:  uint(w),
				Height: uint(h),
				IconId: iconResourceID,
				Center: o.Place.W == 0,
			},
		})
		unhook()
		if view == nil {
			if why == "" {
				why = "WebView2 не создал окно"
			}
			errc <- errors.New(why)
			return
		}
		hwnd := uintptr(view.Window())
		if o.Place.W > 0 && o.Place.H > 0 && onScreen(o.Place) {
			pSetWindowPos.Call(hwnd, 0, uintptr(o.Place.X), uintptr(o.Place.Y),
				uintptr(o.Place.W), uintptr(o.Place.H), swpNoZOrder)
		}
		if o.Widget {
			makeWidget(hwnd, view)
		} else {
			(&ui{hwnd: hwnd}).themeTitleBar()
			// Новое окно — поверх главного: человек нажал и хочет его видеть.
			pShowWindow.Call(hwnd, swShow)
			pSetForegroundWin.Call(hwnd)
		}

		// Перед закрытием запоминаем, где окно было; дальше закрытие идёт как
		// обычно (библиотека разрушает окно и выходит из цикла этого потока).
		var old uintptr
		proc := windows.NewCallback(func(hw, msg, wp, lp uintptr) uintptr {
			if o.Widget {
				widgetMsg(msg, lp)
			}
			if (msg == wmClose || (o.Widget && msg == wmExitSizeMove)) && o.OnClose != nil {
				var r rect
				pGetWindowRect.Call(hw, uintptr(unsafe.Pointer(&r)))
				o.OnClose(Rect{int(r.Left), int(r.Top), int(r.Right - r.Left), int(r.Bottom - r.Top)})
			}
			ret, _, _ := pCallWindowProc.Call(old, hw, msg, wp, lp)
			return ret
		})
		old, _, _ = pSetWindowLongPtr.Call(hwnd, gwlpWndProc, proc)

		extraMu.Lock()
		extra[o.Key] = hwnd
		extraMu.Unlock()
		errc <- nil

		view.Navigate(o.URL)
		view.Run()

		extraMu.Lock()
		delete(extra, o.Key)
		extraMu.Unlock()
	}()
	return <-errc
}

var (
	pMonitorFromRect    = user32.NewProc("MonitorFromRect")
	pSetWindowsHookEx   = user32.NewProc("SetWindowsHookExW")
	pUnhookWindowsHook  = user32.NewProc("UnhookWindowsHookEx")
	pCallNextHookEx     = user32.NewProc("CallNextHookEx")
	pGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
)

// createStruct — CREATESTRUCTW: где и какого размера создаётся окно.
type createStruct struct {
	params, instance, menu, parent uintptr
	cy, cx, y, x                   int32
	style                          int32
	name, class                    uintptr
	exStyle                        uint32
}

// placeOnCreate — поставить следующее окно верхнего уровня этого потока сразу
// туда, где ему место: запомненное место, если оно видно, иначе по центру
// главного окна. Возвращает, чем снять перехват (звать на том же потоке).
func placeOnCreate(p Rect, w, h int) func() {
	x, y, ok := 0, 0, false
	if p.W > 0 && p.H > 0 && onScreen(p) {
		x, y, w, h, ok = p.X, p.Y, p.W, p.H, true
	} else if u := active; u != nil && u.hwnd != 0 {
		var r rect
		pGetWindowRect.Call(u.hwnd, uintptr(unsafe.Pointer(&r)))
		x = int(r.Left+r.Right)/2 - w/2
		y = int(r.Top+r.Bottom)/2 - h/2
		ok = onScreen(Rect{x, y, w, h})
	}
	if !ok {
		return func() {}
	}
	const whCBT, hcbtCreateWnd = 5, 3
	done := false
	var hook uintptr
	cb := windows.NewCallback(func(code, wp, lp uintptr) uintptr {
		if code == hcbtCreateWnd && !done {
			// CBT_CREATEWND: первым полем — указатель на CREATESTRUCTW.
			cs := *(**createStruct)(unsafe.Pointer(lp))
			if cs.parent == 0 {
				done = true
				cs.x, cs.y, cs.cx, cs.cy = int32(x), int32(y), int32(w), int32(h)
			}
		}
		r, _, _ := pCallNextHookEx.Call(hook, code, wp, lp)
		return r
	})
	tid, _, _ := pGetCurrentThreadID.Call()
	hook, _, _ = pSetWindowsHookEx.Call(whCBT, cb, 0, tid)
	return func() {
		if hook != 0 {
			pUnhookWindowsHook.Call(hook)
			hook = 0
		}
	}
}

// onScreen — видно ли запомненное место хоть на одном мониторе (монитор
// могли отключить — тогда окно открывается по центру).
func onScreen(p Rect) bool {
	r := rect{int32(p.X), int32(p.Y), int32(p.X + p.W), int32(p.Y + p.H)}
	const monitorDefaultToNull = 0
	m, _, _ := pMonitorFromRect.Call(uintptr(unsafe.Pointer(&r)), monitorDefaultToNull)
	return m != 0
}

// CloseWindow закрывает отдельное окно из страницы, например при возврате
// плагина в главное. Сообщение уходит в поток окна — разрушать его здесь нельзя.
func CloseWindow(key string) error {
	extraMu.Lock()
	h := extra[key]
	extraMu.Unlock()
	if h == 0 {
		return nil
	}
	if ok, _, _ := pPostMessage.Call(h, wmClose, 0, 0); ok == 0 {
		return errors.New("не удалось закрыть окно")
	}
	return nil
}

// Opener — как программа открывает отдельные окна (ключ «…-widget» — виджет
// на рабочем столе): base — адрес интерфейса
// («http://127.0.0.1:порт»), к нему прибавляется путь страницы. Место и
// размер каждого окна запоминаются в windows.json в папке настроек.
func Opener(base, dataPath string) func(key, title, url string) error {
	pl := &places{path: filepath.Join(dataPath, "windows.json")}
	if b, err := os.ReadFile(pl.path); err == nil {
		_ = json.Unmarshal(b, &pl.m)
	}
	return func(key, title, url string) error {
		return OpenWindow(WindowOptions{
			Key:     key,
			Title:   title,
			URL:     base + url,
			Place:   pl.get(key),
			OnClose: func(r Rect) { pl.set(key, r) },
			// Своя папка данных — свой процесс браузера: с общей папкой закрытое
			// отдельное окно (его WebView2 библиотека не закрывает) вешало главное.
			DataPath: filepath.Join(dataPath, "windows"),
			Widget:   strings.HasSuffix(key, "-widget"),
		})
	}
}

// places — где были отдельные окна (по ключу окна).
type places struct {
	path string
	mu   sync.Mutex
	m    map[string]Rect
}

func (p *places) get(key string) Rect {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.m[key]
}

// set запоминает место; свёрнутое окно (крошечное или за краем экрана) — нет.
func (p *places) set(key string, r Rect) {
	if r.W < 200 || r.H < 150 || r.X < -10000 || r.Y < -10000 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]Rect{}
	}
	p.m[key] = r
	b, _ := json.MarshalIndent(p.m, "", "  ")
	tmp := p.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p.path)
	}
}
