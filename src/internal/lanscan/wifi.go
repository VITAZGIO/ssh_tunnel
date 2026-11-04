package lanscan

// Wi-Fi вокруг («Сканер сети» → «Сети Wi-Fi» и «Сигнал») на компьютере.
// Телефон отдаёт то же сам (WifiScan.kt, мост SSHTunnelAndroid), сюда не
// ходит. На Windows — netsh, на Linux — nmcli; где их нет — available:false.

import (
	"net/http"
	"strconv"
	"strings"
)

// WifiNet — сеть вокруг (одна точка доступа).
type WifiNet struct {
	SSID    string `json:"ssid"`
	BSSID   string `json:"bssid"`
	Level   int    `json:"level"`             // дБм
	Freq    int    `json:"freq,omitempty"`    // МГц
	Channel int    `json:"channel,omitempty"` // номер канала
	Width   int    `json:"width,omitempty"`   // МГц, если система говорит
	Sec     string `json:"sec,omitempty"`
	Radio   string `json:"radio,omitempty"` // 802.11ax и т. п.
	Current bool   `json:"current,omitempty"`
}

// WifiLink — своя сеть сейчас.
type WifiLink struct {
	Connected bool   `json:"connected"`
	SSID      string `json:"ssid,omitempty"`
	BSSID     string `json:"bssid,omitempty"`
	Level     int    `json:"level,omitempty"`
	Freq      int    `json:"freq,omitempty"`
	Channel   int    `json:"channel,omitempty"`
	Rate      int    `json:"rate,omitempty"` // Мбит/с
	Radio     string `json:"radio,omitempty"`
}

// WifiReport — ответ GET /api/lanscan/wifi.
type WifiReport struct {
	Available bool      `json:"available"`
	Error     string    `json:"error,omitempty"`
	Networks  []WifiNet `json:"networks,omitempty"`
	Link      *WifiLink `json:"link,omitempty"`
}

func handleWifi(w http.ResponseWriter, r *http.Request) {
	var rep WifiReport
	if r.URL.Query().Get("now") == "1" {
		rep = wifiLinkOnly()
	} else {
		rep = wifiScan()
	}
	writeJSON(w, rep)
}

// pctToDBm — «Сигнал 80%» Windows в дБм: 0% ≈ −100, 100% ≈ −50.
func pctToDBm(p int) int {
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p/2 - 100
}

// chanFreq — частота по номеру канала (band — «2.4», «5», «6» или "").
func chanFreq(ch int, band string) int {
	switch {
	case ch <= 0:
		return 0
	case band == "6":
		return 5950 + 5*ch
	case ch == 14:
		return 2484
	case ch <= 14 && band != "5":
		return 2407 + 5*ch
	default:
		return 5000 + 5*ch
	}
}

// bandOf — диапазон из строки вроде «5 ГГц» / «2.4 GHz».
func bandOf(v string) string {
	v = strings.ReplaceAll(v, ",", ".")
	switch {
	case strings.HasPrefix(v, "2.4"):
		return "2.4"
	case strings.HasPrefix(v, "5"):
		return "5"
	case strings.HasPrefix(v, "6"):
		return "6"
	}
	return ""
}

func atoiPct(v string) (int, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasSuffix(v, "%") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(v, "%")))
	return n, err == nil
}
