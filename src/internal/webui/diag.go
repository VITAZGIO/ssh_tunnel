package webui

import (
	"context"
	"net"

	"sshtunnel/internal/config"
	"sshtunnel/internal/diag"
	"sshtunnel/internal/events"
)

// diagHost — программа глазами диагностики (internal/diag).
type diagHost struct{ a *App }

func (h diagHost) DialTunnel(network, target string) (net.Conn, error) {
	return h.a.DialTunnel(network, target)
}
func (h diagHost) DialDirect(ctx context.Context, network, target string) (net.Conn, error) {
	return h.a.DialDirect(ctx, network, target)
}
func (h diagHost) DirectResolver() *net.Resolver { return h.a.DirectResolver() }
func (h diagHost) TunnelUp() bool                { return h.a.TunnelUp() }
func (h diagHost) Bypass() bool                  { return h.a.Modes().Bypass }
func (h diagHost) VPNMode() bool                 { return h.a.VPNMode() }
func (h diagHost) ProxyURL() string              { return h.a.ProxyURL() }
func (h diagHost) Config() config.Config         { return h.a.Config() }
func (h diagHost) Bus() *events.Bus              { return h.a.Bus }

// Reconnect — переподключить туннель (выключить и включить круглую кнопку)
// и дождаться результата. Сеть устройств не трогается.
func (h diagHost) Reconnect() error {
	if !h.a.Modes().Bypass {
		return nil
	}
	if err := h.a.SetBypass(false); err != nil {
		return err
	}
	return h.a.SetBypass(true)
}

// newDiag — диагностика и журнал на диске. Зовётся до addons.New: модуль
// молчаливой проверки утечек должен быть в каталоге модулей к его запуску.
func newDiag(a *App) *diag.Service {
	if a == nil || a.Bus == nil {
		return nil
	}
	diag.StartJournal(a.Bus, config.Dir())
	return diag.New(diagHost{a}, config.Dir(), nil)
}

// diagRoutes — адреса API диагностики.
func (s *Server) diagRoutes() []route {
	if s.diag == nil {
		return nil
	}
	var rs []route
	for _, r := range s.diag.Routes() {
		rs = append(rs, route{r.Path, r.Methods, r.Desc, r.H})
	}
	return rs
}
