// Package traffic — плагин «Расход трафика» (ТЗ 5, п. 9):
// сколько прошло через сервер, напрямую и в сеть устройств, по дням и по
// программам.
//
// Отдельного перехвата нет: туннель и так видит каждое соединение, знает
// программу (procinfo, на телефоне — владелец по системе) и путь, который
// выбрало ядро (core.Decide). Он оборачивает встречное соединение в Wrap —
// и байты считаются здесь. Пока приложение не работает, Wrap возвращает
// соединение как есть: ни строчки лишней работы.
package traffic

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Пути — как у core.Route, но строкой: так они лежат в файле и в API.
const (
	RouteServer = "server" // через сервер (VPN)
	RouteDirect = "direct" // напрямую
	RouteMesh   = "mesh"   // в сеть устройств
)

// sink — куда складывать байты (работающий модуль приложения).
type sink interface {
	add(proc, route string, up, down int64)
}

type sinkBox struct{ s sink }

var cur atomic.Pointer[sinkBox]

func setSink(s sink) {
	if s == nil {
		cur.Store(nil)
		return
	}
	cur.Store(&sinkBox{s})
}

// On — считается ли трафик сейчас (приложение добавлено и включено).
func On() bool { return cur.Load() != nil }

// Count добавляет байты программы proc по пути route.
func Count(proc, route string, up, down int64) {
	if b := cur.Load(); b != nil && (up > 0 || down > 0) {
		b.s.add(proc, route, up, down)
	}
}

// Wrap считает байты соединения c: записанное в него — «отдано», прочитанное
// — «получено». c — встречное соединение (к сайту, серверу, устройству), а
// не соединение с программой. Учёт выключен — c возвращается как есть.
func Wrap(c net.Conn, proc, route string) net.Conn {
	if c == nil || !On() {
		return c
	}
	return &conn{Conn: c, proc: proc, route: route}
}

// Отдавать в учёт не чаще раза в flushBytes или flushEvery: Read и Write на
// потоке видео — тысячи в секунду, а замок учёта общий.
const (
	flushBytes = 256 << 10
	flushEvery = 2 * time.Second
)

type conn struct {
	net.Conn
	proc, route string

	mu       sync.Mutex
	up, down int64
	last     time.Time
}

func (c *conn) note(up, down int64) {
	c.mu.Lock()
	c.up += up
	c.down += down
	if c.up+c.down < flushBytes && time.Since(c.last) < flushEvery {
		c.mu.Unlock()
		return
	}
	u, d := c.up, c.down
	c.up, c.down, c.last = 0, 0, time.Now()
	c.mu.Unlock()
	Count(c.proc, c.route, u, d)
}

func (c *conn) flush() {
	c.mu.Lock()
	u, d := c.up, c.down
	c.up, c.down = 0, 0
	c.mu.Unlock()
	Count(c.proc, c.route, u, d)
}

func (c *conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.note(0, int64(n))
	}
	return n, err
}

func (c *conn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.note(int64(n), 0)
	}
	return n, err
}

func (c *conn) Close() error {
	err := c.Conn.Close()
	c.flush()
	return err
}

// CloseWrite — полузакрытие, как у самого соединения (туннель на нём держит
// «дописал запрос — жду ответ»).
func (c *conn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.SetReadDeadline(time.Now().Add(30 * time.Second))
}

// Сеть телефона: Wi-Fi или мобильная. На ПК пусто — деления нет.
var netKind atomic.Value

// SetNet — какая сеть сейчас у телефона: "wifi", "mobile" или "" (не знаем).
func SetNet(kind string) { netKind.Store(kind) }

func currentNet() string {
	s, _ := netKind.Load().(string)
	return s
}
