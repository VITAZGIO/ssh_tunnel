package mobile

// Проверки для общего главного экрана на телефоне (адреса /api/checkip и
// /api/profile/latency): те же, что у окна ПК (internal/app), — чтобы экран
// получал одинаковые ответы на обеих платформах.

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"
)

// CheckIP — внешний адрес, которым туннель выходит в интернет (через сам
// туннель, как на ПК). Ответ — JSON {"ip":…} или {"error":…}.
func (t *Tunnel) CheckIP() string {
	t.mu.Lock()
	tun := t.core
	t.mu.Unlock()
	if tun == nil {
		return `{"error":"туннель не запущен"}`
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
				return tun.Dial(network, addr)
			},
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Get("https://api.ipify.org")
	if err != nil {
		return jsonErr("не удалось проверить адрес: " + err.Error())
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	ip := string(buf[:n])
	if net.ParseIP(ip) == nil {
		return jsonErr("сервис проверки ответил непонятно")
	}
	return `{"ip":` + strconv.Quote(ip) + `}`
}

// Latency — отклик сервера SSH в миллисекундах (лучшее из трёх
// подключений, как на ПК); -1 — не ответил. Само приложение исключено из
// VPN, поэтому замер идёт напрямую, а не через туннель.
func Latency(host string, port int) int64 {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	var best time.Duration
	for i := 0; i < 3; i++ {
		start := time.Now()
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			continue
		}
		took := time.Since(start)
		c.Close()
		if best == 0 || took < best {
			best = took
		}
	}
	if best == 0 {
		return -1
	}
	if ms := best.Milliseconds(); ms > 0 {
		return ms
	}
	return 1
}

func jsonErr(msg string) string {
	return `{"error":` + strconv.Quote(msg) + `}`
}
