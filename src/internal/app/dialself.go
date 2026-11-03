package app

import (
	"context"
	"errors"
	"net"

	"sshtunnel/internal/core"
)

// DialSelf — соединение самой программы (календарь к Nextcloud): через
// работающий туннель, где путь выбирает ядро. Туннель выключен — напрямую;
// сеть устройств тогда недоступна.
func (a *App) DialSelf(ctx context.Context, addr string) (net.Conn, error) {
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if tun != nil {
		return tun.DialSelf(addr)
	}
	if core.IsMeshTarget(addr) {
		return nil, errors.New("сеть устройств выключена — включи её на главном экране")
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}
