package lanscan

// Плагин «Включить по сети» (Wake-on-LAN): список своих машин (имя и MAC) и
// кнопка «Включить» — «волшебный пакет» (6×FF и 16 раз MAC) широковещанием в
// локальную сеть, на порты 9 и 7. Машина должна стоять в той же сети, а WoL —
// быть включён в её BIOS и сетевой карте. Список — wol.json в папке данных.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/devnet"
	"sshtunnel/internal/mesh"
)

// WolID — плагин в каталоге.
const WolID = "wol"

func init() {
	addons.Register(WolID, func() addons.Module { return wolModule{} })
	addons.RegisterRoutes(WolID, addons.Route{
		Path: "/api/wol", Methods: "GET, POST",
		Desc: "Включить по сети (Wake-on-LAN): GET — список машин {name, mac}; POST {\"op\":\"add|remove|wake|shutdown|settings\",\"name\",\"mac\",\"via\",\"peer\"} — добавить, убрать, разбудить (via: local|mesh|пусто), выключить (через peer в сети устройств), настройки {allowShutdown}.",
		H:    handleWol,
	})
}

// WolHost — машина, которую можно разбудить.
type WolHost struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
	// Via — откуда слать: "local" — только отсюда (та же локалка), "mesh" —
	// попросить все свои устройства в сети устройств (у кого добавлен этот
	// плагин) — каждое шлёт пакет в свою локалку; "" — и так, и так.
	Via string `json:"via,omitempty"`
	// Peer — эта же машина в сети устройств (IP 198.19.x.y), если на ней
	// стоит ssh_tunnel с этим плагином: тогда видно, включена ли она, и её
	// можно выключить (если на ней разрешено).
	Peer string `json:"peer,omitempty"`
}

// wolSettings — настройки этого устройства.
type wolSettings struct {
	AllowShutdown bool `json:"allowShutdown"` // выключать это устройство по просьбе своих
}

func wolSetPath() string { return filepath.Join(addons.DataDir(), "wol-settings.json") }

func wolSettingsLoad() wolSettings {
	var s wolSettings
	if b, err := os.ReadFile(wolSetPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

// WolPort — порт плагина в сети устройств: «разбуди MAC в своей локалке».
const WolPort = 47842

type wolModule struct{}

func (wolModule) Start() error {
	mesh.Serve(WolPort, func(c net.Conn, _ mesh.Caller) {
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		var req struct {
			Op  string `json:"op"`
			MAC string `json:"mac"`
		}
		if json.NewDecoder(io.LimitReader(c, 512)).Decode(&req) != nil {
			return
		}
		var err error
		if req.Op == "shutdown" {
			err = shutdownSelf()
		} else {
			var hw net.HardwareAddr
			if _, hw, err = normMAC(req.MAC); err == nil {
				err = wake(hw)
			}
		}
		ans := "ok"
		if err != nil {
			ans = err.Error()
		}
		_, _ = c.Write([]byte(ans))
	})
	return nil
}

func (wolModule) Stop() { mesh.Serve(WolPort, nil) }

// shutdownSelf — выключить это устройство (если на нём разрешено) через 30 с:
// успеет закрыться то, что открыто.
func shutdownSelf() error {
	if !wolSettingsLoad().AllowShutdown {
		return errors.New("на этом устройстве выключение по сети не разрешено — галка в плагине «Включить по сети»")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("shutdown", "/s", "/t", "30", "/c", "ssh_tunnel: выключение по просьбе с другого устройства")
	case "linux":
		cmd = exec.Command("shutdown", "-h", "+1")
	default:
		return errors.New("это устройство так выключить нельзя")
	}
	return cmd.Start()
}

// askPeer — просьба устройству сети: «выключись» и т. п.
func askPeer(peer, op string) error {
	c, err := devnet.DialPeer(net.JoinHostPort(peer, strconv.Itoa(WolPort)))
	if err != nil {
		return errors.New("машина не отвечает в сети устройств — выключена или на ней нет этого плагина")
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(8 * time.Second))
	b, _ := json.Marshal(map[string]string{"op": op})
	_, _ = c.Write(b)
	r, _ := io.ReadAll(io.LimitReader(c, 300))
	if string(r) != "ok" {
		if len(r) == 0 {
			return errors.New("машина не ответила")
		}
		return errors.New(string(r))
	}
	return nil
}

// wakeViaMesh просит все свои устройства в сети разбудить MAC; ответ — кто
// отправил (имена) и кто не смог.
func wakeViaMesh(mac string) (ok, bad []string) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range devnet.Status().Peers {
		if p.Self || !p.Online || !p.IP.IsValid() {
			continue
		}
		name := p.Name
		if name == "" {
			name = p.Host
		}
		wg.Add(1)
		go func(ip, name string) {
			defer wg.Done()
			c, err := devnet.DialPeer(net.JoinHostPort(ip, strconv.Itoa(WolPort)))
			ans := ""
			if err == nil {
				_ = c.SetDeadline(time.Now().Add(8 * time.Second))
				b, _ := json.Marshal(map[string]string{"mac": mac})
				_, _ = c.Write(b)
				r, _ := io.ReadAll(io.LimitReader(c, 300))
				c.Close()
				ans = string(r)
			}
			mu.Lock()
			if ans == "ok" {
				ok = append(ok, name)
			} else if err == nil && ans != "" {
				bad = append(bad, name+": "+ans)
			}
			mu.Unlock()
		}(p.IP.String(), name)
	}
	wg.Wait()
	sort.Strings(ok)
	return ok, bad
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
		type peer struct {
			IP     string `json:"ip"`
			Name   string `json:"name"`
			Online bool   `json:"online"`
		}
		var ps []peer
		online := map[string]bool{}
		for _, p := range devnet.Status().Peers {
			if p.Self || !p.IP.IsValid() {
				continue
			}
			n := p.Name
			if n == "" {
				n = p.Host
			}
			ps = append(ps, peer{p.IP.String(), n, p.Online})
			online[p.IP.String()] = p.Online
		}
		writeJSON(w, map[string]any{"hosts": hs, "peers": ps, "online": online, "settings": wolSettingsLoad(),
			"canShutdown": runtime.GOOS == "windows" || runtime.GOOS == "linux"})
		return
	}
	var req struct {
		Op            string `json:"op"`
		Name          string `json:"name"`
		MAC           string `json:"mac"`
		Via           string `json:"via"`
		Peer          string `json:"peer"`
		AllowShutdown *bool  `json:"allowShutdown"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "нужен JSON {\"op\"}"})
		return
	}
	if req.Op == "settings" {
		if req.AllowShutdown != nil {
			b, _ := json.MarshalIndent(wolSettings{AllowShutdown: *req.AllowShutdown}, "", "  ")
			if err := os.WriteFile(wolSetPath(), b, 0o600); err != nil {
				writeJSON(w, map[string]string{"error": err.Error()})
				return
			}
		}
		writeJSON(w, map[string]bool{"ok": true})
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
		err = wolSave(append(out, WolHost{Name: name, MAC: mac, Via: req.Via, Peer: strings.TrimSpace(req.Peer)}))
	case "shutdown":
		peer := req.Peer
		for _, h := range hs {
			if h.MAC == mac && peer == "" {
				peer = h.Peer
			}
		}
		if peer == "" {
			err = errors.New("не выбрано, какое это устройство в сети устройств — смотри «Как включить выключение»")
			break
		}
		err = askPeer(peer, "shutdown")
	case "remove":
		out := hs[:0]
		for _, h := range hs {
			if h.MAC != mac && h.MAC != strings.ToLower(strings.TrimSpace(req.MAC)) {
				out = append(out, h)
			}
		}
		err = wolSave(out)
	case "wake":
		via := req.Via
		for _, h := range hs {
			if h.MAC == mac && via == "" {
				via = h.Via
			}
		}
		res := map[string]any{"ok": true}
		var sent []string
		if via != "mesh" {
			if e := wake(hw); e == nil {
				sent = append(sent, "это устройство")
			} else {
				res["localError"] = e.Error()
			}
		}
		if via != "local" {
			ok, bad := wakeViaMesh(mac)
			sent = append(sent, ok...)
			res["failed"] = bad
		}
		if len(sent) == 0 {
			err = errors.New("никто не отправил пакет: нет своих устройств в сети с этим плагином, а отсюда — " + fmt.Sprint(res["localError"]))
			break
		}
		res["sent"] = sent
		res["hosts"] = hs
		writeJSON(w, res)
		return
	default:
		err = errors.New("op: add, remove, wake, shutdown или settings")
	}
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "hosts": wolLoad()})
}
