package app

import (
	"context"
	"time"

	"sshtunnel/internal/config"
	"sshtunnel/internal/tunnel"
)

// SelfCheck прогоняет цепочку самопроверки (см. tunnel.RunSelfCheck) для
// текущего активного сервера. Соединение для проверки отдельное от уже
// поднятого пула — работает и объясняет, где именно не так, даже когда
// туннель выключен.
func (a *App) SelfCheck(ctx context.Context) []tunnel.CheckStep {
	a.mu.Lock()
	cfg := a.cfg
	netLayer := a.net
	a.mu.Unlock()
	active := cfg.Active()

	opt := tunnel.SelfCheckOptions{
		Config: tunnel.Config{
			Host:           active.Host,
			SSHPort:        active.SSHPort,
			User:           active.User,
			KeyPath:        active.KeyPath,
			KnownHostsPath: config.KnownHostsPath(),
			Cipher:         active.Cipher,
			IPVersion:      active.IPVersion,
		},
		DialTimeout: time.Duration(active.ConnectTimeoutSec) * time.Second,
	}
	// В режиме VPN проверочное соединение должно идти мимо адаптера, как и
	// сам туннель: иначе при включённом обходе оно попадало в наш же адаптер,
	// тот закрывал его как петлю, и проверка ругалась на исправный сервер.
	if sc, ok := netLayer.(SelfCheckLayer); ok {
		sc.PrepareSelfCheck(&opt.Config)
	}
	return tunnel.RunSelfCheck(ctx, opt)
}

// SelfCheckLayer — слой VPN, который знает, как пустить проверочное
// соединение мимо своего адаптера (привязка сокета к настоящей сети и
// резолвер настоящей сети).
type SelfCheckLayer interface {
	PrepareSelfCheck(cfg *tunnel.Config)
}
