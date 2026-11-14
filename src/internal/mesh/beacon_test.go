package mesh

// Маяки без настоящего id устройства (ТЗ 20).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"net"
	"net/netip"
	"testing"
	"time"
)

// beaconOf — маяк, который отправило бы устройство c с адресом self.
func beaconOf(t *testing.T, c *Client, self string) (raw []byte, p *p2p) {
	t.Helper()
	c.status.Self.IP = netip.MustParseAddr(self)
	cert, fp, _ := p2pCert()
	p, err := startP2P(context.Background(), c, cert, fp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.close)
	raw = p.beaconMsg()
	if raw == nil {
		t.Fatal("маяк не собран")
	}
	return raw, p
}

// В маяке нет настоящего id устройства — только PeerID.
func TestМаякБезНастоящегоID(t *testing.T) {
	dev := NewDeviceID()
	c := New(Config{Key: NewKey(), DeviceID: dev, Via: "127.0.0.1"}, directDial, nil)
	raw, p := beaconOf(t, c, "198.19.0.9")
	if bytes.Contains(raw, []byte(dev)) {
		t.Fatal("в маяке настоящий id устройства")
	}
	b, err := decodeBeacon(p.bkey, raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != PeerID(dev) || b.ID == dev {
		t.Fatalf("id в маяке %q, ждали PeerID %q", b.ID, PeerID(dev))
	}
	var fields map[string]any
	if err := json.Unmarshal(raw[4:len(raw)-sha256.Size], &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["dev"]; ok {
		t.Fatal("в маяке осталось поле dev")
	}
	for k, v := range fields {
		if s, _ := v.(string); s == dev {
			t.Fatalf("поле %q несёт настоящий id", k)
		}
	}
}

// Пойманный маяк не даёт войти к meshd от имени устройства: всё, что в нём
// есть, — PeerID, а с ним meshd видит другое устройство.
func TestПойманныйМаякНеПароль(t *testing.T) {
	addr := startMeshd(t)
	key := NewKey()
	victimID := NewDeviceID()
	victim := startClient(t, addr, Config{Key: key, DeviceID: victimID, Name: "жертва"})
	vst := victim.Status()

	// Тот, кто в той же локалке и знает ключ сети, ловит маяк жертвы.
	sniff := New(Config{Key: key, DeviceID: victimID, Via: "127.0.0.1"}, directDial, nil)
	raw, p := beaconOf(t, sniff, vst.Self.IP.String())
	b, err := decodeBeacon(p.bkey, raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// …и пробует всё, что в маяке похоже на id, как id устройства.
	for _, guess := range []string{b.ID, b.FP, b.Net} {
		if guess == "" {
			continue
		}
		thief := startClient(t, addr, Config{Key: key, DeviceID: guess, Name: "вор"})
		st := thief.Status()
		if st.Self.IP == vst.Self.IP || st.Self.ID == vst.Self.ID {
			t.Fatalf("по %q из маяка meshd пустил как жертву: %+v", guess, st.Self)
		}
	}
	if victim.Status().State != "online" || victim.Status().Self.IP != vst.Self.IP {
		t.Fatal("жертва потеряла адрес")
	}
}

// Маяк старой версии (с полем dev) принимается новой: в эпоху обновления
// старые и новые устройства находят друг друга в локальной сети.
func TestСтарыйМаякПринимается(t *testing.T) {
	c := New(Config{Key: NewKey(), DeviceID: NewDeviceID(), Via: "127.0.0.1"}, directDial, nil)
	ln, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	c.beaconListen = func() (net.PacketConn, error) { return ln, nil }
	c.cache.setList(cachedPeer{IP: "198.19.0.9"}, []cachedPeer{{IP: "198.19.0.5", Host: "x"}})
	c.cache.worked("198.19.0.5", "fp-x", "")
	_, p := beaconOf(t, c, "198.19.0.9")

	now := time.Now().UnixMilli()
	// Ровно как собирала маяк версия до ТЗ 20.
	body, _ := json.Marshal(map[string]any{"v": 1, "net": p.netID, "dev": "raw-old-device-id",
		"ip": "198.19.0.5", "port": 1, "fp": "fp-x", "t": now})
	old := append(append([]byte(nil), beaconMagic[:]...), body...)
	m := hmac.New(sha256.New, p.bkey)
	m.Write(old)
	old = m.Sum(old)

	b, err := decodeBeacon(p.bkey, old, time.Now())
	if err != nil || b.IP != "198.19.0.5" || b.FP != "fp-x" {
		t.Fatalf("старый маяк не разобран: %+v %v", b, err)
	}
	s, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	defer s.Close()
	s.WriteTo(old, ln.LocalAddr())
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.beaconT["198.19.0.5"] == now }, "старый маяк не принят")

	// А новый маяк старому получателю годится: все поля, которые тот читал
	// (net, ip, port, fp, t), на месте.
	var fields map[string]any
	raw := p.beaconMsg()
	json.Unmarshal(raw[4:len(raw)-sha256.Size], &fields)
	for _, k := range []string{"v", "net", "ip", "port", "fp", "t"} {
		if _, ok := fields[k]; !ok {
			t.Fatalf("в новом маяке нет поля %q, нужного старым версиям", k)
		}
	}
}
