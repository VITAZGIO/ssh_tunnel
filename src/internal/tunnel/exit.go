package tunnel

// Цепочка выхода (ТЗ 8): устройство подключается к серверу А, а в интернет
// выходит только через сервер Б (и, может быть, В).
//
// Как устроено. Внутри SSH-сессии с А открывается канал direct-tcpip до
// SSH-порта Б, и по нему идёт вторая, вложенная SSH-сессия — со своим
// ключом профиля Б и своей проверкой ключа сервера. А видит только
// шифрованный поток. Третий сервер — так же внутри сессии с Б.
//
// Обхода нет нарочно: всё, что ядро отправило «через сервер»
// (core.RouteServer), — соединения, DNS через сервер, ретранслятор UDP,
// проверка адреса и тест скорости — идёт через Tunnel.Dial, а он при
// заданной цепочке ходит только по вложенным сессиям. Нет живой вложенной
// сессии — соединение получает ошибку ExitDownError, ни через А, ни напрямую
// оно не пойдёт. Сеть устройств и служебное соединение с самим А
// (пользователи сервера, карта сети) идут через пул А, как без цепочки
// (serverDial, DialEntry).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"sshtunnel/internal/hostkey"
)

// MaxExitHops — сколько серверов можно поставить после входного: цепочка
// бывает не длиннее трёх серверов.
const MaxExitHops = 2

// Hop — один сервер цепочки после входного. Поля — из его профиля.
type Hop struct {
	Name    string
	Host    string
	SSHPort int
	User    string
	KeyPath string
	Cipher  string
	// UDPRelay — на этом сервере стоит ретранслятор UDP (галочка его
	// профиля). Смотрится только у последнего: UDP выходит через него.
	UDPRelay bool
	// Need — звено из панели сервера, для которого на устройстве нет профиля
	// («адрес:порт», ТЗ 16): туннель не стартует, а говорит, чего не хватает.
	Need string
}

func (h Hop) addr() string {
	port := h.SSHPort
	if port <= 0 {
		port = 22
	}
	return net.JoinHostPort(h.Host, fmt.Sprint(port))
}

// ExitDownError — интернет выключен цепочкой: последний сервер (или один из
// промежуточных) недоступен, а мимо него трафик не пускается. Hop — номер
// сервера цепочки (0 — первый после входного), Name — его имя.
type ExitDownError struct {
	Hop  int
	Name string
	Err  error
}

func (e *ExitDownError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("интернет выключен цепочкой: выход через «%s» ещё не на связи", e.Name)
	}
	return fmt.Sprintf("интернет выключен цепочкой: выход через «%s» недоступен (%v)", e.Name, e.Err)
}
func (e *ExitDownError) Unwrap() error { return e.Err }

// ExitStatus — как живёт цепочка, для экрана и API.
type ExitStatus struct {
	// Path — имена серверов от входного до последнего.
	Path []string
	// Up — есть живая сессия с последним сервером.
	Up bool
	// Seen — она хоть раз поднималась за жизнь туннеля.
	Seen bool
	// Err — почему сейчас не на связи (*ExitDownError); nil — неизвестно
	// (ещё подключаемся) или всё в порядке.
	Err error
}

// Пауза между попытками поднять вложенную сессию: нарастает от min до max.
// Меньше, чем у пула А (30 с): человек только что поднял Б и ждёт, что
// интернет вернётся сам. Переменные — для тестов.
var (
	exitRetryMin = time.Second
	exitRetryMax = 15 * time.Second
)

// softDeadlineConn эмулирует SetDeadline таймером для соединений, которые
// его не умеют (канал direct-tcpip через client.Dial). SetWriteDeadline не
// нужен отдельно — sshHandshakeWire и wireConn пользуются только общим
// SetDeadline.
type softDeadlineConn struct {
	net.Conn
	mu    sync.Mutex
	timer *time.Timer
}

func withSoftDeadline(c net.Conn) net.Conn { return &softDeadlineConn{Conn: c} }

func (c *softDeadlineConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if t.IsZero() {
		return nil
	}
	d := time.Until(t)
	if d <= 0 {
		c.Conn.Close()
		return nil
	}
	c.timer = time.AfterFunc(d, func() { c.Conn.Close() })
	return nil
}

func (c *softDeadlineConn) SetReadDeadline(t time.Time) error  { return c.SetDeadline(t) }
func (c *softDeadlineConn) SetWriteDeadline(t time.Time) error { return c.SetDeadline(t) }

// chain — одна вложенная сессия: клиенты всех серверов цепочки по порядку,
// последний и есть выход. done закрывается, когда умер любой из них
// (или сессия с А под ними).
type chain struct {
	clients []*ssh.Client
	done    chan struct{}
	once    sync.Once
}

func newChain(clients []*ssh.Client) *chain {
	c := &chain{clients: clients, done: make(chan struct{})}
	for _, cl := range clients {
		go func(cl *ssh.Client) {
			cl.Wait()
			c.close()
		}(cl)
	}
	return c
}

func (c *chain) last() *ssh.Client { return c.clients[len(c.clients)-1] }

// close закрывает сессии сверху вниз: сначала последнюю, потом те, внутри
// которых она шла.
func (c *chain) close() {
	c.once.Do(func() {
		for i := len(c.clients) - 1; i >= 0; i-- {
			c.clients[i].Close()
		}
		close(c.done)
	})
}

// exitLink — слот пула выхода.
type exitLink struct {
	mu sync.RWMutex
	ch *chain
}

func (l *exitLink) get() *chain {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.ch
}

func (l *exitLink) set(c *chain) {
	l.mu.Lock()
	old := l.ch
	l.ch = c
	l.mu.Unlock()
	if old != nil && old != c {
		old.close()
	}
}

// exitState — общее состояние пула выхода.
type exitState struct {
	mu      sync.Mutex
	links   []*exitLink
	signers []ssh.Signer
	err     error // последняя ошибка основного слота
	seen    bool
	up      bool // последнее, о чём сообщили наружу
}

// Chained — у туннеля задана цепочка выхода.
func (t *Tunnel) Chained() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.cfg.Exit) > 0
}

func (t *Tunnel) hops() []Hop {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cfg.Exit
}

// loadExitSigners читает ключи серверов цепочки. Нет ключа — туннель не
// стартует: молча выпустить интернет через А нельзя.
func (t *Tunnel) loadExitSigners() error {
	hops := t.hops()
	if len(hops) > MaxExitHops {
		return fmt.Errorf("цепочка выхода длиннее %d серверов после входного", MaxExitHops)
	}
	signers := make([]ssh.Signer, len(hops))
	for i, h := range hops {
		if h.Need != "" {
			return fmt.Errorf("в цепочке выхода нужен профиль сервера %s — добавь его на этом устройстве или поменяй цепочку", h.Need)
		}
		if h.Host == "" {
			return fmt.Errorf("в цепочке выхода у сервера «%s» не указан адрес", h.Name)
		}
		key, err := os.ReadFile(h.KeyPath)
		if err != nil {
			return fmt.Errorf("цепочка выхода: не могу прочитать ключ сервера «%s» (%s): %w", h.Name, h.KeyPath, err)
		}
		s, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return fmt.Errorf("цепочка выхода: ключ сервера «%s» повреждён или зашифрован паролем: %w", h.Name, err)
		}
		signers[i] = s
	}
	t.exit.mu.Lock()
	t.exit.signers = signers
	t.exit.mu.Unlock()
	return nil
}

// hopConfig — настройки SSH для сервера цепочки: свой пользователь, ключ и
// шифр из его профиля; ключ сервера — по тому же known_hosts, что и у А.
func (t *Tunnel) hopConfig(i int, h Hop) *ssh.ClientConfig {
	t.exit.mu.Lock()
	signer := t.exit.signers[i]
	t.exit.mu.Unlock()
	return &ssh.ClientConfig{
		User: h.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostkey.CallbackByName(t.cfg.KnownHostsPath, func(host, fp string) {
			t.bus.Infof("Ключ сервера %s (цепочка выхода) запомнен: %s", host, fp)
		}),
		Timeout: t.dialTimeout(),
		Config:  ssh.Config{Ciphers: sshCiphers(h.Cipher)},
	}
}

// dialChain поднимает одну вложенную сессию через пул А. Ошибка —
// *ExitDownError с номером сервера, на котором цепочка оборвалась;
// смена ключа сервера внутри — как есть (hostkey.ErrChanged).
func (t *Tunnel) dialChain(ctx context.Context) (*chain, error) {
	hops := t.hops()
	via := func(network, addr string) (net.Conn, error) {
		conn, err, had := t.tryDial(network, addr)
		if !had {
			return nil, ErrNoServer
		}
		return conn, err
	}
	var clients []*ssh.Client
	fail := func(i int, err error) (*chain, error) {
		for j := len(clients) - 1; j >= 0; j-- {
			clients[j].Close()
		}
		var changed *hostkey.ErrChanged
		if !errors.As(err, &changed) && !errors.Is(err, ErrNoServer) {
			err = classifyConnError(fmt.Errorf("не удалось подключиться к %s через предыдущий сервер: %w", hops[i].addr(), err))
		}
		return nil, &ExitDownError{Hop: i, Name: hops[i].Name, Err: err}
	}
	for i, h := range hops {
		addr := h.addr()
		conn, err := via("tcp", addr)
		if err != nil {
			return fail(i, err)
		}
		cfg := t.hopConfig(i, h)
		// Канал direct-tcpip (client.Dial) не умеет SetDeadline — сервер SSH
		// такого соединения не даёт («deadline not supported»), а
		// sshHandshakeWire его требует для срока «тишина/потолок». Снаружи
		// эмулируем дедлайн таймером, который просто закрывает канал.
		c, _, err := sshHandshakeWire(ctx, withSoftDeadline(conn), addr, cfg, cfg.Timeout)
		if err != nil {
			return fail(i, err)
		}
		clients = append(clients, c)
		via = c.Dial
	}
	return newChain(clients), nil
}

// launchExit заводит слоты пула выхода — столько же, сколько у пула А:
// вложенные сессии раскладываются по его соединениям, и скорость цепочки
// упирается в те же потолки, что и прямая.
func (t *Tunnel) launchExit(ctx context.Context, n int) {
	if !t.Chained() {
		return
	}
	links := make([]*exitLink, n)
	for i := range links {
		links[i] = &exitLink{}
	}
	t.exit.mu.Lock()
	t.exit.links = links
	t.exit.mu.Unlock()
	for i := range links {
		t.wg.Add(1)
		go t.keepExitAlive(ctx, links[i], i)
	}
}

// clearExit рвёт все вложенные сессии (слив, остановка).
func (t *Tunnel) clearExit() {
	t.exit.mu.Lock()
	links := t.exit.links
	t.exit.links = nil
	t.exit.mu.Unlock()
	for _, l := range links {
		l.set(nil)
	}
	t.exitChanged()
}

func (t *Tunnel) exitLinks() []*exitLink {
	t.exit.mu.Lock()
	defer t.exit.mu.Unlock()
	return t.exit.links
}

// exitPrimaryUp — основной слот выхода на связи.
func (t *Tunnel) exitPrimaryUp() bool {
	links := t.exitLinks()
	return len(links) > 0 && links[0].get() != nil
}

// exitAnyUp — хоть одна вложенная сессия жива: интернет есть.
func (t *Tunnel) exitAnyUp() bool {
	for _, l := range t.exitLinks() {
		if l.get() != nil {
			return true
		}
	}
	return false
}

// keepExitAlive держит один слот выхода: поднимает вложенную сессию, когда
// жив пул А, замечает её смерть (умерла сама, умер А под ней, не отвечает
// на keepalive) и поднимает заново с нарастающей паузой.
func (t *Tunnel) keepExitAlive(ctx context.Context, l *exitLink, idx int) {
	defer t.wg.Done()
	backoff := exitRetryMin
	wait := func(d time.Duration, kick <-chan struct{}) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
		case <-kick:
		}
		return true
	}
	for {
		if ctx.Err() != nil {
			return
		}
		kick := t.currentKick()
		ch := l.get()
		if ch == nil {
			// Без А до Б не дойти; остальные слоты ждут основной, как и в
			// пуле А, чтобы не стучаться вчетвером в упавший сервер.
			if !t.primaryUp() || (idx > 0 && !t.exitPrimaryUp()) {
				if !wait(200*time.Millisecond, kick) {
					return
				}
				continue
			}
			c, err := t.dialChain(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if idx == 0 {
					t.setExitErr(err)
				}
				if !wait(jitter(backoff), kick) {
					return
				}
				if backoff *= 2; backoff > exitRetryMax {
					backoff = exitRetryMax
				}
				continue
			}
			if ctx.Err() != nil {
				c.close()
				return
			}
			l.set(c)
			backoff = exitRetryMin
			if idx == 0 {
				t.setExitErr(nil)
			}
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-ch.done:
			l.mu.Lock()
			if l.ch == ch {
				l.ch = nil
			}
			l.mu.Unlock()
			if idx == 0 {
				t.bus.Warnf("Цепочка выхода оборвалась — интернет выключен, пока она не поднимется снова")
				t.setExitErr(nil)
			}
			continue
		case <-kick:
			l.set(nil)
			continue
		case <-time.After(t.keepAliveEvery()):
		}
		// Проверка живости — по всей цепочке сразу: запрос к последнему
		// серверу идёт через все промежуточные.
		reply := make(chan error, 1)
		go func(c *ssh.Client) {
			_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
			reply <- err
		}(ch.last())
		select {
		case err := <-reply:
			if err != nil {
				l.set(nil)
			}
		case <-time.After(keepaliveReply * time.Duration(keepaliveMisses)):
			t.bus.Warnf("Цепочка выхода не отвечает — переподключаю")
			l.set(nil)
		case <-ctx.Done():
			return
		}
	}
}

// setExitErr запоминает, почему основной слот выхода не на связи (nil —
// на связи или ещё не ясно), и сообщает экрану, если что-то изменилось.
func (t *Tunnel) setExitErr(err error) {
	up := t.exitAnyUp()
	t.exit.mu.Lock()
	changed := up != t.exit.up || (err == nil) != (t.exit.err == nil) ||
		(err != nil && t.exit.err != nil && err.Error() != t.exit.err.Error())
	wasSeen := t.exit.seen
	t.exit.err, t.exit.up = err, up
	if up {
		t.exit.seen = true
	}
	t.exit.mu.Unlock()
	if !changed {
		return
	}
	switch {
	case up && wasSeen:
		t.bus.Infof("Цепочка выхода снова на связи: %s", strings.Join(t.ExitStatus().Path, " → "))
	case up:
		t.bus.Infof("Цепочка выхода на связи: %s", strings.Join(t.ExitStatus().Path, " → "))
	case err != nil:
		t.bus.Warnf("%v", err)
	}
	t.exitChanged()
}

func (t *Tunnel) exitChanged() {
	t.mu.RLock()
	f := t.cfg.OnExitChange
	t.mu.RUnlock()
	if f != nil {
		f()
	}
}

// ExitStatus — состояние цепочки; Path пуст — цепочки нет.
func (t *Tunnel) ExitStatus() ExitStatus {
	hops := t.hops()
	if len(hops) == 0 {
		return ExitStatus{}
	}
	t.mu.RLock()
	entry := t.cfg.Name
	if entry == "" {
		entry = t.cfg.Host
	}
	t.mu.RUnlock()
	path := []string{entry}
	for _, h := range hops {
		path = append(path, h.Name)
	}
	t.exit.mu.Lock()
	st := ExitStatus{Path: path, Seen: t.exit.seen, Err: t.exit.err}
	t.exit.mu.Unlock()
	st.Up = !t.draining.Load() && t.exitAnyUp()
	if st.Up {
		st.Err = nil
	} else if st.Err == nil && !t.primaryUp() && t.ServerReached() {
		st.Err = &ExitDownError{Hop: -1, Name: entry, Err: ErrNoServer}
	}
	return st
}

// dialExit — соединение в интернет через последний сервер цепочки. Ждёт
// живую вложенную сессию столько же, сколько Dial ждёт пул (dialWait), и
// отказывает. Никакого запасного пути нет.
func (t *Tunnel) dialExit(network, target string) (net.Conn, error) {
	deadline := time.Now().Add(dialWait)
	for {
		if conn, err, had := t.tryExit(network, target); had {
			return conn, err
		}
		if time.Now().After(deadline) {
			return nil, t.exitDownErr()
		}
		select {
		case <-t.ctx.Done():
			return nil, t.exitDownErr()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// tryExit — один проход по слотам выхода. had=false — живых сессий нет
// (или все оказались мёртвыми), есть смысл подождать.
func (t *Tunnel) tryExit(network, target string) (net.Conn, error, bool) {
	links := t.exitLinks()
	for i := 0; i < len(links); i++ {
		idx := int(t.exitRR.Add(1)-1) % len(links)
		ch := links[idx].get()
		if ch == nil {
			continue
		}
		c, err := ch.last().Dial(network, target)
		if err == nil {
			return c, nil, true
		}
		if isLinkDead(err) {
			links[idx].set(nil)
			continue
		}
		return nil, err, true
	}
	return nil, nil, false
}

func (t *Tunnel) exitDownErr() error {
	if st := t.ExitStatus(); st.Err != nil {
		var ed *ExitDownError
		if errors.As(st.Err, &ed) {
			return ed
		}
		return &ExitDownError{Hop: len(st.Path) - 2, Name: st.Path[len(st.Path)-1], Err: st.Err}
	}
	hops := t.hops()
	return &ExitDownError{Hop: len(hops) - 1, Name: hops[len(hops)-1].Name}
}

// exitClient — сессия с последним сервером для замера задержки; nil — нет.
func (t *Tunnel) exitClient() *ssh.Client {
	links := t.exitLinks()
	if len(links) == 0 {
		return nil
	}
	if ch := links[0].get(); ch != nil {
		return ch.last()
	}
	return nil
}

// udpRelayOn — пробрасывать ли UDP: с цепочкой решает галочка последнего
// сервера — ретранслятор нужен именно на нём.
func (t *Tunnel) udpRelayOn() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if n := len(t.cfg.Exit); n > 0 {
		return t.cfg.Exit[n-1].UDPRelay
	}
	return t.cfg.UDPRelayEnabled
}
