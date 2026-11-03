// Package diag — диагностика: «Проверить сайт», «Кто трогает трафик»,
// «Проверка утечек», «Отчёт для поддержки». Каждое — дополнительное
// приложение (internal/addons, каталог — addons/catalog_diag.go), логика —
// здесь, в ядре, экран — общие страницы uiassets/addons/*.html, всё — через
// API (/api/diag/…, см. docs/API.md).
//
// Платформу диагностика видит через Host: те же пути, что у настоящих
// соединений (через сервер и напрямую), настройки и журнал.
package diag

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"time"

	"sshtunnel/internal/config"
	"sshtunnel/internal/events"
)

// Host — что диагностике нужно от программы.
type Host interface {
	// DialTunnel — соединение через сервер (имя разрешает сервер). Ошибка,
	// если туннель выключен.
	DialTunnel(network, target string) (net.Conn, error)
	// DialDirect — соединение мимо сервера, как у правила «напрямую».
	DialDirect(ctx context.Context, network, target string) (net.Conn, error)
	// DirectResolver — DNS настоящей сети.
	DirectResolver() *net.Resolver
	// TunnelUp — туннель подключён.
	TunnelUp() bool
	// Bypass — круглая кнопка включена (сайты должны идти через сервер).
	Bypass() bool
	// VPNMode — работает адаптер VPN (иначе только прокси).
	VPNMode() bool
	// ProxyURL — адрес локального HTTP-прокси («http://127.0.0.1:1081»).
	ProxyURL() string
	Config() config.Config
	// Reconnect переподключает туннель, если он должен быть включён, и ждёт
	// результата.
	Reconnect() error
	Bus() *events.Bus
}

// IDs приложений диагностики (совпадают с каталогом addons).
const (
	AppSite     = "sitecheck"
	AppMeddlers = "meddlers"
	AppLeaks    = "leaks"
	AppReport   = "report"
)

// browserClient — клиент, который ходит так же, как браузер: через
// системный прокси, если программа работает прокси, или как есть, если
// работает адаптер VPN.
func browserClient(h Host) *http.Client {
	tr := &http.Transport{DisableKeepAlives: true}
	if !h.VPNMode() && h.TunnelUp() && h.Config().SysProxy {
		if u, err := url.Parse(h.ProxyURL()); err == nil && u.Host != "" {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr, Timeout: 12 * time.Second}
}

// tunnelClient — клиент через сервер.
func tunnelClient(h Host) *http.Client {
	return &http.Client{Timeout: 12 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
			return h.DialTunnel(network, addr)
		},
	}}
}

// directClient — клиент мимо сервера.
func directClient(h Host) *http.Client {
	return &http.Client{Timeout: 12 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext:       h.DialDirect,
	}}
}
