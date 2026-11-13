package mesh

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"sync"
)

// Службы внутри программы: порт устройства, который обслуживает сама
// программа, а не программа на 127.0.0.1 (например, «Файлы устройства»).
// Такая служба знает, КТО к ней пришёл: адрес звонящего ставит meshd по
// его управляющему соединению (или прямое соединение, проверенное по
// отпечатку), подделать его устройство не может. Поэтому служба работает
// и при выключенном «Пускать другие устройства к моим службам» — доступ
// она решает сама.

// Caller — устройство сети, которое открыло соединение.
type Caller struct {
	IP   string // адрес в сети устройств, 198.19.x.y
	Host string // имя без «.mesh»
	// ID — постоянный id устройства (PeerID): адрес и имя могут смениться,
	// он — нет. Пусто — не узнать (старый meshd и устройства нет в списке).
	ID string
}

// PeerID — постоянный id устройства, который видят другие устройства
// (ТЗ 18): хеш от id устройства (Config.DeviceID), а не он сам — тот служит
// устройству паролем у meshd. Так же считает cmd/meshd (peerID).
func PeerID(device string) string {
	if device == "" {
		return ""
	}
	h := sha256.Sum256([]byte("ssh_tunnel peer v1\x00" + device))
	return hex.EncodeToString(h[:12])
}

// ServiceFunc обслуживает одно соединение и закрывает его.
type ServiceFunc func(conn net.Conn, from Caller)

var (
	svcMu    sync.Mutex
	services = map[int]ServiceFunc{}
)

// Serve ставит службу на порт port этого устройства (в сети устройств);
// h == nil — снять. Одна на процесс: переживает переподключения сети.
func Serve(port int, h ServiceFunc) {
	svcMu.Lock()
	defer svcMu.Unlock()
	if h == nil {
		delete(services, port)
		return
	}
	services[port] = h
}

func service(port int) ServiceFunc {
	svcMu.Lock()
	defer svcMu.Unlock()
	return services[port]
}

// localService — соединение «сам к себе» со своей службой: труба в памяти.
func localService(h ServiceFunc, from Caller) net.Conn {
	a, b := net.Pipe()
	go h(b, from)
	return a
}
