package probe

// Проверка сайта с сервера (ТЗ 15): тот же путь DNS → TCP → TLS → HTTP, но
// из сети сервера. Сервер не открытый прокси: проверяется только имя в
// интернете на порту 443, запрос — один GET «/» без тела, локальные,
// закрытые и служебные адреса (и имена, которые в них разрешаются)
// отвергаются до соединения, а соединение идёт на уже проверенный адрес —
// подменить его вторым ответом DNS нельзя.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"
)

// ServerResult — проверка сайта с сервера.
type ServerResult struct {
	Site    string    `json:"site"`
	Host    string    `json:"host"`
	Time    time.Time `json:"time"`
	Server  Path      `json:"server"`
	Verdict string    `json:"verdict"` // ServerVerdict
	Text    string    `json:"text"`
}

// ServerOptions — чем сервер ходит в сеть (тесты подменяют).
type ServerOptions struct {
	Resolver *net.Resolver
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	// Allow — можно ли проверять адрес; nil — Public.
	Allow func(netip.Addr) bool
}

// ErrPort — с сервера проверяется только 443.
var ErrPort = errors.New("с сервера проверяется только порт 443")

// закрытые сети, которых нет в netip: общий NAT провайдеров, тестовые
// сети, сеть устройств и подставные адреса (198.18.0.0/15), служебные.
var reserved = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// Public — адрес в интернете: не локальный, не закрытый, не служебный.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// localName — имена, которые бывают только в своей сети.
func localName(h string) bool {
	h = strings.TrimSuffix(h, ".")
	if !strings.Contains(h, ".") {
		return true
	}
	for _, s := range []string{".localhost", ".local", ".lan", ".home", ".internal", ".intranet",
		".corp", ".home.arpa", ".mesh", ".localdomain", ".in-addr.arpa", ".ip6.arpa"} {
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	return h == "localhost"
}

// CheckFromServer — проверить сайт из сети сервера.
func CheckFromServer(ctx context.Context, site, lang string, o ServerOptions) (ServerResult, error) {
	t, err := ParseSite(site)
	if err != nil {
		return ServerResult{}, err
	}
	if t.Port != "443" {
		return ServerResult{}, ErrPort
	}
	t.Path = "/" // свой путь не передаём: проверка одна и та же для всех
	allow := o.Allow
	if allow == nil {
		allow = Public
	}
	if o.Resolver == nil {
		o.Resolver = net.DefaultResolver
	}
	if o.Dial == nil {
		var d net.Dialer
		o.Dial = d.DialContext
	}
	res := ServerResult{Site: site, Host: t.Host, Time: time.Now()}
	finish := func() (ServerResult, error) {
		res.Verdict = ServerVerdict(res.Server)
		res.Text = ServerPhrase(res.Verdict, lang)
		return res, nil
	}
	refuse := func(note string) (ServerResult, error) {
		res.Server = res.Server.FailAt(Step{ID: "dns", Kind: KindRefused, Note: note})
		return finish()
	}
	if a, err := netip.ParseAddr(t.Host); err == nil {
		if !allow(a) {
			return refuse(a.String())
		}
	} else if localName(t.Host) {
		return refuse(t.Host)
	}

	t0 := time.Now()
	ips, err := Lookup(ctx, o.Resolver, t.Host)
	st := Step{ID: "dns", Ms: ms(t0)}
	if err != nil {
		st.Kind, st.Note = KindDNSFail, ShortErr(err)
		res.Server = res.Server.FailAt(st)
		return finish()
	}
	res.Server.IPs = ips
	var target netip.Addr
	for _, s := range ips {
		a, err := netip.ParseAddr(s)
		if err != nil || !allow(a) {
			// Имя в интернете, а адрес — локальный: так ходят в чужую
			// внутреннюю сеть через сервер. Отказ целиком.
			return refuse(s)
		}
		if !target.IsValid() {
			target = a
		}
	}
	st.OK, st.Note = true, strings.Join(ips, ", ")
	res.Server.Steps = append(res.Server.Steps, st)
	res.Server = res.Server.Connect(ctx, t, func(ctx context.Context) (net.Conn, error) {
		return o.Dial(ctx, "tcp", net.JoinHostPort(target.String(), t.Port))
	})
	return finish()
}

// ServerVerdict — код итога проверки с сервера.
func ServerVerdict(p Path) string {
	switch {
	case p.OK:
		return "server_ok"
	case p.Fail == KindRefused:
		return "server_refused"
	case p.Fail == KindDNSFail:
		return "server_no_such"
	}
	return "server_fail"
}

var serverPhrases = map[string][2]string{
	"server_ok":      {"С сервера сайт открывается.", "The site opens from the server."},
	"server_no_such": {"С сервера имя не находится — такого сайта нет.", "The name does not resolve from the server — no such site."},
	"server_refused": {"Сервер не проверяет локальные и закрытые адреса.", "The server does not check local or private addresses."},
	"server_fail":    {"С сервера сайт не открывается: он лежит или не пускает адрес сервера.", "The site does not open from the server: it is down or does not let the server's address in."},
}

// ServerPhrase — итог проверки с сервера одной фразой (ru по умолчанию).
func ServerPhrase(code, lang string) string {
	p, ok := serverPhrases[code]
	if !ok {
		return code
	}
	if lang == "en" {
		return p[1]
	}
	return p[0]
}
