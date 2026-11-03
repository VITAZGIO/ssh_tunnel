package diag

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// «Проверка утечек»: какой адрес видят сайты, чей DNS отвечает, нет ли
// IPv6 мимо туннеля. Ходим так же, как браузер (browserClient), и
// сравниваем с тем, что видно через сервер и напрямую.

// IPInfo — адрес и чей он.
type IPInfo struct {
	IP      string `json:"ip,omitempty"`
	Country string `json:"country,omitempty"`
	ISP     string `json:"isp,omitempty"`
	AS      string `json:"as,omitempty"`
	Via     string `json:"via,omitempty"` // panel — своя точка на панели, ipify — запасная
}

// Fix — что включить, чтобы закрыть утечку (через тот же /api/config).
type Fix struct {
	Field string `json:"field"` // sysProxy | profile.filterMode
	Value any    `json:"value"`
	Text  string `json:"text"`
}

// LeakReport — результат проверки.
type LeakReport struct {
	Time     time.Time `json:"time"`
	Active   bool      `json:"active"` // туннель включён и должен вести сайты через сервер
	Seen     IPInfo    `json:"seen"`   // что видят сайты
	Server   IPInfo    `json:"server"` // адрес сервера (через туннель)
	Direct   IPInfo    `json:"direct"` // свой адрес у провайдера
	IPLeak   bool      `json:"ipLeak"`
	DNS      []IPInfo  `json:"dns,omitempty"`
	DNSLeak  bool      `json:"dnsLeak"`
	IPv6     string    `json:"ipv6,omitempty"`
	IPv6Leak bool      `json:"ipv6Leak"`
	Fixes    []Fix     `json:"fixes,omitempty"`
	WebRTC   string    `json:"webrtc,omitempty"` // страница проверки WebRTC на панели
	Summary  string    `json:"summary"`
	Leak     bool      `json:"leak"`
}

func getJSON(ctx context.Context, c *http.Client, url string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("ответ %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(v)
}

// seenIP — адрес, который видит сервер-«зеркало»: своя точка на панели
// VPS (если сервер выдан панелью), иначе ipify.
func seenIP(ctx context.Context, c *http.Client, panel string) IPInfo {
	var r struct {
		IP string `json:"ip"`
	}
	if panel != "" && getJSON(ctx, c, strings.TrimRight(panel, "/")+"/ip", &r) == nil && net.ParseIP(r.IP) != nil {
		return IPInfo{IP: r.IP, Via: "panel"}
	}
	if getJSON(ctx, c, "https://api.ipify.org?format=json", &r) == nil && net.ParseIP(r.IP) != nil {
		return IPInfo{IP: r.IP, Via: "ipify"}
	}
	return IPInfo{}
}

// geo — страна и провайдер адреса (ip-api.com).
func geo(ctx context.Context, c *http.Client, info *IPInfo) {
	if info.IP == "" {
		return
	}
	var r struct {
		Status, Country, ISP, AS string
	}
	if getJSON(ctx, c, "http://ip-api.com/json/"+info.IP+"?fields=status,country,isp,as", &r) == nil && r.Status == "success" {
		info.Country, info.ISP, info.AS = r.Country, r.ISP, r.AS
	}
}

// resolverIP — какой DNS-сервер на самом деле спросил имя: уникальное имя
// у edns.ip-api.com — его разрешит тот резолвер, через который идёт браузер.
func resolverIP(ctx context.Context, c *http.Client) IPInfo {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	var r struct {
		DNS struct {
			IP  string `json:"ip"`
			Geo string `json:"geo"`
		} `json:"dns"`
	}
	if getJSON(ctx, c, "http://"+hex.EncodeToString(b)+".edns.ip-api.com/json", &r) != nil || r.DNS.IP == "" {
		return IPInfo{}
	}
	return IPInfo{IP: r.DNS.IP, ISP: r.DNS.Geo}
}

func asOf(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0] // «AS12389 Rostelecom» → «AS12389»
	}
	return ""
}

// CheckLeaks — проверка утечек целиком.
func CheckLeaks(ctx context.Context, h Host, lang string) LeakReport {
	rep := LeakReport{Time: time.Now(), Active: h.TunnelUp() && h.Bypass()}
	cfg := h.Config()
	panel := cfg.Active().Panel
	if panel != "" {
		rep.WebRTC = strings.TrimRight(panel, "/") + "/webrtc"
	}
	bc, tc, dc := browserClient(h), tunnelClient(h), directClient(h)

	var wg sync.WaitGroup
	var v6seen, v6tun string
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	run(func() { rep.Seen = seenIP(ctx, bc, panel); geo(ctx, bc, &rep.Seen) })
	run(func() { rep.Direct = seenIP(ctx, dc, ""); geo(ctx, dc, &rep.Direct) })
	if h.TunnelUp() {
		run(func() { rep.Server = seenIP(ctx, tc, panel); geo(ctx, tc, &rep.Server) })
		run(func() {
			var r struct{ IP string }
			if getJSON(ctx, tc, "https://api6.ipify.org?format=json", &r) == nil {
				v6tun = r.IP
			}
		})
	}
	run(func() {
		var r struct{ IP string }
		if getJSON(ctx, bc, "https://api6.ipify.org?format=json", &r) == nil {
			v6seen = r.IP
		}
	})
	run(func() {
		if d := resolverIP(ctx, bc); d.IP != "" {
			geo(ctx, bc, &d)
			rep.DNS = append(rep.DNS, d)
		}
	})
	wg.Wait()
	rep.IPv6 = v6seen

	if rep.Active {
		rep.IPLeak = rep.Seen.IP != "" && rep.Server.IP != "" && rep.Seen.IP != rep.Server.IP
		directAS := asOf(rep.Direct.AS)
		for _, d := range rep.DNS {
			if directAS != "" && asOf(d.AS) == directAS && asOf(rep.Server.AS) != directAS {
				rep.DNSLeak = true
			}
		}
		rep.IPv6Leak = v6seen != "" && v6seen != v6tun
		if rep.IPLeak {
			if !h.VPNMode() && !cfg.SysProxy {
				rep.Fixes = append(rep.Fixes, Fix{Field: "sysProxy", Value: true,
					Text: tr(lang, "Включить системный прокси", "Turn on the system proxy")})
			}
			if fm := cfg.Active().FilterMode; fm != "" && fm != "all" {
				rep.Fixes = append(rep.Fixes, Fix{Field: "profile.filterMode", Value: "all",
					Text: tr(lang, "Пускать через сервер все программы", "Send all programs through the server")})
			}
		}
	}
	rep.Leak = rep.IPLeak || rep.DNSLeak || rep.IPv6Leak
	rep.Summary = leakSummary(rep, lang)
	return rep
}

func leakSummary(r LeakReport, lang string) string {
	switch {
	case !r.Active:
		return tr(lang, "Туннель выключен: сайты видят твой настоящий адрес — это не утечка, VPN просто не работает.",
			"The tunnel is off: sites see your real address — not a leak, the VPN is just not running.")
	case r.Seen.IP == "":
		return tr(lang, "Не получилось узнать адрес — нет связи с проверочными сайтами.",
			"Could not find out the address — no connection to the check sites.")
	case r.IPLeak:
		return tr(lang, "Утечка: сайты видят твой настоящий адрес, а не адрес сервера.",
			"Leak: sites see your real address, not the server's.")
	case r.DNSLeak:
		return tr(lang, "Утечка DNS: имена сайтов спрашиваются у провайдера мимо туннеля.",
			"DNS leak: site names are asked from your ISP, bypassing the tunnel.")
	case r.IPv6Leak:
		return tr(lang, "Утечка IPv6: часть сайтов видит твой настоящий IPv6-адрес.",
			"IPv6 leak: some sites see your real IPv6 address.")
	}
	return tr(lang, "Утечек нет: сайты видят адрес сервера, DNS идёт через туннель.",
		"No leaks: sites see the server's address, DNS goes through the tunnel.")
}

// Watch — молчаливая проверка при каждом подключении: сообщение только при
// утечке. Работает, пока приложение «Проверка утечек» добавлено и
// включено (модуль ядра, см. addons.Register).
type Watch struct {
	h    Host
	st   *Store
	stop chan struct{}
}

func (w *Watch) Start() error {
	w.stop = make(chan struct{})
	ch, unsub := w.h.Bus().Subscribe()
	go func() {
		defer unsub()
		for {
			select {
			case <-w.stop:
				return
			case e, ok := <-ch:
				if !ok {
					return
				}
				if e.Kind != "state" || e.State != "connected" {
					continue
				}
				go w.check()
			}
		}
	}()
	return nil
}

func (w *Watch) Stop() {
	if w.stop != nil {
		close(w.stop)
	}
}

func (w *Watch) check() {
	time.Sleep(4 * time.Second) // дать адаптеру и DNS встать
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	r := CheckLeaks(ctx, w.h, "ru")
	w.st.update(func(d *storeData) { d.Leaks = &r })
	if r.Leak {
		w.h.Bus().Warnf("Проверка утечек: %s Подробнее — «Плагины» → «Проверка утечек».", r.Summary)
	}
}
