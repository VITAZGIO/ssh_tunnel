package tunnel

// Настоящие адреса, разрешённые через сервер (ТЗ 9).
//
// Наш DNS в режиме VPN раньше отвечал на всё подставным адресом 198.18.x.x,
// а настоящее имя разрешал сервер в момент соединения. Программы, которые
// перед соединением проверяют адрес (защита от SSRF, списки по IP), такой
// адрес считают внутренним и отказываются. Теперь имя «через сервер»
// разрешается сразу и отдаётся программе настоящим адресом. Спрашивает
// сервер: запрос по TCP уходит через SSH-канал к открытому резолверу, и
// провайдер не видит ни имени, ни ответа — подменить его ему нечем.
//
// Куда потом вести соединение, ядро решает по имени (core.Decide): его
// приносит само соединение (core.SniffHost) или таблица «адрес → имя» из
// этих ответов (LearnName).

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"
)

// serverResolvers — резолверы, которых спрашиваем через сервер, по порядку.
// Публичные, а не DNS самого сервера: у VPS он бывает кривым, а этим
// адресам до сервера всегда близко (anycast), и CDN отдаёт адреса рядом с
// ним — туда, откуда соединение и пойдёт.
var serverResolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// serverDNSTTL — сколько помнить ответ. Столько же живёт ответ нашего DNS
// у системы (см. vpnlayer), так что переспрашивать раньше незачем.
const serverDNSTTL = time.Minute

// serverDNSMax — потолок памяти ответов.
const serverDNSMax = 4096

type serverDNSEntry struct {
	ips   []net.IP
	until time.Time
}

type serverDNS struct {
	mu sync.Mutex
	m  map[string]serverDNSEntry
}

// lookupViaServer — разрешение имени через сервер; переменная, чтобы тесты
// обходились без сервера.
var lookupViaServer = func(ctx context.Context, t *Tunnel, name string) ([]net.IP, error) {
	r := &net.Resolver{
		PreferGo: true,
		// Адрес из настроек системы не нужен (это наш же DNS): соединение
		// всегда к резолверу через сервер. Обычный net.Conn, а не
		// PacketConn, — резолвер Go сам говорит по нему DNS поверх TCP.
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var lastErr error
			for _, addr := range serverResolvers {
				c, err := t.Dial("tcp", addr)
				if err == nil {
					return c, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
	// Точка в конце — имя полное: суффиксы поиска системы не подставлять.
	return r.LookupIP(ctx, "ip4", strings.TrimSuffix(name, ".")+".")
}

// LookupViaServer разрешает имя через сервер (только IPv4: адреса шестой
// версии наш DNS не отдаёт, см. android/core/dns.go). Адреса запоминаются
// под это имя: соединение придёт с адресом, а правилам нужно имя.
func (t *Tunnel) LookupViaServer(name string) ([]net.IP, error) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	now := time.Now()
	c := &t.sdns
	c.mu.Lock()
	if e, ok := c.m[name]; ok && now.Before(e.until) {
		c.mu.Unlock()
		t.LearnName(name, e.ips)
		return e.ips, nil
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := lookupViaServer(ctx, t, name)
	if err != nil {
		return nil, err
	}
	var v4 []net.IP
	for _, ip := range ips {
		if ip4 := ip.To4(); ip4 != nil && !fakeNet.Contains(ip4) {
			v4 = append(v4, ip4)
		}
	}
	if len(v4) == 0 {
		return nil, errors.New("у имени нет адресов IPv4")
	}

	c.mu.Lock()
	if c.m == nil {
		c.m = make(map[string]serverDNSEntry)
	}
	if len(c.m) >= serverDNSMax {
		for k, e := range c.m {
			if now.After(e.until) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= serverDNSMax {
			clear(c.m)
		}
	}
	c.m[name] = serverDNSEntry{ips: v4, until: now.Add(serverDNSTTL)}
	c.mu.Unlock()

	t.LearnName(name, v4)
	return v4, nil
}

// resetServerDNS забывает ответы (смена режима DNS, см. ApplyHygiene).
func (t *Tunnel) resetServerDNS() {
	t.sdns.mu.Lock()
	clear(t.sdns.m)
	t.sdns.mu.Unlock()
}
