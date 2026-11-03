package tunnel

// Имя соединения, пришедшего из адаптера с настоящим адресом (без подставных
// адресов, ТЗ 9). Само чтение имени — core.SniffHost; здесь — откуда взять
// первые байты и сколько их ждать.

import (
	"net"
	"time"

	"sshtunnel/internal/core"
)

// sniffWait — сколько ждать первых байтов от приложения. TLS и HTTP шлют
// запрос сразу же; молчит только протокол, где первым говорит сервер, — для
// известных таких портов не ждём вовсе (serverFirst).
const sniffWait = 300 * time.Millisecond

// serverFirst — порты, где первым говорит сервер (FTP, SSH, почта, базы):
// приложение будет ждать его, а мы — приложение, так что имя там не узнать.
var serverFirst = map[string]bool{
	"21": true, "22": true, "23": true, "25": true, "110": true, "143": true,
	"587": true, "3306": true, "5432": true, "5900": true,
}

// sniffWanted — искать ли имя в начале соединения: пришло с голым адресом,
// подставных адресов нет (с ними имя приносит сам адрес), и от имени хоть
// что-то зависит (в «Обходе», не к устройствам сети).
func (t *Tunnel) sniffWanted(target string) bool {
	if t.fakeIP.Load() || t.bypassOff.Load() || t.draining.Load() || core.IsMeshTarget(target) {
		return false
	}
	_, port, err := net.SplitHostPort(target)
	return err == nil && !serverFirst[port]
}

// sniffConn читает начало соединения, пока в нём не найдётся имя (или не
// станет ясно, что его нет), но не дольше sniffWait. head — прочитанное:
// его надо отдать дальше раньше остального потока.
func sniffConn(conn net.Conn) (host string, head []byte) {
	deadline := time.Now().Add(sniffWait)
	conn.SetReadDeadline(deadline)
	defer conn.SetReadDeadline(time.Time{})
	buf := make([]byte, 4096)
	for len(head) < 16384 {
		n, err := conn.Read(buf)
		head = append(head, buf[:n]...)
		h, more := core.SniffHost(head)
		if h != "" || !more || err != nil {
			return h, head
		}
	}
	return "", head
}

// namedTarget — target с именем вместо адреса (порт тот же).
func namedTarget(target, name string) string {
	if _, port, err := net.SplitHostPort(target); err == nil {
		return net.JoinHostPort(name, port)
	}
	return name
}
