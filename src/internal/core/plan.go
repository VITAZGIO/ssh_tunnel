package core

import (
	"net/netip"
	"strings"

	"sshtunnel/internal/mesh"
	"sshtunnel/internal/routing"
)

// Адреса и маршруты режима VPN. Решение «что ставить» — здесь, общее для
// Windows, Linux и Android; платформа только ставит.
var (
	// AdapterPrefix4/6 — адреса адаптера. 198.18.0.0/15 зарезервирован под
	// тесты сетевого оборудования: настоящих сайтов там нет, спутать не с
	// чем. В эту же подсеть попадают сеть устройств (198.19.0.0/16), наш
	// DNS и подставные адреса — поэтому они ходят в адаптер в любом режиме.
	AdapterPrefix4 = netip.MustParsePrefix("198.18.0.1/15")
	AdapterPrefix6 = netip.MustParsePrefix("fd00:c0de:7075::1/64")

	// DNSAddr — наш DNS. Отдельный адрес, не адрес самого адаптера: пакет
	// на собственный адрес система обработала бы сама и нам бы не отдала.
	DNSAddr = netip.MustParseAddr("198.18.0.53")

	// FakeNet — подставные адреса для имён (fake-IP).
	FakeNet = netip.MustParsePrefix("198.18.128.0/17")

	// InternetRoutes4/6 — весь интернет двумя половинками, а не одним
	// маршрутом по умолчанию: так наши маршруты всегда точнее маршрута по
	// умолчанию настоящей сети, и метрики сравнивать не приходится. Своя
	// локальная подсеть (роутер, принтер) остаётся ещё точнее и идёт мимо.
	InternetRoutes4 = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/1"),
		netip.MustParsePrefix("128.0.0.0/1"),
	}
	// IPv6 заворачивается всегда, даже если сервер его не умеет: иначе
	// соединения по шестой версии уходили бы мимо туннеля — утечка.
	InternetRoutes6 = []netip.Prefix{
		netip.MustParsePrefix("::/1"),
		netip.MustParsePrefix("8000::/1"),
	}
)

// MeshDomain — зона имён сети устройств (без точки в начале).
const MeshDomain = "mesh"

// VPNPlan — полный план режима для адаптера Windows и Linux. Платформа
// получает его целиком и приводит систему к нему; что было до этого, ей
// знать не нужно.
type VPNPlan struct {
	// Routes — маршруты в адаптер сверх его собственной подсети. Пусто —
	// в адаптер идёт только сеть устройств, наш DNS и подставные адреса.
	Routes []netip.Prefix
	// DNSAll — весь DNS системы через наш DNS, с запретом DNS мимо адаптера
	// (иначе провайдер видит запросы и подсовывает заглушки).
	DNSAll bool
	// DNSDomains — зоны, которые всегда спрашивать у нашего DNS, даже без
	// DNSAll (имена «.mesh»).
	DNSDomains []string
	// PinServer — маршрут к серверу через настоящую сеть: с включённой
	// точкой доступа Windows сверяет пакеты с таблицей маршрутов.
	PinServer bool
	// PinDirect — ставить маршрут через настоящую сеть к адресам, куда
	// соединение ведётся напрямую: по той же причине, что PinServer, привязки
	// сокета к карте Windows бывает мало.
	PinDirect bool
	// Share — раздавать VPN на мобильную точку доступа, если она включена.
	Share bool
}

// PlanOptions — что про систему знает платформа и что выбрано в настройках.
type PlanOptions struct {
	IPv6        bool // у адаптера есть IPv6
	ShareWanted bool // галочка «раздавать VPN на точку доступа»
}

// PlanVPN — план для режима m. Одна функция и для включения с нуля, и для
// переключения: состояние после переключения равно состоянию после чистого
// запуска в этом режиме.
func PlanVPN(m Modes, o PlanOptions) VPNPlan {
	var p VPNPlan
	if m.Mesh {
		p.DNSDomains = []string{MeshDomain}
	}
	if !m.Bypass {
		return p
	}
	p.Routes = append(p.Routes, InternetRoutes4...)
	if o.IPv6 {
		p.Routes = append(p.Routes, InternetRoutes6...)
	}
	p.DNSAll = true
	// Имена .mesh при DNSAll и так идут к нам; отдельное правило всё равно
	// ставим — оно переживает короткие моменты перестройки DNS адаптера.
	p.DNSDomains = []string{MeshDomain}
	p.PinServer = true
	p.PinDirect = true
	p.Share = o.ShareWanted
	return p
}

// DNSAnswer — чем отвечать на имя в нашем DNS.
type DNSAnswer int

const (
	DNSFake DNSAnswer = iota // подставным адресом: соединение пойдёт через сервер, имя разрешит он
	DNSReal                  // настоящим адресом из DNS настоящей сети: соединение пойдёт напрямую
	DNSMesh                  // адресом устройства из сети устройств
)

// DecideDNS — чем отвечать на DNS-запрос имени. Та же логика, что Decide,
// но про имя без порта и без программы: в Windows DNS спрашивает системная
// служба, а не программа, поэтому по DNS программу не узнать — правила по
// программам действуют уже на соединении (см. Decide и подставные адреса у
// исключённых программ в tunnel).
func DecideDNS(r Rules, name string) DNSAnswer {
	n := strings.TrimSuffix(strings.ToLower(name), ".")
	if strings.HasSuffix(n, mesh.Suffix) {
		return DNSMesh
	}
	if !r.Bypass || r.Draining {
		return DNSReal
	}
	if r.Tunneled != nil && r.Tunneled.Match(n) {
		return DNSFake
	}
	if !r.LocalViaTunnel && routing.IsLocalHost(n) {
		return DNSReal
	}
	if r.Direct != nil && r.Direct.Match(n) {
		return DNSReal
	}
	return DNSFake
}
