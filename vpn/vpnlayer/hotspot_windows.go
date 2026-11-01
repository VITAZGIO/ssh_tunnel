package vpnlayer

import (
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// Раздача VPN на мобильную точку доступа Windows.
//
// Точка доступа — это общий доступ к подключению (ICS): Windows подменяет
// адрес телефона адресом «общего» подключения (обычно кабеля) и ждёт
// ответов только через него. Пакеты телефона же по нашим маршрутам уходят
// в адаптер, ответы приходят оттуда — и ICS их выкидывает: телефон пишет
// «подключено без интернета». Помогает то же, что делают руками для
// WireGuard: сделать общим подключением сам адаптер VPN (вкладка «Доступ» в
// его свойствах). Тогда телефон ходит через сервер, как и сам компьютер.
//
// Мобильная точка доступа Windows 10/11 включает ICS по-своему: её адаптер
// (Microsoft Wi-Fi Direct Virtual Adapter) не помечен как «домашняя сеть»,
// и ни одно подключение не значится общим. Поэтому домашней сетью делаем сам
// этот адаптер, если он подключён.
//
// Делается это через COM-объект HNetCfg.HNetShare — тот же, что стоит за
// вкладкой «Доступ». Проще всего достать его из PowerShell: служба VPN и
// так работает с правами системы.

// hotspotOn делает адаптер VPN общим подключением точки доступа. Точка
// доступа не включена — ничего не делает. Возвращает имя подключения,
// которое было общим до нас (его вернёт hotspotOff), и пустую строку, если
// менять было нечего.
func hotspotOn(adapter string) (string, error) {
	out, err := runPS(`
$m = New-Object -ComObject HNetCfg.HNetShare
$pub = $null; $pubName = ''; $priv = $null; $hot = $null; $tun = $null
foreach ($c in $m.EnumEveryConnection) {
  $p = $m.NetConnectionProps.Invoke($c)
  $s = $m.INetSharingConfigurationForINetConnection.Invoke($c)
  if ($p.Name -eq '` + adapter + `') { $tun = $s; continue }
  if ($s.SharingEnabled -and $s.SharingConnectionType -eq 0) { $pub = $s; $pubName = $p.Name }
  if ($s.SharingEnabled -and $s.SharingConnectionType -eq 1) { $priv = $s }
  elseif (-not $hot -and $p.Status -eq 2 -and $p.DeviceName -like '*Wi-Fi Direct Virtual Adapter*') { $hot = $s }
}
if (-not $priv) { $priv = $hot }
if (-not $priv -or -not $tun) { exit 0 }
if ($tun.SharingEnabled -and $tun.SharingConnectionType -eq 0) { exit 0 }
if ($pub) { $pub.DisableSharing() }
$tun.EnableSharing(0)
$priv.EnableSharing(1)
Write-Output ('was=' + $pubName)
`)
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "was="); ok {
			// Общим могло не быть ничего — тогда возвращать некуда, но
			// снимать общий доступ с адаптера всё равно надо.
			if v == "" {
				v = "-"
			}
			return v, nil
		}
	}
	return "", nil
}

// hotspotOff возвращает общий доступ подключению prev (как было до
// hotspotOn) и снимает его с адаптера VPN. Зовётся до того, как адаптер
// исчезнет: общий доступ, оставленный на несуществующем адаптере, лишил бы
// точку доступа интернета до её перезапуска.
func hotspotOff(adapter, prev string) error {
	if prev == "-" {
		prev = ""
	}
	_, err := runPS(`
$m = New-Object -ComObject HNetCfg.HNetShare
$priv = $null; $tun = $null; $back = $null
foreach ($c in $m.EnumEveryConnection) {
  $p = $m.NetConnectionProps.Invoke($c)
  $s = $m.INetSharingConfigurationForINetConnection.Invoke($c)
  if ($p.Name -eq '` + adapter + `') { $tun = $s; continue }
  if ($p.Name -eq '` + strings.ReplaceAll(prev, "'", "''") + `') { $back = $s }
  if ($s.SharingEnabled -and $s.SharingConnectionType -eq 1) { $priv = $s }
}
if ($tun -and $tun.SharingEnabled) { $tun.DisableSharing() }
if ($back) { $back.EnableSharing(0) }
if ($priv -and $back) { $priv.EnableSharing(1) } elseif ($priv) { $priv.DisableSharing() }
`)
	return err
}

// hotspotUp — включена ли сейчас мобильная точка доступа: её адаптер
// подключён. Дёшево, без PowerShell — зовётся раз в несколько секунд.
func hotspotUp() bool {
	ads, err := winipcfg.GetAdaptersAddresses(windows.AF_UNSPEC, winipcfg.GAAFlagDefault)
	if err != nil {
		return false
	}
	for _, a := range ads {
		if a.OperStatus == winipcfg.IfOperStatusUp && strings.Contains(a.Description(), "Wi-Fi Direct Virtual Adapter") {
			return true
		}
	}
	return false
}

func runPS(script string) (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), &psError{err: err, out: strings.TrimSpace(string(out))}
	}
	return string(out), nil
}

type psError struct {
	err error
	out string
}

func (e *psError) Error() string {
	if e.out != "" {
		return e.err.Error() + ": " + e.out
	}
	return e.err.Error()
}
