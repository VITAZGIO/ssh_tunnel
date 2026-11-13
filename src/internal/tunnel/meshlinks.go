package tunnel

// Лёгкие соединения с дополнительными серверами сети устройств (ТЗ 12).
//
// Сеть устройств живёт на нескольких серверах сразу, и устройство держит
// управляющее соединение с каждым, который знает. С сервером туннеля оно идёт
// через пул (MeshHost.Attach). С остальными — через MeshLinks: одно
// SSH-соединение на сервер, только для сети устройств — трафик в интернет по
// нему не ходит, локальных прокси нет. Вход — как в обычный профиль: свой
// ключ и пользователь, ключ сервера проверяется по known_hosts той же
// программы (первый раз запоминается, сменился — отказ).
//
// Батарея: keepalive SSH раз в минуту (а не раз в 20 с, как у пула), а meshd
// пингует такое соединение раз в 60 с (hello с quiet, см. mesh.go).

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"sshtunnel/internal/events"
)

// MeshServer — дополнительный сервер сети: как на него войти.
type MeshServer struct {
	// Key — как называть сервер (обычно id профиля): по нему MeshLinks
	// понимает, что сервер тот же, а MeshHost различает пути.
	Key     string
	Name    string
	Host    string
	SSHPort int
	User    string
	KeyPath string
	// Общие для программы: где known_hosts, как помечать сокеты (Android,
	// режим VPN), чем разрешать имена мимо туннеля, по какому протоколу.
	KnownHostsPath string
	IPVersion      string
	Cipher         string
	ProtectSocket  func(network, address string, c syscall.RawConn) error
	Resolver       *net.Resolver
}

// meshLinkKeepAlive — как часто проверять лёгкое соединение.
const meshLinkKeepAlive = 60 * time.Second

// MeshLinks держит лёгкие соединения с дополнительными серверами и отдаёт
// их хозяину клиента сети устройств.
type MeshLinks struct {
	host *MeshHost
	log  func(level, text string)

	mu    sync.Mutex
	links map[string]*meshLink
}

type meshLink struct {
	srv    MeshServer
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	client *ssh.Client
	err    error
}

// NewMeshLinks — соединения для хозяина h; log — журнал ("info", "warn").
func NewMeshLinks(h *MeshHost, log func(level, text string)) *MeshLinks {
	if log == nil {
		log = func(string, string) {}
	}
	return &MeshLinks{host: h, log: log, links: map[string]*meshLink{}}
}

// Set приводит соединения к списку: новые поднимаются, пропавшие и
// изменившиеся закрываются (изменившиеся — и поднимаются заново).
func (m *MeshLinks) Set(list []MeshServer) {
	want := map[string]MeshServer{}
	for _, s := range list {
		want[s.Key] = s
	}
	m.mu.Lock()
	var stop []*meshLink
	for key, l := range m.links {
		if s, ok := want[key]; !ok || !sameMeshServer(s, l.srv) {
			stop = append(stop, l)
			delete(m.links, key)
		}
	}
	var start []*meshLink
	for key, s := range want {
		if _, ok := m.links[key]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		l := &meshLink{srv: s, cancel: cancel, done: make(chan struct{})}
		m.links[key] = l
		start = append(start, l)
		go func() {
			defer close(l.done)
			m.run(ctx, l)
		}()
	}
	m.mu.Unlock()
	for _, l := range stop {
		m.stop(l)
	}
	for _, l := range start {
		key := l.srv.Key
		m.host.AttachExtra(key, l.srv.Host, l.dial)
	}
}

// Stop закрывает все соединения.
func (m *MeshLinks) Stop() { m.Set(nil) }

// Keys — серверы, которые сейчас в списке.
func (m *MeshLinks) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.links))
	for k := range m.links {
		out = append(out, k)
	}
	return out
}

func (m *MeshLinks) stop(l *meshLink) {
	m.host.DetachExtra(l.srv.Key)
	l.cancel()
	l.mu.Lock()
	if l.client != nil {
		l.client.Close()
	}
	l.mu.Unlock()
	select {
	case <-l.done:
	case <-time.After(5 * time.Second):
	}
}

// dial — путь к meshd этого сервера; нет соединения — ErrNoServer сразу.
func (l *meshLink) dial(network, addr string) (net.Conn, error) {
	l.mu.Lock()
	c := l.client
	l.mu.Unlock()
	if c == nil {
		return nil, ErrNoServer
	}
	return c.Dial(network, addr)
}

// run держит соединение: входит, проверяет keepalive-ом, при обрыве
// переподключается с паузой от 5 с до 2 мин.
func (m *MeshLinks) run(ctx context.Context, l *meshLink) {
	// Туннель-«одиночка» только ради входа: тот же разбор ключа, та же
	// проверка ключа сервера и пометка сокетов, что у обычного профиля. Он
	// никогда не стартует; события — в свою шину, чтобы не путать экран
	// состоянием чужого сервера.
	t := New(Config{
		Host: l.srv.Host, SSHPort: l.srv.SSHPort, User: l.srv.User, KeyPath: l.srv.KeyPath,
		KnownHostsPath: l.srv.KnownHostsPath, IPVersion: l.srv.IPVersion, Cipher: l.srv.Cipher,
		ProtectSocket: l.srv.ProtectSocket, Resolver: l.srv.Resolver,
	}, events.NewBus())
	name := l.srv.Name
	if name == "" {
		name = l.srv.Host
	}
	backoff := 5 * time.Second
	reported := ""
	for ctx.Err() == nil {
		err := t.loadSigner()
		var c *ssh.Client
		if err == nil {
			c, _, err = t.dial(ctx)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			l.mu.Lock()
			l.err = err
			l.mu.Unlock()
			if text := err.Error(); text != reported {
				m.log("warn", "Сеть устройств: сервер "+name+" недоступен: "+text)
				reported = text
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(jitter(backoff)):
			}
			if backoff < 2*time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff, reported = 5*time.Second, ""
		l.mu.Lock()
		l.client, l.err = c, nil
		l.mu.Unlock()
		m.log("info", "Сеть устройств: связь с сервером "+name+" (только для сети устройств)")
		err = keepMeshLink(ctx, c)
		l.mu.Lock()
		l.client = nil
		l.mu.Unlock()
		c.Close()
		if ctx.Err() != nil {
			return
		}
		m.log("warn", "Сеть устройств: связь с сервером "+name+" оборвалась: "+err.Error())
	}
}

// keepMeshLink ждёт смерти соединения: keepalive раз в минуту, ответа нет
// дважды подряд (по 15 с) — мёртво.
func keepMeshLink(ctx context.Context, c *ssh.Client) error {
	closed := make(chan error, 1)
	go func() { closed <- c.Wait() }()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-closed:
			if err == nil {
				err = errors.New("сервер закрыл соединение")
			}
			return err
		case <-time.After(meshLinkKeepAlive):
		}
		reply := make(chan error, 1)
		go func() {
			_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
			reply <- err
		}()
		select {
		case err := <-reply:
			if err != nil {
				return err
			}
			misses = 0
		case <-time.After(15 * time.Second):
			misses++
			if misses >= 2 {
				return errors.New("сервер не отвечает")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// MeshServerAddr — «хост:порт» сервера для журнала и сравнения.
func MeshServerAddr(s MeshServer) string {
	return net.JoinHostPort(s.Host, strconv.Itoa(s.SSHPort))
}

// sameMeshServer — тот же вход (функции и резолвер не сравниваются: они
// общие для программы).
func sameMeshServer(a, b MeshServer) bool {
	return a.Key == b.Key && a.Name == b.Name && a.Host == b.Host && a.SSHPort == b.SSHPort &&
		a.User == b.User && a.KeyPath == b.KeyPath && a.KnownHostsPath == b.KnownHostsPath &&
		a.IPVersion == b.IPVersion && a.Cipher == b.Cipher
}
