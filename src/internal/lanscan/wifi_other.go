//go:build !windows

package lanscan

// Wi-Fi на Linux — через nmcli (NetworkManager). Без него, на macOS и на
// сервере без Wi-Fi — available:false. Телефон сюда не ходит (WifiScan.kt).

import (
	"os/exec"
	"strconv"
	"strings"
)

// nmFields — «поле:поле» nmcli -t; двоеточие внутри значения экранировано «\:».
func nmFields(line string) []string {
	var out []string
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line):
			i++
			b.WriteByte(line[i])
		case line[i] == ':':
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteByte(line[i])
		}
	}
	return append(out, b.String())
}

func wifiScan() WifiReport { return nmScan("auto") }

// nmScan — список nmcli; rescan "no" — без нового скана (для «Сигнала» раз в
// секунду), "auto" — nmcli пересканирует сам, если список устарел.
func nmScan(rescan string) WifiReport {
	out, err := exec.Command("nmcli", "-t", "-f", "IN-USE,SSID,BSSID,CHAN,FREQ,SIGNAL,SECURITY,RATE", "dev", "wifi", "list", "--rescan", rescan).Output()
	if err != nil {
		return WifiReport{Error: "нужен NetworkManager (nmcli) и Wi-Fi адаптер"}
	}
	rep := WifiReport{Available: true, Networks: []WifiNet{}}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := nmFields(line)
		if len(f) < 8 {
			continue
		}
		ch, _ := strconv.Atoi(f[3])
		freq, _ := strconv.Atoi(strings.Fields(f[4] + " 0")[0])
		sig, _ := strconv.Atoi(f[5])
		n := WifiNet{SSID: f[1], BSSID: strings.ToLower(f[2]), Channel: ch, Freq: freq, Level: pctToDBm(sig), Sec: f[6], Current: f[0] == "*"}
		rep.Networks = append(rep.Networks, n)
		if n.Current {
			rate, _ := strconv.Atoi(strings.Fields(f[7] + " 0")[0])
			rep.Link = &WifiLink{Connected: true, SSID: n.SSID, BSSID: n.BSSID, Level: n.Level, Freq: n.Freq, Channel: n.Channel, Rate: rate}
		}
	}
	if rep.Link == nil {
		rep.Link = &WifiLink{}
	}
	return rep
}

func wifiLinkOnly() WifiReport {
	r := nmScan("no")
	r.Networks = nil
	return r
}

func openLocationSettings() {}
