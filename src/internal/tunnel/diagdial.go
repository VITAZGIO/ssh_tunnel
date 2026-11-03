package tunnel

import (
	"net"
	"time"
)

// DialDirect — соединение мимо сервера, как у правила «напрямую»: сокет
// привязан к настоящей сети, имя разрешает DNS настоящей сети. Нужен
// диагностике (internal/diag): «открывается ли сайт без нас».
func (t *Tunnel) DialDirect(network, target string, timeout time.Duration) (net.Conn, error) {
	return t.dialDirect(network, target, timeout)
}

// DirectResolver — резолвер настоящей сети (тот, что у прямых соединений).
func (t *Tunnel) DirectResolver() *net.Resolver { return t.resolver() }

// DirectDialer — диалер мимо сервера для своих соединений (DNS настоящей
// сети по UDP и т. п.).
func (t *Tunnel) DirectDialer(timeout time.Duration) *net.Dialer { return t.directDialer(timeout) }
