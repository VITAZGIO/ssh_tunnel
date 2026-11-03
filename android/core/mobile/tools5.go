package mobile

// ТЗ 5: Расход трафика, Сканер сети, Общий буфер — то, что есть только у
// телефона. Сами приложения общие (sshtunnel/internal/traffic, lanscan,
// clipshare); Kotlin даёт сюда то, чего Go на Android не видит: чьё
// соединение, какая сеть, подсеть Wi-Fi, системный буфер, уведомления.

import (
	"encoding/json"
	"net/netip"
	"os"
	"sync"

	"sshtunnel/internal/clipshare"
	"sshtunnel/internal/lanscan"
	"sshtunnel/internal/traffic"
)

// Phone — то, что умеет приложение (реализует Kotlin, Tools5.kt).
type Phone interface {
	// OwnerOf — пакет приложения, открывшего соединение
	// (ConnectivityManager.getConnectionOwnerUid); proto 6 — TCP, 17 — UDP;
	// dstIP "" — не известно. "" — не узнали.
	OwnerOf(proto int, srcIP string, srcPort int, dstIP string, dstPort int) string
	// LocalNet — адрес телефона в Wi-Fi с маской «192.168.1.50/24», "" — нет.
	LocalNet() string
	// OnClip — запись буфера с другого устройства: положить в системный
	// буфер. path — файл с содержимым (в папке данных, отдаётся через
	// FileProvider).
	OnClip(kind, text, path, mime, name string)
	// Alert — предупреждение человеку (лимит трафика). Не Notify: в Java
	// такое имя уже занято у Object.
	Alert(text string)
}

var (
	phoneMu sync.Mutex
	phone   Phone
)

func getPhone() Phone {
	phoneMu.Lock()
	defer phoneMu.Unlock()
	return phone
}

// SetPhone подключает телефон к приложениям ТЗ 5 (при запуске, до StartUI).
func SetPhone(p Phone) {
	phoneMu.Lock()
	phone = p
	phoneMu.Unlock()
	if p == nil {
		lanscan.SetLANSource(nil)
		clipshare.SetSink(nil)
		return
	}
	lanscan.SetLANSource(func() string { return p.LocalNet() })
	clipshare.SetSink(func(it clipshare.Item, path string) {
		text := ""
		if it.Kind == clipshare.KindText {
			text = readText(path)
		}
		p.OnClip(it.Kind, text, path, it.Mime, it.Name)
	})
}

// SetNetworkKind — какая сеть у телефона сейчас: "wifi", "mobile" или "".
func SetNetworkKind(kind string) { traffic.SetNet(kind) }

// ownerOf — для tunnel.Config.OwnerOf.
func ownerOf(proto int, src, dst netip.AddrPort) string {
	p := getPhone()
	if p == nil {
		return ""
	}
	d := ""
	if dst.IsValid() {
		d = dst.Addr().String()
	}
	return p.OwnerOf(proto, src.Addr().String(), int(src.Port()), d, int(dst.Port()))
}

// Warn — devnet.Warn для фоновых приложений: уведомление и строка журнала.
func (d devnetSrc) Warn(text string) {
	if p := getPhone(); p != nil {
		p.Alert(text)
	}
	if b, err := json.Marshal(map[string]string{"kind": "log", "level": "warn", "text": text}); err == nil {
		Emit(string(b))
	}
}

// ClipPush — «Поделиться → ssh_tunnel» и плитка: отправить всем устройствам.
// Ответ — JSON {"sent":N} или {"error","tooBig"}.
func ClipPush(kind, name, mime string, data []byte) string {
	n, err := clipshare.Push(kind, name, mime, data)
	if err != nil {
		b, _ := json.Marshal(map[string]any{"error": err.Error(), "tooBig": err == clipshare.ErrTooBig})
		return string(b)
	}
	b, _ := json.Marshal(map[string]int{"sent": n})
	return string(b)
}

func readText(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
