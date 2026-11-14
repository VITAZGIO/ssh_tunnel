package diag

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/diag/probe"
)

// Шаги проверки и их словарь живут в diag/probe — их же зовёт хост плагинов
// на сервере («с сервера», ТЗ 15). Здесь — два пути устройства и итог.

// Step — один шаг проверки: DNS, подключение, защищённое соединение, ответ
// сайта.
type Step = probe.Step

// Path — один путь: напрямую или через VPN.
type Path = probe.Path

// Что сломалось на шаге (коды — в probe).
const (
	kDNSFail    = probe.KindDNSFail
	kDNSSpoof   = probe.KindDNSSpoof
	kTCPReset   = probe.KindTCPReset
	kTCPTimeout = probe.KindTCPTimeout
	kTCPRefused = probe.KindTCPRefused
	kTLSReset   = probe.KindTLSReset
	kTLSTimeout = probe.KindTLSTimeout
	kTLSCert    = probe.KindTLSCert
	kHTTPStub   = probe.KindHTTPStub
	kHTTPError  = probe.KindHTTPError
	kOff        = probe.KindOff
	kOther      = probe.KindOther
)

// SiteResult — проверка сайта целиком.
type SiteResult struct {
	Site    string    `json:"site"`
	Time    time.Time `json:"time"`
	Direct  Path      `json:"direct"`
	VPN     Path      `json:"vpn"`
	Verdict string    `json:"verdict"` // код итога (verdicts.go)
	Text    string    `json:"text"`    // итог одной фразой на языке запроса
	// Suggest — какое правило предложить: "vpn" (пускать через VPN),
	// "direct" (пускать напрямую) или пусто.
	Suggest string `json:"suggest,omitempty"`
	Host    string `json:"host"` // имя для правила
}

// target — что проверять: имя, порт и путь из того, что ввёл человек.
type target = probe.Target

func parseSite(s string) (target, error) { return probe.ParseSite(s) }

// stepTimeout — сколько ждать один шаг.
const stepTimeout = probe.StepTimeout

// CheckSite проверяет сайт двумя путями сразу и подводит итог.
func CheckSite(ctx context.Context, h Host, site, lang string) (SiteResult, error) {
	t, err := parseSite(site)
	if err != nil {
		return SiteResult{}, err
	}
	res := SiteResult{Site: site, Host: t.Host, Time: time.Now()}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); res.Direct = checkDirect(ctx, h, t) }()
	go func() { defer wg.Done(); res.VPN = checkVPN(ctx, h, t) }()
	wg.Wait()
	refineSpoof(&res)
	res.Verdict, res.Suggest = verdict(res.Direct, res.VPN)
	res.Text = phrase(res.Verdict, lang)
	return res, nil
}

func ms(t0 time.Time) int64 { return time.Since(t0).Milliseconds() }

// checkDirect — путь «напрямую»: DNS настоящей сети, соединение мимо
// сервера.
func checkDirect(ctx context.Context, h Host, t target) Path {
	var p Path
	t0 := time.Now()
	ips, err := lookup(ctx, h.DirectResolver(), t.Host)
	st := Step{ID: "dns", Ms: ms(t0)}
	if err != nil {
		st.Kind, st.Note = kDNSFail, shortErr(err)
		return p.FailAt(st)
	}
	p.IPs = ips
	st.OK, st.Note = true, strings.Join(ips, ", ")
	if bogon(ips) {
		st.OK, st.Kind = false, kDNSSpoof
		return p.FailAt(st)
	}
	p.Steps = append(p.Steps, st)
	return p.Connect(ctx, t, func(ctx context.Context) (net.Conn, error) {
		return h.DialDirect(ctx, "tcp", net.JoinHostPort(ips[0], t.Port))
	})
}

// checkVPN — путь «через VPN»: DNS через сервер (DoH), соединение через
// сервер (имя разрешает он).
func checkVPN(ctx context.Context, h Host, t target) Path {
	var p Path
	if !h.TunnelUp() {
		return p.FailAt(Step{ID: "dns", Kind: kOff})
	}
	t0 := time.Now()
	ips, err := dohLookup(ctx, h, t.Host)
	st := Step{ID: "dns", Ms: ms(t0)}
	if err != nil {
		st.Kind, st.Note = kDNSFail, shortErr(err)
		return p.FailAt(st)
	}
	p.IPs = ips
	st.OK, st.Note = true, strings.Join(ips, ", ")
	p.Steps = append(p.Steps, st)
	return p.Connect(ctx, t, func(context.Context) (net.Conn, error) {
		return h.DialTunnel("tcp", net.JoinHostPort(t.Host, t.Port))
	})
}

// refineSpoof — чужой сертификат напрямую при нормальном пути через VPN и
// разных адресах в DNS — это подмена DNS (провайдер ведёт на свою заглушку),
// а не подмена сертификата антивирусом.
func refineSpoof(r *SiteResult) {
	if r.Direct.Fail != kTLSCert || !r.VPN.OK || len(r.VPN.IPs) == 0 {
		return
	}
	for _, a := range r.Direct.IPs {
		for _, b := range r.VPN.IPs {
			if a == b {
				return
			}
		}
	}
	r.Direct.Fail = kDNSSpoof
}

func lookup(ctx context.Context, r *net.Resolver, host string) ([]string, error) {
	return probe.Lookup(ctx, r, host)
}

// dohLookup спрашивает имя у DNS Cloudflare по HTTPS через сервер: так
// отвечает не провайдер.
func dohLookup(ctx context.Context, h Host, host string) ([]string, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []string{a.String()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://cloudflare-dns.com/dns-query?type=A&name="+url.QueryEscape(host), nil)
	req.Header.Set("Accept", "application/dns-json")
	resp, err := tunnelClient(h).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ans struct {
		Status int `json:"Status"`
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ans); err != nil {
		return nil, err
	}
	var out []string
	for _, a := range ans.Answer {
		if a.Type == 1 {
			out = append(out, a.Data)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("имя не найдено")
	}
	return out, nil
}

// bogon — адреса, которые не могут быть у сайта в интернете: заглушки
// провайдера (0.0.0.0, 127.x, локальные сети).
func bogon(ips []string) bool { return probe.Bogon(ips) }

func isStub(resp *http.Response, body []byte) bool { return probe.IsStub(resp, body) }

func tcpKind(err error) string { return probe.TCPKind(err) }

func tlsKind(err error) string { return probe.TLSKind(err) }

func shortErr(err error) string { return probe.ShortErr(err) }

// CombineServer — итог по пути устройства «напрямую» и пути с сервера (ТЗ
// 15): тот же словарь, что у «напрямую / через VPN» — путь с сервера и есть
// путь через VPN, только DNS — сервера.
func CombineServer(direct, server Path, lang string) (code, text, suggest string) {
	code, suggest = verdict(direct, server)
	return code, phrase(code, lang), suggest
}
