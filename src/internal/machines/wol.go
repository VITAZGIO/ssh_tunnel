package machines

// Включить и выключить машину — дополнительная настройка машины.
//
// Включить (Wake-on-LAN): «волшебный пакет» (6×FF и 16 раз MAC)
// широковещанием в локальную сеть — отсюда и руками всех своих устройств в
// сети устройств, у которых добавлен этот плагин: каждое шлёт пакет в свою
// локалку, так с работы будится ПК дома. Выключить: просьба самой машине
// через сеть устройств (на ней ssh_tunnel с этим плагином и разрешено
// выключать).

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/devnet"
	"sshtunnel/internal/lanscan"
	"sshtunnel/internal/mesh"
)

// WolPort — порт плагина в сети устройств: «разбуди MAC», «выключись».
const WolPort = 47842

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
	lans, _ := lanscan.FindLANs()
	for _, l := range lans {
		p := l.Prefix.Masked()
		if !p.Addr().Is4() {
			continue
		}
		a := p.Addr().As4()
		host := uint32(1)<<(32-p.Bits()) - 1
		v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3]) | host
		dsts = append(dsts, net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)))
	}
	sent := 0
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

// serveWol — просьбы своих устройств: разбудить MAC в своей локалке или
// выключиться самому.
func serveWol(c net.Conn, _ mesh.Caller) {
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
}

// shutdownSelf — выключить это устройство (если разрешено) через 30 с.
func shutdownSelf() error {
	if !loadSettings().AllowShutdown {
		return errors.New("на этом устройстве выключение по сети не разрешено — галка в плагине «Машины»")
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

// askPeer — просьба устройству сети (op: wake с mac или shutdown).
func askPeer(peer, op, mac string) error {
	c, err := devnet.DialPeer(net.JoinHostPort(peer, strconv.Itoa(WolPort)))
	if err != nil {
		return errors.New("не отвечает в сети устройств — выключено или на нём нет плагина «Машины»")
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(8 * time.Second))
	b, _ := json.Marshal(map[string]string{"op": op, "mac": mac})
	_, _ = c.Write(b)
	r, _ := io.ReadAll(io.LimitReader(c, 300))
	if string(r) != "ok" {
		if len(r) == 0 {
			return errors.New("не ответило")
		}
		return errors.New(string(r))
	}
	return nil
}

// wakeAll — отсюда и всеми своими устройствами в сети; кто отправил.
func wakeAll(mac string) (sent, failed []string) {
	_, hw, err := normMAC(mac)
	if err != nil {
		return nil, []string{err.Error()}
	}
	if err := wake(hw); err == nil {
		sent = append(sent, "это устройство")
	}
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
			err := askPeer(ip, "wake", mac)
			mu.Lock()
			if err == nil {
				sent = append(sent, name)
			} else if !strings.HasPrefix(err.Error(), "не отвечает") {
				failed = append(failed, name+": "+err.Error())
			}
			mu.Unlock()
		}(p.IP.String(), name)
	}
	wg.Wait()
	return sent, failed
}
