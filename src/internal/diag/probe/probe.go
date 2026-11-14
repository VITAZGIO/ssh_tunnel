// Package probe — шаги «Проверить сайт» (DNS → TCP → TLS → HTTP) без
// остальной диагностики: их зовут и программа на устройстве
// (internal/diag), и хост плагинов на сервере (internal/node, ТЗ 15).
// Только стандартная библиотека — хост плагинов не тянет за собой окно,
// настройки и Windows.
//
// Словарь один на оба места: виды поломок шага (Kind*) и коды итога
// (verdicts в internal/diag и ServerVerdict здесь) — те же строки в API.
package probe

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// Step — один шаг проверки: DNS, подключение, защищённое соединение, ответ
// сайта.
type Step struct {
	ID   string `json:"id"` // dns, tcp, tls, http
	OK   bool   `json:"ok"`
	Ms   int64  `json:"ms"`
	Kind string `json:"kind,omitempty"` // что сломалось (Kind*)
	Note string `json:"note,omitempty"` // подробность: адреса, код ответа, ошибка
}

// Что сломалось на шаге.
const (
	KindDNSFail    = "dns_fail"    // имя не разрешилось
	KindDNSSpoof   = "dns_spoof"   // провайдер подменил ответ DNS
	KindTCPReset   = "tcp_reset"   // соединение сброшено
	KindTCPTimeout = "tcp_timeout" // нет ответа
	KindTCPRefused = "tcp_refused"
	KindTLSReset   = "tls_reset"   // обрыв на рукопожатии TLS — признак DPI
	KindTLSTimeout = "tls_timeout" // рукопожатие повисло — тоже DPI
	KindTLSCert    = "tls_cert"    // чужой сертификат: заглушка или подмена
	KindHTTPStub   = "http_stub"   // вместо сайта — страница блокировки
	KindHTTPError  = "http_error"  // сайт ответил ошибкой сервера
	KindOff        = "off"         // путь не проверен: туннель выключен
	KindRefused    = "refused"     // проверять нельзя: адрес локальный или закрытый
	KindOther      = "other"
)

// Path — один путь: напрямую, через VPN или с сервера.
type Path struct {
	OK    bool     `json:"ok"`
	Fail  string   `json:"fail,omitempty"` // kind первого упавшего шага
	Steps []Step   `json:"steps"`
	IPs   []string `json:"ips,omitempty"`
}

// FailAt — путь кончился шагом st.
func (p Path) FailAt(st Step) Path {
	p.Steps = append(p.Steps, st)
	p.Fail = st.Kind
	return p
}

// Target — что проверять: имя, порт и путь из того, что ввёл человек.
type Target struct{ Host, Port, Path string }

// ParseSite разбирает «example.com», «https://example.com:8443/x».
func ParseSite(s string) (Target, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Target{}, errors.New("пустой адрес")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return Target{}, fmt.Errorf("не похоже на адрес сайта: %q", s)
	}
	t := Target{Host: strings.ToLower(u.Hostname()), Port: u.Port(), Path: u.EscapedPath()}
	if t.Port == "" {
		t.Port = "443"
	}
	if t.Path == "" {
		t.Path = "/"
	}
	return t, nil
}

// StepTimeout — сколько ждать один шаг.
const StepTimeout = 8 * time.Second

func ms(t0 time.Time) int64 { return time.Since(t0).Milliseconds() }

// Connect — TCP, TLS и HTTP поверх dial (Steps дописываются к p).
func (p Path) Connect(ctx context.Context, t Target, dial func(context.Context) (net.Conn, error)) Path {
	t0 := time.Now()
	dctx, cancel := context.WithTimeout(ctx, StepTimeout)
	c, err := dial(dctx)
	cancel()
	st := Step{ID: "tcp", Ms: ms(t0)}
	if err != nil {
		st.Kind, st.Note = TCPKind(err), ShortErr(err)
		return p.FailAt(st)
	}
	defer c.Close()
	st.OK = true
	p.Steps = append(p.Steps, st)

	t0 = time.Now()
	_ = c.SetDeadline(time.Now().Add(StepTimeout))
	tc := tls.Client(c, &tls.Config{ServerName: t.Host, NextProtos: []string{"http/1.1"}})
	err = tc.HandshakeContext(ctx)
	st = Step{ID: "tls", Ms: ms(t0)}
	if err != nil {
		st.Kind, st.Note = TLSKind(err), ShortErr(err)
		return p.FailAt(st)
	}
	st.OK = true
	if cs := tc.ConnectionState(); len(cs.PeerCertificates) > 0 {
		st.Note = issuerOf(cs.PeerCertificates[0])
	}
	p.Steps = append(p.Steps, st)

	t0 = time.Now()
	_ = c.SetDeadline(time.Now().Add(StepTimeout))
	req := "GET " + t.Path + " HTTP/1.1\r\nHost: " + t.Host +
		"\r\nUser-Agent: Mozilla/5.0 (ssh_tunnel check)\r\nAccept: text/html\r\nConnection: close\r\n\r\n"
	st = Step{ID: "http"}
	if _, err := io.WriteString(tc, req); err != nil {
		st.Ms, st.Kind, st.Note = ms(t0), TLSKind(err), ShortErr(err)
		return p.FailAt(st)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	st.Ms = ms(t0)
	if err != nil {
		st.Kind, st.Note = TLSKind(err), ShortErr(err)
		return p.FailAt(st)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	st.Note = resp.Status
	if IsStub(resp, body) {
		st.Kind = KindHTTPStub
		return p.FailAt(st)
	}
	if resp.StatusCode >= 500 {
		st.Kind = KindHTTPError
		return p.FailAt(st)
	}
	st.OK = true
	p.Steps = append(p.Steps, st)
	p.OK = true
	return p
}

// Lookup — адреса имени (IP — как есть).
func Lookup(ctx context.Context, r *net.Resolver, host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, StepTimeout)
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

// Bogon — адреса, которые не могут быть у сайта в интернете: заглушки
// провайдера (0.0.0.0, 127.x, локальные сети).
func Bogon(ips []string) bool {
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

// IsStub — ответ похож на страницу блокировки провайдера.
func IsStub(resp *http.Response, body []byte) bool {
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

// TCPKind — вид поломки соединения.
func TCPKind(err error) string {
	switch {
	case isTimeout(err):
		return KindTCPTimeout
	case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "refused"):
		return KindTCPRefused
	case isReset(err):
		return KindTCPReset
	}
	return KindOther
}

// TLSKind — вид поломки рукопожатия или ответа.
func TLSKind(err error) string {
	var ce *tls.CertificateVerificationError
	var ue x509.UnknownAuthorityError
	var he x509.HostnameError
	switch {
	case errors.As(err, &ce), errors.As(err, &ue), errors.As(err, &he):
		return KindTLSCert
	case isTimeout(err):
		return KindTLSTimeout
	case isReset(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return KindTLSReset
	}
	return KindOther
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

// ShortErr — ошибка не длиннее 160 знаков.
func ShortErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
