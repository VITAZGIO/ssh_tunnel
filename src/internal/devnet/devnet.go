// Package devnet — сеть устройств для плагинов (Карта
// сети, SSH, Файлы устройства). Приложения не знают, где живёт туннель:
// в окне ПК (internal/app), в службе VPN-версии или в ядре Android
// (android/core/mobile). Тот, у кого туннель, ставит сюда Provider.
package devnet

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"sshtunnel/internal/mesh"
)

// Provider — работающий туннель с сетью устройств.
type Provider interface {
	// Mesh — клиент сети устройств; nil — сеть выключена или ещё не
	// подключилась.
	Mesh() *mesh.Client
	// Dial — соединение от имени программы: куда вести, решает ядро
	// (tunnel.DialApp).
	Dial(network, addr string) (net.Conn, error)
}

var (
	mu  sync.Mutex
	cur Provider
)

// Set ставит (nil — снимает) источник сети устройств.
func Set(p Provider) {
	mu.Lock()
	cur = p
	mu.Unlock()
}

func get() Provider {
	mu.Lock()
	defer mu.Unlock()
	return cur
}

// ErrOff — сеть устройств не работает.
var ErrOff = errors.New("сеть устройств не подключена")

// Client — клиент сети устройств, nil — нет.
func Client() *mesh.Client {
	if p := get(); p != nil {
		return p.Mesh()
	}
	return nil
}

// Status — кто в сети. State "off" — сети нет.
func Status() mesh.Status {
	if c := Client(); c != nil {
		return c.Status()
	}
	return mesh.Status{State: "off", Peers: []mesh.Peer{}}
}

// Ping — задержка до устройства (напрямую или через сервер).
func Ping(ip netip.Addr) (time.Duration, error) {
	c := Client()
	if c == nil {
		return 0, ErrOff
	}
	return c.Ping(ip)
}

// DialPeer — соединение с портом устройства сети («198.19.x.y:порт» или
// «имя.mesh:порт»).
func DialPeer(target string) (net.Conn, error) {
	c := Client()
	if c == nil {
		return nil, ErrOff
	}
	return c.DialPeer(target)
}

// Dial — соединение куда угодно тем путём, который выбрало ядро. Туннеля
// нет (окно VPN-версии, где ядро в службе, или программа выключена) —
// обычное соединение: в режиме VPN его и так поймает адаптер.
func Dial(network, addr string) (net.Conn, error) {
	if p := get(); p != nil {
		if c, err := p.Dial(network, addr); err == nil {
			return c, nil
		} else if !errors.Is(err, ErrOff) {
			return nil, err
		}
	}
	return net.DialTimeout(network, addr, 15*time.Second)
}

// Routed — есть ли туннель, который сам выбирает путь (иначе Dial —
// обычное соединение).
func Routed() bool { return get() != nil }

// serverDialer — источник, который умеет соединяться с адресом на самом
// сервере (через SSH-канал, как вход панели 127.0.0.1:47832).
type serverDialer interface {
	DialServer(network, addr string) (net.Conn, error)
}

// DialServer — соединение с адресом на сервере, к которому подключён туннель.
func DialServer(network, addr string) (net.Conn, error) {
	if sd, ok := get().(serverDialer); ok {
		return sd.DialServer(network, addr)
	}
	return nil, ErrOff
}
