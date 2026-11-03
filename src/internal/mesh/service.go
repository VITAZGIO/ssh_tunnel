package mesh

import (
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
