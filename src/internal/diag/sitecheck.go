package diag

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Step — один шаг проверки: DNS, подключение, защищённое соединение, ответ
// сайта.
type Step struct {
	ID   string `json:"id"` // dns, tcp, tls, http
	OK   bool   `json:"ok"`
	Ms   int64  `json:"ms"`
	Kind string `json:"kind,omitempty"` // что сломалось (см. kinds ниже)
	Note string `json:"note,omitempty"` // подробность: адреса, код ответа, ошибка
}

// Что сломалось на шаге.
const (
	kDNSFail    = "dns_fail"    // имя не разрешилось
	kDNSSpoof   = "dns_spoof"   // провайдер подменил ответ DNS
	kTCPReset   = "tcp_reset"   // соединение сброшено
	kTCPTimeout = "tcp_timeout" // нет ответа
	kTCPRefused = "tcp_refused"
	kTLSReset   = "tls_reset"   // обрыв на рукопожатии TLS — признак DPI
	kTLSTimeout = "tls_timeout" // рукопожатие повисло — тоже DPI
	kTLSCert    = "tls_cert"    // чужой сертификат: заглушка или подмена
	kHTTPStub   = "http_stub"   // вместо сайта — страница блокировки
	kHTTPError  = "http_error"  // сайт ответил ошибкой сервера
	kOff        = "off"         // путь не проверен: туннель выключен
	kOther      = "other"
)

// Path — один путь: напрямую или через VPN.
type Path struct {
	OK    bool     `json:"ok"`
	Fail  string   `json:"fail,omitempty"` // kind первого упавшего шага
	Steps []Step   `json:"steps"`
	IPs   []string `json:"ips,omitempty"`
}

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
type target struct{ host, port, path string }

func parseSite(s string) (target, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return target{}, errors.New("пустой адрес")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return target{}, fmt.Errorf("не похоже на адрес сайта: %q", s)
	}
	t := target{host: strings.ToLower(u.Hostname()), port: u.Port(), path: u.EscapedPath()}
	if t.port == "" {
		t.port = "443"
	}
	if t.path == "" {
		t.path = "/"
	}
	return t, nil
}

// stepTimeout — сколько ждать один шаг.
const stepTimeout = 8 * time.Second

// CheckSite проверяет сайт двумя путями сразу и подводит итог.
func CheckSite(ctx context.Context, h Host, site, lang string) (SiteResult, error) {
	t, err := parseSite(site)
	if err != nil {
		return SiteResult{}, err
	}
	res := SiteResult{Site: site, Host: t.host, Time: time.Now()}
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
	ips, err := lookup(ctx, h.DirectResolver(), t.host)
	st := Step{ID: "dns", Ms: ms(t0)}
	if err != nil {
		st.Kind, st.Note = kDNSFail, shortErr(err)
		return p.fail(st)
	}
	p.IPs = ips
	st.OK, st.Note = true, strings.Join(ips, ", ")
	if bogon(ips) {
		st.OK, st.Kind = false, kDNSSpoof
		return p.fail(st)
	}
	p.Steps = append(p.Steps, st)
	return p.connect(ctx, t, func(ctx context.Context) (net.Conn, error) {
		return h.DialDirect(ctx, "tcp", net.JoinHostPort(ips[0], t.port))
	})
}

// checkVPN — путь «через VPN»: DNS через сервер (DoH), соединение через
// сервер (имя разрешает он).
func checkVPN(ctx context.Context, h Host, t target) Path {
	var p Path
	if !h.TunnelUp() {
		return p.fail(Step{ID: "dns", Kind: kOff})
	}
	t0 := time.Now()
	ips, err := dohLookup(ctx, h, t.host)
	st := Step{ID: "dns", Ms: ms(t0)}
	if err != nil {
		st.Kind, st.Note = kDNSFail, shortErr(err)
		return p.fail(st)
	}
	p.IPs = ips
	st.OK, st.Note = true, strings.Join(ips, ", ")
	p.Steps = append(p.Steps, st)
	return p.connect(ctx, t, func(context.Context) (net.Conn, error) {
		return h.DialTunnel("tcp", net.JoinHostPort(t.host, t.port))
	})
}

func (p Path) fail(st Step) Path {
	p.Steps = append(p.Steps, st)
	p.Fail = st.Kind
	return p
}

// connect — TCP, TLS и HTTP поверх dial.
func (p Path) connect(ctx context.Context, t target, dial func(context.Context) (net.Conn, error)) Path {
	t0 := time.Now()
	dctx, cancel := context.WithTimeout(ctx, stepTimeout)
	c, err := dial(dctx)
	cancel()
	st := Step{ID: "tcp", Ms: ms(t0)}
	if err != nil {
		st.Kind, st.Note = tcpKind(err), shortErr(err)
		return p.fail(st)
	}
	defer c.Close()
	st.OK = true
	p.Steps = append(p.Steps, st)

	t0 = time.Now()
	_ = c.SetDeadline(time.Now().Add(stepTimeout))
	tc := tls.Client(c, &tls.Config{ServerName: t.host, NextProtos: []string{"http/1.1"}})
	err = tc.HandshakeContext(ctx)
	st = Step{ID: "tls", Ms: ms(t0)}
	if err != nil {
		st.Kind, st.Note = tlsKind(err), shortErr(err)
		return p.fail(st)
	}
	st.OK = true
	if cs := tc.ConnectionState(); len(cs.PeerCertificates) > 0 {
		st.Note = issuerOf(cs.PeerCertificates[0])
	}
	p.Steps = append(p.Steps, st)

	t0 = time.Now()
	_ = c.SetDeadline(time.Now().Add(stepTimeout))
	req := "GET " + t.path + " HTTP/1.1\r\nHost: " + t.host +
		"\r\nUser-Agent: Mozilla/5.0 (ssh_tunnel check)\r\nAccept: text/html\r\nConnection: close\r\n\r\n"
	st = Step{ID: "http"}
	if _, err := io.WriteString(tc, req); err != nil {
		st.Ms, st.Kind, st.Note = ms(t0), tlsKind(err), shortErr(err)
		return p.fail(st)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	st.Ms = ms(t0)
	if err != nil {
		st.Kind, st.Note = tlsKind(err), shortErr(err)
		return p.fail(st)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	st.Note = resp.Status
	if isStub(resp, body) {
		st.Kind = kHTTPStub
		return p.fail(st)
	}
	if resp.StatusCode >= 500 {
		st.Kind = kHTTPError
		return p.fail(st)
	}
	st.OK = true
	p.Steps = append(p.Steps, st)
	p.OK = true
	return p
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
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	if a, err := netip.ParseAddr(host); err == nil {
		return []string{a.String()}, nil
	}
	addrs, err := r.LookupNetIP(ctx, "ip4", host)
	if err != nil || len(addrs) == 0 {
		addrs, err = r.LookupNetIP(ctx, "ip", host)
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range addrs {
		out = append(out, a.Unmap().String())
	}
	if len(out) == 0 {
		return nil, errors.New("нет адресов")
	}
	return out, nil
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
func bogon(ips []string) bool {
	for _, s := range ips {
		a, err := netip.ParseAddr(s)
		if err != nil {
			continue
		}
		if a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

// stubMarkers — признаки страницы блокировки у провайдеров.
var stubMarkers = []string{
	"blocked", "роскомнадзор", "rkn.gov.ru", "eais.rkn", "доступ ограничен",
	"доступ к ресурсу ограничен", "заблокирован", "zapret-info", "warning.rt.ru",
	"blackhole", "access denied by", "данный ресурс",
}

func isStub(resp *http.Response, body []byte) bool {
	if loc := strings.ToLower(resp.Header.Get("Location")); loc != "" {
		for _, m := range []string{"rkn", "zapret", "block", "warning.rt.ru", "blocked"} {
			if strings.Contains(loc, m) {
				return true
			}
		}
	}
	if resp.StatusCode == 451 {
		return true
	}
	b := strings.ToLower(string(body))
	if len(b) > 6000 {
		// Настоящая страница сайта большая; заглушка — пара килобайт. По
		// большой странице слово «blocked» ничего не значит.
		return false
	}
	for _, m := range stubMarkers {
		if strings.Contains(b, m) {
			return true
		}
	}
	return false
}

func tcpKind(err error) string {
	switch {
	case isTimeout(err):
		return kTCPTimeout
	case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "refused"):
		return kTCPRefused
	case isReset(err):
		return kTCPReset
	}
	return kOther
}

func tlsKind(err error) string {
	var ce *tls.CertificateVerificationError
	var ue x509.UnknownAuthorityError
	var he x509.HostnameError
	switch {
	case errors.As(err, &ce), errors.As(err, &ue), errors.As(err, &he):
		return kTLSCert
	case isTimeout(err):
		return kTLSTimeout
	case isReset(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return kTLSReset
	}
	return kOther
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &ne) && ne.Timeout())
}

func isReset(err error) bool {
	s := err.Error()
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		strings.Contains(s, "reset") || strings.Contains(s, "forcibly closed") ||
		strings.Contains(s, "aborted")
}

func issuerOf(c *x509.Certificate) string {
	if len(c.Issuer.Organization) > 0 {
		return c.Issuer.Organization[0]
	}
	return c.Issuer.CommonName
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
