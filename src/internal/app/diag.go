package app

import (
	"context"
	"errors"
	"net"
	"time"
)

// Доступ диагностики (internal/diag) к туннелю: те же пути, что у настоящих
// соединений, — через сервер и напрямую.

// errTunnelOff — туннель выключен: путь «через VPN» не проверить.
var errTunnelOff = errors.New("туннель выключен")

// DialTunnel — соединение через сервер (имя разрешает сервер).
func (a *App) DialTunnel(network, target string) (net.Conn, error) {
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if tun == nil {
		return nil, errTunnelOff
	}
	return tun.Dial(network, target)
}

// DialDirect — соединение мимо сервера, как у правила «напрямую». Туннель
// выключен — обычное соединение (адаптера VPN тогда нет).
func (a *App) DialDirect(ctx context.Context, network, target string) (net.Conn, error) {
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	timeout := 10 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	if tun == nil {
		d := &net.Dialer{Timeout: timeout}
		return d.DialContext(ctx, network, target)
	}
	return tun.DialDirect(network, target, timeout)
}

// DirectResolver — DNS настоящей сети (мимо адаптера VPN).
func (a *App) DirectResolver() *net.Resolver {
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if tun == nil {
		return net.DefaultResolver
	}
	return tun.DirectResolver()
}

// TunnelUp — туннель подключён (есть живая связь с сервером).
func (a *App) TunnelUp() bool {
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	return tun != nil && tun.State() == "connected"
}

// VPNMode — работает слой VPN (адаптер), а не только прокси.
func (a *App) VPNMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.net != nil
}
