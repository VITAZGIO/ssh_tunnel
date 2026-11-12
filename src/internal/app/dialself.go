package app

import (
	"context"
	"errors"
	"net"

	"sshtunnel/internal/core"
)

// DialSelf — соединение самой программы (календарь к Nextcloud): через
// работающий туннель, где путь выбирает ядро. Туннель выключен — напрямую,
// а «.mesh» — в сеть устройств, если её клиент жив (он живёт и без туннеля).
func (a *App) DialSelf(ctx context.Context, addr string) (net.Conn, error) {
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if tun != nil {
		return tun.DialSelf(addr)
	}
	switch a.decideNoTunnel(addr) {
	case core.RouteMesh:
		return a.meshHost.Client().DialPeer(addr)
	case core.RouteBlock:
		return nil, errors.New("сеть устройств выключена — включи её на главном экране")
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}
