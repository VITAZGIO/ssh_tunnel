// Package docker — плагин «Docker» (Linux, VPN-версия): человек выбирает для
// каждого контейнера, как он ходит в интернет: «как сам компьютер», «через
// сервер» или «напрямую».
//
// Здесь — список контейнеров (сокет Docker API, без программы docker), выбор
// человека (переживает пересоздание контейнера) и два чистых расчёта:
// Resolve — что действует для каждого контейнера, KernelPlan — какие правила и
// маршруты ядра Linux для этого нужны. Исполняет план ядра слой VPN
// (vpn/vpnlayer/docker_linux.go) через интерфейс Kernel; сам пакет к ядру не
// ходит, поэтому тесты не требуют ни root, ни Docker.
package docker

import (
	"net/netip"
	"sort"
	"strings"
)

// Choice — как контейнер ходит в интернет.
type Choice string

const (
	// ChoiceHost — «как сам компьютер»: правил для контейнера нет вовсе.
	ChoiceHost Choice = "host"
	// ChoiceTunnel — «через сервер»: только в адаптер VPN; нет туннеля —
	// нет интернета, без запасного пути мимо.
	ChoiceTunnel Choice = "tunnel"
	// ChoiceDirect — «напрямую»: мимо адаптера VPN, через настоящую сеть.
	ChoiceDirect Choice = "direct"
)

// ParseChoice понимает и слова, которыми пишут метку и команду.
func ParseChoice(s string) (Choice, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "host", "default", "auto", "":
		return ChoiceHost, true
	case "tunnel", "server", "vpn":
		return ChoiceTunnel, true
	case "direct":
		return ChoiceDirect, true
	}
	return "", false
}

// Label — метка контейнера, которая сильнее выбора в программе
// (`ssh_tunnel.route=tunnel|direct|host`).
const Label = "ssh_tunnel.route"

// Метки docker compose.
const (
	labelProject = "com.docker.compose.project"
	labelService = "com.docker.compose.service"
)

// Container — контейнер глазами программы.
type Container struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	State   string            `json:"state"` // running, exited, created…
	Project string            `json:"project,omitempty"`
	Service string            `json:"service,omitempty"`
	Labels  map[string]string `json:"-"`
	// NetMode — режим сети: "bridge" (обычная сеть Docker, в том числе
	// своя), "host", "none" или "container" (сеть другого контейнера).
	NetMode string `json:"netMode"`
	// Parent — чью сеть делит контейнер (NetMode "container"): ID или имя.
	Parent   string    `json:"parent,omitempty"`
	Networks []Network `json:"networks"`
}

// Network — сеть Docker, в которой стоит контейнер, и его адреса в ней.
type Network struct {
	Name string     `json:"name"`
	IP   netip.Addr `json:"ip,omitempty"`
	IP6  netip.Addr `json:"ip6,omitempty"`
}

// Running — контейнер работает (только у работающих есть адреса).
func (c Container) Running() bool { return c.State == "running" }

// Key — под каким ключом хранится выбор контейнера: у контейнеров compose —
// проект и служба (пересоздание меняет ID, а иногда и имя), у остальных —
// имя.
func (c Container) Key() string {
	if c.Project != "" && c.Service != "" {
		return "service:" + c.Project + "/" + c.Service
	}
	return "name:" + c.Name
}

// Choices — выбор человека: ключ контейнера (Container.Key) или
// "project:<имя>" → выбор.
type Choices map[string]Choice

// Row — контейнер и что для него действует.
type Row struct {
	Container
	// Choice — выбор человека для этого контейнера (без метки и проекта).
	Choice Choice `json:"choice"`
	// LabelChoice — что велит метка; пусто — метки нет.
	LabelChoice Choice `json:"label,omitempty"`
	// Effective — что действует на самом деле.
	Effective Choice `json:"effective"`
	// From — откуда взялось Effective: label | container | project | default
	// | parent (сеть другого контейнера) | fixed (не отделить).
	From string `json:"from"`
	// Switchable — можно ли выбрать для контейнера отдельно.
	Switchable bool `json:"switchable"`
	// Why — почему нельзя (по-русски), если Switchable == false.
	Why string `json:"why,omitempty"`
	// WhyEn — то же по-английски.
	WhyEn string `json:"whyEn,omitempty"`
}

// Resolve — что действует для каждого контейнера. Порядок: метка
// `ssh_tunnel.route` → выбор для самого контейнера (служба compose или имя)
// → выбор для проекта compose → «как сам компьютер». Контейнер в сети
// хоста не отделить; контейнер в сети другого контейнера идёт так же, как
// тот (у них одни адреса).
func Resolve(cs []Container, ch Choices) []Row {
	rows := make([]Row, len(cs))
	byKey := map[string]int{}
	for i, c := range cs {
		byKey[c.ID] = i
		if c.Name != "" {
			byKey[c.Name] = i
		}
		r := Row{Container: c, Choice: ChoiceHost, Effective: ChoiceHost, From: "default", Switchable: true}
		if v, ok := ch[c.Key()]; ok {
			r.Choice = v
		}
		if l, ok := c.Labels[Label]; ok {
			if v, ok := ParseChoice(l); ok {
				r.LabelChoice = v
			}
		}
		switch c.NetMode {
		case "host":
			r.Switchable, r.From = false, "fixed"
			r.Why = "сеть самого компьютера (network_mode: host) — идёт как компьютер"
			r.WhyEn = "uses the host network (network_mode: host) — goes like the computer"
		case "none":
			r.Switchable, r.From = false, "fixed"
			r.Why = "без сети (network_mode: none)"
			r.WhyEn = "no network (network_mode: none)"
		case "container":
			r.Switchable, r.From = false, "parent"
			r.Why = "в сети другого контейнера — идёт так же, как он"
			r.WhyEn = "shares another container's network — goes the same way"
		default:
			switch {
			case r.LabelChoice != "":
				r.Effective, r.From = r.LabelChoice, "label"
			case ch[c.Key()] != "":
				r.Effective, r.From = ch[c.Key()], "container"
			case c.Project != "" && ch["project:"+c.Project] != "":
				r.Effective, r.From = ch["project:"+c.Project], "project"
			}
		}
		rows[i] = r
	}
	// Сеть другого контейнера: идёт как тот (цепочка — до первого
	// «настоящего»; петли Docker не допускает, но и здесь не зациклимся).
	for i := range rows {
		if rows[i].NetMode != "container" {
			continue
		}
		j, seen := i, map[int]bool{}
		for rows[j].NetMode == "container" && !seen[j] {
			seen[j] = true
			k, ok := byKey[rows[j].Parent]
			if !ok {
				break
			}
			j = k
		}
		if j != i && rows[j].NetMode != "container" {
			rows[i].Effective = rows[j].Effective
			if rows[j].Name != "" {
				rows[i].Why = "в сети контейнера «" + rows[j].Name + "» — идёт так же, как он"
				rows[i].WhyEn = "shares the network of \"" + rows[j].Name + "\" — goes the same way"
			}
		}
	}
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].Project != rows[b].Project {
			return rows[a].Project < rows[b].Project
		}
		return rows[a].Name < rows[b].Name
	})
	return rows
}

// Targets — адреса работающих контейнеров, которым нужны правила ядра. У
// «как сам компьютер» правил нет; у контейнеров в чужой сети — тоже: их
// пакеты идут с адреса того контейнера, и правила того контейнера их уже
// покрывают.
func Targets(rows []Row) []Target {
	var out []Target
	seen := map[netip.Addr]bool{}
	for _, r := range rows {
		if !r.Running() || r.NetMode == "host" || r.NetMode == "none" || r.NetMode == "container" {
			continue
		}
		if r.Effective != ChoiceTunnel && r.Effective != ChoiceDirect {
			continue
		}
		for _, n := range r.Networks {
			for _, a := range []netip.Addr{n.IP, n.IP6} {
				if a.IsValid() && !a.IsUnspecified() && !seen[a] {
					seen[a] = true
					out = append(out, Target{Addr: a, Route: r.Effective})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr.Less(out[j].Addr) })
	return out
}

// Project — проект compose в списке: сколько контейнеров и что выбрано
// для него целиком.
type Project struct {
	Name   string `json:"name"`
	Choice Choice `json:"choice"`
	Count  int    `json:"count"`
}

// Projects — проекты compose из списка контейнеров.
func Projects(rows []Row, ch Choices) []Project {
	idx := map[string]int{}
	var out []Project
	for _, r := range rows {
		if r.Project == "" {
			continue
		}
		i, ok := idx[r.Project]
		if !ok {
			c := ch["project:"+r.Project]
			if c == "" {
				c = ChoiceHost
			}
			idx[r.Project] = len(out)
			out = append(out, Project{Name: r.Project, Choice: c})
			i = len(out) - 1
		}
		out[i].Count++
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
