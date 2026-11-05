package lanscan

// Wi-Fi на Windows — через netsh wlan. Подписи у netsh на языке системы
// («Сигнал»/«Signal»), поэтому строки узнаём по значению, а не по подписи:
// «80%» — уровень, «802.11ax» — стандарт, «5 ГГц» — диапазон, первое голое
// число после уровня — канал. UTF-8 — через chcp 65001, иначе кириллица
// приходит в кодировке консоли (cp866).

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

var macRe = regexp.MustCompile(`(?i)^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

func netsh(args string) (string, error) {
	c := exec.Command("cmd", "/c", "chcp 65001>nul & netsh wlan "+args)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := c.Output()
	return string(out), err
}

// kv — «Подпись : значение» (подпись может содержать пробелы и цифры).
func kv(line string) (string, string, bool) {
	if t := strings.TrimRight(line, " "); strings.HasSuffix(t, " :") {
		return strings.TrimSpace(t[:len(t)-2]), "", true // «SSID 2 :» — скрытая сеть
	}
	i := strings.Index(line, " : ")
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+3:]), true
}

func wifiScan() WifiReport {
	out, err := netsh("show networks mode=bssid")
	if strings.Contains(out, "privacy-location") {
		// Windows 11 (24H2): список сетей — только с доступом к
		// местоположению (по точкам доступа можно понять, где ты).
		return WifiReport{Need: "winlocation", Error: "Windows не показывает сети Wi-Fi без доступа к местоположению"}
	}
	if err != nil || !strings.Contains(out, "BSSID") {
		if strings.Contains(strings.ToLower(out), "wlansvc") || err != nil {
			return WifiReport{Error: "нет Wi-Fi адаптера или служба WLAN выключена"}
		}
		return WifiReport{Available: true, Networks: []WifiNet{}, Link: wifiLinkOnly().Link}
	}
	rep := WifiReport{Available: true, Networks: parseNetshNetworks(out), Link: wifiLinkOnly().Link}
	if rep.Link != nil {
		for i := range rep.Networks {
			if rep.Networks[i].BSSID == rep.Link.BSSID {
				rep.Networks[i].Current = true
			}
		}
	}
	return rep
}

// parseNetshNetworks разбирает «netsh wlan show networks mode=bssid».
func parseNetshNetworks(out string) []WifiNet {
	nets := []WifiNet{}
	var ssid, sec string
	var cur *WifiNet
	band, gotLevel, gotChan := "", false, false
	flush := func() {
		if cur != nil {
			if cur.Freq == 0 {
				cur.Freq = chanFreq(cur.Channel, band)
			}
			nets = append(nets, *cur)
		}
		cur, band, gotLevel, gotChan = nil, "", false, false
	}
	for _, raw := range strings.Split(out, "\n") {
		k, v, ok := kv(strings.TrimRight(raw, "\r"))
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(k, "SSID"):
			flush()
			ssid, sec = v, ""
		case strings.HasPrefix(k, "BSSID") && macRe.MatchString(v):
			flush()
			cur = &WifiNet{SSID: ssid, BSSID: strings.ToLower(v), Sec: sec}
		case cur == nil:
			// Строки сети до первой точки: защита — первое «WPA…/WEP/Open».
			u := strings.ToUpper(v)
			if sec == "" && (strings.Contains(u, "WPA") || strings.Contains(u, "WEP") || u == "OPEN" || strings.HasPrefix(u, "ОТКРЫТ")) {
				sec = v
			}
		default:
			if p, ok := atoiPct(v); ok && !gotLevel {
				cur.Level, gotLevel = pctToDBm(p), true
			} else if strings.HasPrefix(v, "802.11") {
				cur.Radio = v
			} else if b := bandOf(v); b != "" && (strings.Contains(v, "Hz") || strings.Contains(v, "Гц")) {
				band = b
			} else if n, err := strconv.Atoi(v); err == nil && gotLevel && !gotChan {
				cur.Channel, gotChan = n, true
			}
		}
	}
	flush()
	return nets
}

// wifiLinkOnly — «netsh wlan show interfaces»: своя сеть, уровень сейчас.
func wifiLinkOnly() WifiReport {
	out, err := netsh("show interfaces")
	if err != nil {
		return WifiReport{Error: "нет Wi-Fi адаптера или служба WLAN выключена"}
	}
	l := &WifiLink{}
	band := ""
	pct, rssi := -1, 0
	for _, raw := range strings.Split(out, "\n") {
		k, v, ok := kv(strings.TrimRight(raw, "\r"))
		if !ok {
			continue
		}
		lk := strings.ToLower(k)
		switch {
		case strings.Contains(lk, "bssid") && macRe.MatchString(v):
			l.BSSID = strings.ToLower(v)
		case strings.HasPrefix(k, "SSID"):
			l.SSID = v
		case lk == "rssi":
			rssi, _ = strconv.Atoi(v)
		case strings.HasPrefix(v, "802.11"):
			l.Radio = v
		case strings.Contains(lk, "канал") || strings.Contains(lk, "channel"):
			l.Channel, _ = strconv.Atoi(v)
		case strings.Contains(lk, "прием") || strings.Contains(lk, "приём") || strings.Contains(lk, "receive"):
			f, _ := strconv.ParseFloat(strings.ReplaceAll(v, ",", "."), 64)
			l.Rate = int(f)
		default:
			if p, ok := atoiPct(v); ok && pct < 0 {
				pct = p
			} else if b := bandOf(v); b != "" && (strings.Contains(v, "Hz") || strings.Contains(v, "Гц")) {
				band = b
			}
		}
	}
	if l.BSSID == "" {
		return WifiReport{Available: true, Link: &WifiLink{Connected: false}}
	}
	l.Connected = true
	l.Level = rssi
	if l.Level == 0 && pct >= 0 {
		l.Level = pctToDBm(pct)
	}
	l.Freq = chanFreq(l.Channel, band)
	return WifiReport{Available: true, Link: l}
}

// openLocationSettings — «Параметры → Конфиденциальность → Расположение».
func openLocationSettings() {
	c := exec.Command("cmd", "/c", "start", "", "ms-settings:privacy-location")
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	_ = c.Start()
}
