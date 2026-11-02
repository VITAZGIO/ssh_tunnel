package tunnel

import (
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"sshtunnel/internal/core"
	"sshtunnel/internal/events"
	"sshtunnel/internal/procinfo"
)

const syscallECONNRESET = syscall.ECONNRESET

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// bufSize — размер буфера перекачки. io.Copy по умолчанию берёт 32 КБ; на
// канале с задержкой это лишние переключения и системные вызовы, 64 КБ
// заметно ровнее ложатся на SSH-пакеты и окно канала.
const bufSize = 64 * 1024

var bufPool = make(chan []byte, 64)

func getBuf() []byte {
	select {
	case b := <-bufPool:
		return b
	default:
		return make([]byte, bufSize)
	}
}

func putBuf(b []byte) {
	select {
	case bufPool <- b:
	default:
	}
}

// request описывает разобранный запрос от локального приложения, независимо
// от того, каким протоколом он пришёл.
type request struct {
	conn   net.Conn
	target string // host:port, куда хочет попасть приложение
	proto  string // socks4 / socks4a / socks5 / http-connect
	byIP   bool   // приложение прислало готовый IP => DNS резолвил сам

	// src — откуда читать данные клиента. Обычно это сам conn, но если
	// протокол разбирался через bufio, часть байтов клиента уже лежит в его
	// буфере, и читать надо именно оттуда, иначе они потеряются.
	src io.Reader

	writeErr func(error)
	writeOK  func(net.Conn) error
}

// serve — общая для всех протоколов часть: определить процесс, открыть
// соединение через SSH, ответить клиенту и перекачать данные в обе стороны.
func (t *Tunnel) serve(r request) {
	defer r.conn.Close()

	proc, pid := lookupProcess(r.conn)

	route, c := t.decide(proc, r.target)
	direct := route == core.RouteDirect
	remote, err := t.dialRoute(route, r.target)
	if err != nil {
		t.bus.Publish(eventConn(proc, pid, r.target, r.proto, r.byIP, direct, err))
		if r.writeErr != nil {
			r.writeErr(err)
		}
		return
	}
	defer remote.Close()

	if r.writeOK != nil {
		if err := r.writeOK(r.conn); err != nil {
			return
		}
	}
	untrack := t.track(c, route, entryProxy, func() { r.conn.Close(); remote.Close() })
	defer untrack()

	t.bus.Publish(eventConn(proc, pid, r.target, r.proto, r.byIP, direct, nil))

	t.stats.active.Add(1)
	t.stats.total.Add(1)
	defer t.stats.active.Add(-1)

	src := r.src
	if src == nil {
		src = r.conn
	}
	t.pump(r.conn, src, remote)

	if t.cfg.Verbose {
		t.bus.Infof("%s → %s: закрыто", displayProc(proc), r.target)
	}
}

// pump перекачивает данные в обе стороны и ждёт завершения ОБОИХ направлений.
// Если выйти по первому, второе направление обрывается на полуслове — именно
// это раньше давало ERR_CONNECTION_RESET на середине страницы.
//
// local — соединение с приложением (нужно для закрытия на запись),
// localSrc — поток чтения от приложения (может быть буфером поверх local).
func (t *Tunnel) pump(local net.Conn, localSrc io.Reader, remote net.Conn) {
	done := make(chan struct{}, 2)

	go func() {
		buf := getBuf()
		n, _ := io.CopyBuffer(remote, localSrc, buf)
		putBuf(buf)
		t.stats.up.Add(n)
		closeWrite(remote)
		done <- struct{}{}
	}()
	go func() {
		buf := getBuf()
		n, _ := io.CopyBuffer(local, remote, buf)
		putBuf(buf)
		t.stats.down.Add(n)
		closeWrite(local)
		done <- struct{}{}
	}()

	<-done
	<-done
}

// closeWrite сообщает другой стороне "я больше не пишу", не разрывая приём.
// Многие протоколы на этом и держатся: клиент дописал запрос и ждёт ответ.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	// Если полузакрытие недоступно, мягко ограничим время дочитывания, чтобы
	// соединение не висело вечно.
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
}

// dialFor открывает соединение до цели тем путём, который ядро выбрало для
// этой программы (core.Decide). direct — соединение идёт мимо сервера.
func (t *Tunnel) dialFor(process, target string) (net.Conn, bool, error) {
	route, _ := t.decide(process, target)
	c, err := t.dialRoute(route, target)
	return c, route == core.RouteDirect, err
}

// listedDirect — цель попала в пользовательский список «всегда напрямую».
// Это про чужие сети, о которых программа знать не может: mesh-VPN, рабочий
// VPN, самодельный WireGuard, — и про свои сайты.
//
// Пришёл голый адрес, а в списке имя — сверяем имя, под которым этот адрес
// известен (см. LearnDirect и directnames.go). Имя сверяется с текущим
// списком: убрали его из настроек — адрес снова идёт через сервер.
func (t *Tunnel) listedDirect(target string) bool {
	t.mu.RLock()
	d := t.cfg.Direct
	t.mu.RUnlock()
	if d.Match(target) {
		return true
	}
	name, ok := t.learnedName(target)
	return ok && d.Match(name)
}

func eventConn(proc string, pid int, target, proto string, byIP, direct bool, err error) events.Event {
	e := events.Event{
		Kind: events.KindConn, Process: proc, PID: pid,
		Target: target, Proto: proto, DNSLeak: byIP, Direct: direct,
	}
	if err != nil {
		e.Failed = true
		e.Error = shortErr(err)
	}
	return e
}

func lookupProcess(c net.Conn) (string, int) {
	addr, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return "", 0
	}
	return procinfo.Lookup(uint16(addr.Port))
}

func displayProc(p string) string {
	if p == "" {
		return "приложение"
	}
	return p
}

// shortErr убирает из ошибки техническую обвязку — в интерфейсе нужен смысл,
// а не внутренности библиотеки.
func shortErr(err error) string {
	s := err.Error()
	switch {
	case containsAny(s, "connection refused"):
		return "сервер отказал в соединении"
	case containsAny(s, "no such host", "Name or service not known"):
		return "имя не разрешилось"
	case containsAny(s, "i/o timeout", "timeout"):
		return "таймаут"
	case containsAny(s, "administratively prohibited"):
		return "сервер запретил исходящее соединение"
	}
	if i := strings.LastIndex(s, ": "); i > 0 && len(s)-i < 60 {
		return s[i+2:]
	}
	return s
}
