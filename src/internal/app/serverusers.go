package app

import (
	"errors"
	"net"

	"sshtunnel/internal/serverusers"
)

// ServerUsers вызывает вход создания пользователей на сервере, к которому
// сейчас подключён туннель (см. serverusers): op — "whoami" или "create".
func (a *App) ServerUsers(op string, body any) (map[string]any, error) {
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if tun == nil {
		return nil, errors.New("сначала подключись к серверу, на котором заводить пользователя")
	}
	return serverusers.Call(tun.DialEntry, op, body)
}

// DialServer — соединение с адресом на сервере через туннель (devnet).
func (a *App) DialServer(network, addr string) (net.Conn, error) {
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if tun == nil {
		return nil, errors.New("туннель не подключён")
	}
	// Службы на самом сервере — через входной сервер, мимо цепочки выхода.
	return tun.DialEntry(network, addr)
}
