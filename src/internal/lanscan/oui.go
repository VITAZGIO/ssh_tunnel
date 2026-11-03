package lanscan

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"net"
	"strings"
	"sync"
)

// База производителей по первым трём байтам MAC — внутри программы, из сети
// ничего не грузится. Собирается tools/mkoui из oui.csv IEEE.
//
//go:embed oui.txt.gz
var ouiGz []byte

var (
	ouiOnce sync.Once
	ouiMap  map[string]string
)

func loadOUI() {
	ouiMap = map[string]string{}
	zr, err := gzip.NewReader(bytes.NewReader(ouiGz))
	if err != nil {
		return
	}
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "\t"); ok {
			ouiMap[k] = v
		}
	}
}

// Vendor — производитель по MAC; "" — не знаем.
func Vendor(mac net.HardwareAddr) string {
	if len(mac) < 3 || RandomMAC(mac) {
		return ""
	}
	ouiOnce.Do(loadOUI)
	return ouiMap[strings.ToUpper(strings.ReplaceAll(mac[:3].String(), ":", ""))]
}

// RandomMAC — «локально администрируемый» MAC: у телефонов с приватным
// адресом Wi-Fi. Производителя у такого нет.
func RandomMAC(mac net.HardwareAddr) bool { return len(mac) > 0 && mac[0]&0x02 != 0 }
