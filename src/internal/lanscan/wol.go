package lanscan

// Плагин «Включить по сети» (Wake-on-LAN): список своих машин (имя и MAC) и
// кнопка «Включить» — «волшебный пакет» (6×FF и 16 раз MAC) широковещанием в
// локальную сеть, на порты 9 и 7. Машина должна стоять в той же сети, а WoL —
// быть включён в её BIOS и сетевой карте. Список — wol.json в папке данных.

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"sshtunnel/internal/addons"
)

// WolID — плагин в каталоге.
const WolID = "wol"

func init() {
	addons.Register(WolID, func() addons.Module { return module{} })
	addons.RegisterRoutes(WolID, addons.Route{
		Path: "/api/wol", Methods: "GET, POST",
		Desc: "Включить по сети (Wake-on-LAN): GET — список машин {name, mac}; POST {\"op\":\"add|remove|wake\",\"name\",\"mac\"} — добавить, убрать, разбудить (wake можно и с mac не из списка).",
		H:    handleWol,
	})
}

// WolHost — машина, которую можно разбудить.
type WolHost struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
}

var wolMu sync.Mutex

func wolPath() string { return filepath.Join(addons.DataDir(), "wol.json") }

func wolLoad() []WolHost {
	var hs []WolHost
	if b, err := os.ReadFile(wolPath()); err == nil {
		_ = json.Unmarshal(b, &hs)
	}
	if hs == nil {
		hs = []WolHost{}
	}
	return hs
}

func wolSave(hs []WolHost) error {
	sort.Slice(hs, func(i, j int) bool { return strings.ToLower(hs[i].Name) < strings.ToLower(hs[j].Name) })
	b, _ := json.MarshalIndent(hs, "", "  ")
	tmp := wolPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, wolPath())
}

// normMAC — MAC в виде aa:bb:cc:dd:ee:ff из любого привычного написания.
func normMAC(s string) (string, net.HardwareAddr, error) {
	s = strings.TrimSpace(s)
	clean := strings.NewReplacer("-", "", ":", "", ".", "", " ", "").Replace(s)
	if len(clean) == 12 {
		var b strings.Builder
		for i := 0; i < 12; i += 2 {
			if i > 0 {
				b.WriteByte(':')
			}
			b.WriteString(clean[i : i+2])
		}
		s = b.String()
	}
	hw, err := net.ParseMAC(s)
	if err != nil || len(hw) != 6 {
		return "", nil, errors.New("MAC не похож на адрес вида AA:BB:CC:DD:EE:FF")
	}
	return strings.ToLower(hw.String()), hw, nil
}

// wake шлёт волшебный пакет на общий широковещательный адрес и на адрес
// каждой своей сети (некоторые роутеры пропускают только второй).
func wake(hw net.HardwareAddr) error {
	pkt := make([]byte, 0, 102)
	for i := 0; i < 6; i++ {
		pkt = append(pkt, 0xff)
	}
	for i := 0; i < 16; i++ {
		pkt = append(pkt, hw...)
	}
	dsts := []net.IP{net.IPv4bcast}
	lans, _ := FindLANs()
	for _, l := range lans {
		p := l.Prefix.Masked()
		if !p.Addr().Is4() {
			continue
		}
		a := p.Addr().As4()
		bits := p.Bits()
		host := uint32(1)<<(32-bits) - 1
		v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3]) | host
		dsts = append(dsts, net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)))
	}
	var sent int
	var lastErr error
	for _, ip := range dsts {
		for _, port := range []int{9, 7} {
			c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: ip, Port: port})
			if err != nil {
				lastErr = err
				continue
			}
			if _, err := c.Write(pkt); err == nil {
				sent++
			} else {
				lastErr = err
			}
			c.Close()
		}
	}
	if sent == 0 {
		return lastErr
	}
	return nil
}

func handleWol(w http.ResponseWriter, r *http.Request) {
	wolMu.Lock()
	defer wolMu.Unlock()
	hs := wolLoad()
	if r.Method != http.MethodPost {
		writeJSON(w, map[string]any{"hosts": hs})
		return
	}
	var req struct {
		Op   string `json:"op"`
		Name string `json:"name"`
		MAC  string `json:"mac"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "нужен JSON {\"op\"}"})
		return
	}
	mac, hw, err := normMAC(req.MAC)
	if err != nil && req.Op != "remove" {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	switch req.Op {
	case "add":
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = mac
		}
		out := hs[:0]
		for _, h := range hs {
			if h.MAC != mac {
				out = append(out, h)
			}
		}
		err = wolSave(append(out, WolHost{Name: name, MAC: mac}))
	case "remove":
		out := hs[:0]
		for _, h := range hs {
			if h.MAC != mac && h.MAC != strings.ToLower(strings.TrimSpace(req.MAC)) {
				out = append(out, h)
			}
		}
		err = wolSave(out)
	case "wake":
		err = wake(hw)
	default:
		err = errors.New("op: add, remove или wake")
	}
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "hosts": wolLoad()})
}
