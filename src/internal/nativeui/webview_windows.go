//go:build windows

package nativeui

import (
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/jchv/go-webview2"
)

// newView создаёт окно с WebView2, а не вышло — говорит, почему.
//
// Библиотека на любую неудачу возвращает nil, а причину (код Windows) пишет
// в журнал, которого у оконной программы никто не видит. Раньше человек
// всегда получал «нет компонента WebView2», даже когда компонент стоял, а
// окну мешали процессы прежнего окна, оставшиеся после обновления (помогала
// только перезагрузка). Теперь такие процессы закрываются, и окно
// создаётся заново; не помогло — показывается настоящая причина.
func newView(opts webview2.WebViewOptions) (webview2.WebView, error) {
	view, why := tryView(opts)
	if view != nil {
		return view, nil
	}
	if killStaleWebViews(opts.DataPath) > 0 {
		time.Sleep(time.Second)
		if view, why = tryView(opts); view != nil {
			return view, nil
		}
	}
	return nil, viewError(why)
}

// tryView — одна попытка; второе значение — что библиотека написала в журнал.
func tryView(opts webview2.WebViewOptions) (webview2.WebView, string) {
	var buf bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	view := webview2.NewWithOptions(opts)
	log.SetOutput(out)
	log.SetFlags(flags)
	return view, strings.TrimSpace(buf.String())
}

// killStaleWebViews закрывает процессы WebView2, работающие с папкой данных
// этой программы, — их окно уже закрыто, а папку они держат. Возвращает,
// сколько закрыл.
func killStaleWebViews(dataPath string) int {
	if dataPath == "" {
		return 0
	}
	dir := strings.ReplaceAll(filepath.Join(dataPath, "EBWebView"), "'", "''")
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		`$n = 0; Get-CimInstance Win32_Process -Filter "Name='msedgewebview2.exe'" | `+
			`Where-Object { $_.CommandLine -like '*`+dir+`*' } | `+
			`ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue; $n++ }; $n`)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscan(strings.TrimSpace(string(out)), &n)
	return n
}

var hresultRe = regexp.MustCompile(`(?i)\b(?:0x)?(8[0-9a-f]{7})\b`)

// viewError переводит журнал библиотеки в понятную человеку причину.
func viewError(why string) error {
	if !WebView2Installed() {
		return fmt.Errorf("не удалось создать окно: не установлен компонент WebView2. " +
			"Поставь его с сайта Microsoft: https://go.microsoft.com/fwlink/p/?LinkId=2124703")
	}
	code := ""
	if m := hresultRe.FindStringSubmatch(why); m != nil {
		code = strings.ToUpper(m[1])
	}
	reason := "окно WebView2 не запустилось"
	switch code {
	case "80070005":
		reason = "нет доступа к папке данных окна"
	case "8007139F", "800700AA", "80070020":
		reason = "папку данных окна держит другая копия программы"
	case "80070002", "80070003":
		reason = "компонент WebView2 повреждён — переустанови его: https://go.microsoft.com/fwlink/p/?LinkId=2124703"
	}
	if code != "" {
		reason += " (код " + code + ")"
	} else if why != "" {
		reason += " (" + why + ")"
	}
	return fmt.Errorf("не удалось создать окно: %s. Перезагрузи компьютер — обычно помогает", reason)
}
