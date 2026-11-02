package core

import (
	"net"

	"sshtunnel/internal/mesh"
	"sshtunnel/internal/routing"
)

// Route — куда вести соединение. Решает только Decide.
type Route int

const (
	RouteServer Route = iota // через сервер (SSH)
	RouteDirect              // напрямую с этого устройства, мимо адаптера
	RouteMesh                // в сеть устройств
	RouteBlock               // никуда: честный отказ
)

func (r Route) String() string {
	switch r {
	case RouteServer:
		return "server"
	case RouteDirect:
		return "direct"
	case RouteMesh:
		return "mesh"
	}
	return "block"
}

// Rules — всё, от чего зависит решение. Собирается из режима (кнопки) и
// настроек; списки и политика меняются на ходу, без переподключения.
type Rules struct {
	// Bypass — круглая кнопка: интернет по правилам через сервер. Выключена
	// — всё, что не к устройствам, идёт напрямую, как без программы.
	Bypass bool
	// Mesh — сеть устройств включена.
	Mesh bool
	// Draining — слив после «Выключить»: связи с сервером уже нет, а
	// локальные прокси ещё донашивают соединения приложений — напрямую.
	Draining bool
	// LocalViaTunnel — галочка «локальную сеть тоже через сервер».
	LocalViaTunnel bool
	// Policy — правила по программам; nil — «все через сервер».
	Policy *routing.Policy
	// Direct — «всегда напрямую», Tunneled — «всегда через сервер» (сильнее
	// всех правил, кроме сети устройств). nil — список пуст.
	Direct   *routing.DirectList
	Tunneled *routing.DirectList
}

// Conn — что известно о соединении в момент решения.
type Conn struct {
	// Process — программа, открывшая соединение (chrome.exe); пусто —
	// неизвестна или правила по программам здесь не действуют (на Android
	// приложения выбирает сама система).
	Process string
	// Target — куда шло приложение, host:port: имя, если оно известно
	// (прокси, восстановлено из fake-IP), иначе адрес.
	Target string
	// Name — имя, под которое этот адрес выдан нашим DNS «настоящим» (имя из
	// «всегда напрямую»); пусто — не выдавался. Сверяется со списками так же,
	// как Target: правило по имени работает и тогда, когда приложение пришло
	// с адресом.
	Name string
}

// named — Target с именем вместо адреса (порт тот же); пусто, если имени нет.
func (c Conn) named() string {
	if c.Name == "" {
		return ""
	}
	if _, port, err := net.SplitHostPort(c.Target); err == nil {
		return net.JoinHostPort(c.Name, port)
	}
	return c.Name
}

// Decide — единственное место, где решается, куда вести соединение. Одно и
// то же для прокси (SOCKS/HTTP), для стека режима VPN и для Android: правило
// пишется один раз.
//
// Порядок:
//  1. Сеть устройств — раньше всего: её адреса достижимы только через неё.
//     Выключена — отказ, а не «через сервер»: сервер всё равно не знает
//     дороги к устройству, а человек выключил сеть нарочно.
//  2. Круглая кнопка выключена или идёт слив — напрямую, как без программы.
//  3. «Всегда через сервер» — сильнее правил ниже.
//  4. Локальная сеть — напрямую (сервер искал бы такой адрес у себя).
//  5. «Всегда напрямую» — по адресу, сети или имени.
//  6. Программа, которой по правилам положено мимо сервера, — напрямую.
//  7. Остальное — через сервер. Неопознанная программа тоже: неожиданная
//     утечка мимо туннеля хуже лишнего приложения внутри него.
func Decide(r Rules, c Conn) Route {
	named := c.named()
	if IsMeshTarget(c.Target) || (named != "" && IsMeshTarget(named)) {
		if r.Mesh {
			return RouteMesh
		}
		return RouteBlock
	}
	if !r.Bypass || r.Draining {
		return RouteDirect
	}
	if matches(r.Tunneled, c.Target, named) {
		return RouteServer
	}
	if !r.LocalViaTunnel && (routing.IsLocalTarget(c.Target) || (named != "" && routing.IsLocalTarget(named))) {
		return RouteDirect
	}
	if matches(r.Direct, c.Target, named) {
		return RouteDirect
	}
	if c.Process != "" && r.Policy != nil && !r.Policy.UseTunnel(c.Process) {
		return RouteDirect
	}
	return RouteServer
}

func matches(d *routing.DirectList, target, named string) bool {
	if d == nil {
		return false
	}
	return d.Match(target) || (named != "" && d.Match(named))
}

// IsMeshTarget — адрес или имя сети устройств (198.19.x.y, «имя.mesh»).
func IsMeshTarget(target string) bool { return mesh.IsTarget(target) }

// Hygiene — что сделать при смене режима, чтобы состояние после
// переключения совпало с состоянием после чистого запуска в новом режиме.
// Перезагрузка никогда не нужна.
type Hygiene struct {
	// ResetFakeIP — забыть таблицу подставных адресов: адреса, выданные в
	// старом режиме, не должны ни во что превращаться в новом.
	ResetFakeIP bool
	// CloseStackInternet — закрыть соединения из адаптера, кроме сети
	// устройств: маршруты интернета в адаптер сняты, приложение должно сразу
	// получить отказ и переподключиться по обычной сети, а не ждать тайм-аута.
	CloseStackInternet bool
	// CloseMesh — закрыть соединения в сеть устройств (её выключили).
	CloseMesh bool
	// Reevaluate — пересчитать решение для каждого живого соединения и
	// закрыть те, у которых оно изменилось: открытое «через сервер» в
	// «Обходе» не должно ходить через сервер после его выключения.
	Reevaluate bool
	// FlushDNS — сбросить DNS-кэш системы: ответы старого режима (подставные
	// адреса или, наоборот, настоящие) не должны пережить переключение.
	FlushDNS bool
}

// Any — есть что делать.
func (h Hygiene) Any() bool {
	return h.ResetFakeIP || h.CloseStackInternet || h.CloseMesh || h.Reevaluate || h.FlushDNS
}

// Transition — гигиена перехода из режима prev в next. Без смены режима —
// только пересчёт решений (поменялись правила, сменилась сеть).
func Transition(prev, next Modes) Hygiene {
	h := Hygiene{Reevaluate: true}
	if prev.Bypass != next.Bypass {
		h.ResetFakeIP = true
		h.FlushDNS = true
		// Выключили обход — маршрутов интернета в адаптер больше нет.
		h.CloseStackInternet = prev.Bypass
	}
	if prev.Mesh != next.Mesh {
		h.FlushDNS = true
		h.CloseMesh = prev.Mesh
	}
	return h
}
