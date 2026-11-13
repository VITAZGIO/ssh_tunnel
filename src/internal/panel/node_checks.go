package panel

// Что мешает плагинам сервера работать (ТЗ 13): панель сама сверяет
// настройки сети устройств и права доступа с портами включённых плагинов и
// говорит об этом на вкладке «Плагины». Где исправление безопасно —
// одна кнопка (POST /api/node/fix).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"sshtunnel/internal/nodeapi"
)

// Коды предупреждений (тексты — в таблицах страницы).
const (
	// WarnServerOff — «Сервер в сети устройств» выключен: устройства не
	// дозваниваются до сервера вовсе. Исправление: включить только для
	// портов плагинов.
	WarnServerOff = "server_access_off"
	// WarnPortsBlocked — список портов сервера не пускает на порты плагинов.
	// Исправление: дописать их (плагины сами опознают каждого).
	WarnPortsBlocked = "ports_blocked"
	// WarnAccessRules — права доступа включены, а у части пользователей нет
	// правила к @servers на эти порты. Сами не правим: это решение о правах.
	WarnAccessRules = "access_rules"
	// WarnSecondary — сервер побочный: meshd здесь — пересылка, опознание
	// устройств не работает, плагины никого не пустят.
	WarnSecondary = "secondary"
	// WarnNoMesh — сети устройств на этом сервере нет.
	WarnNoMesh = "no_mesh"
)

// NodeWarning — что мешает плагинам.
type NodeWarning struct {
	Code  string   `json:"code"`
	Ports string   `json:"ports,omitempty"` // какие порты плагинов закрыты
	Users []string `json:"users,omitempty"` // у кого нет прав на эти порты
	Fix   bool     `json:"fix,omitempty"`   // есть безопасное исправление
}

// pluginPorts — порты включённых плагинов.
func pluginPorts(c nodeapi.Config) []int {
	var out []int
	if c.Files {
		out = append(out, nodeapi.PortFiles)
	}
	if c.Mirror {
		out = append(out, nodeapi.PortMirror)
	}
	if c.Clipboard {
		out = append(out, nodeapi.PortClipboard)
	}
	if c.Backup {
		out = append(out, nodeapi.PortBackup)
	}
	return out
}

// portIn — порт в списке «22,80,8000-8100» (пусто — все), как в meshd.
func portIn(spec string, port int) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return true
	}
	for _, part := range strings.Split(spec, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		b, err2 := a, error(nil)
		if isRange {
			b, err2 = strconv.Atoi(strings.TrimSpace(hi))
		}
		if err1 == nil && err2 == nil && port >= a && port <= b {
			return true
		}
	}
	return false
}

func portList(ps []int) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

// pluginWarnings — что мешает плагинам. role — действующая роль сервера в
// сети устройств, access/ports — «Сервер в сети устройств».
func pluginWarnings(role string, access bool, ports string, a AccessSettings, clients []Client, cfg nodeapi.Config) []NodeWarning {
	out := []NodeWarning{}
	switch role {
	case MeshRoleSecondary:
		return append(out, NodeWarning{Code: WarnSecondary})
	case MeshRoleMain:
	default:
		return append(out, NodeWarning{Code: WarnNoMesh})
	}
	want := pluginPorts(cfg)
	if len(want) == 0 {
		return out
	}
	if !access {
		out = append(out, NodeWarning{Code: WarnServerOff, Ports: portList(want), Fix: true})
	} else {
		var blocked []int
		for _, p := range want {
			if !portIn(ports, p) {
				blocked = append(blocked, p)
			}
		}
		if len(blocked) > 0 {
			out = append(out, NodeWarning{Code: WarnPortsBlocked, Ports: portList(blocked), Fix: true})
		}
	}
	if a.Enabled {
		var users []string
		for _, c := range clients {
			if c.Username == "" || c.State != StateActive {
				continue
			}
			member := func(g string) bool {
				if g == GroupAll {
					return true
				}
				for _, x := range c.Groups {
					if x == g {
						return true
					}
				}
				return false
			}
			ok := true
			for _, p := range want {
				covered := false
				for _, r := range a.Rules {
					if member(r.From) && (r.To == GroupAll || r.To == GroupServers) && (r.Ports == "" || portIn(r.Ports, p)) {
						covered = true
						break
					}
				}
				if !covered {
					ok = false
					break
				}
			}
			if !ok {
				name := c.Name
				if name == "" {
					name = c.Username
				}
				users = append(users, name)
			}
		}
		if len(users) > 0 {
			sort.Strings(users)
			out = append(out, NodeWarning{Code: WarnAccessRules, Ports: portList(want), Users: users})
		}
	}
	return out
}

// fixServerPorts — «Сервер в сети устройств» после исправления: включён,
// порты плагинов в списке. Пустой список при выключенном доступе не
// превращается в «все порты» — только порты плагинов.
func fixServerPorts(access bool, ports string, cfg nodeapi.Config) string {
	want := pluginPorts(cfg)
	ports = strings.TrimSpace(ports)
	if access && ports == "" {
		return ""
	}
	var add []int
	for _, p := range want {
		if ports == "" || !portIn(ports, p) {
			add = append(add, p)
		}
	}
	if len(add) == 0 {
		return ports
	}
	if ports == "" {
		return portList(add)
	}
	return ports + "," + portList(add)
}

// nodeWarnings — предупреждения для живого сервера.
func (s *Server) nodeWarnings(ctx context.Context, st *nodeapi.State) []NodeWarning {
	if st == nil || s.mesh == nil || s.settings == nil {
		return nil
	}
	ms := s.mesh.settings.Get().Mesh
	role := ms.Role
	if role == MeshRoleOff && s.mesh.sys.UnitState(meshdUnit).Active {
		role = MeshRoleMain
	}
	return pluginWarnings(role, !ms.ServerAccessOff, ms.ServerPorts, s.settings.Get().Access, s.clients.List(), st.Config)
}

// fixNode — безопасное исправление по коду предупреждения.
func (s *Server) fixNode(ctx context.Context, code string) error {
	if s.mesh == nil {
		return errors.New("сети устройств на этом сервере нет")
	}
	st, err := s.node.api.State(ctx)
	if err != nil {
		return err
	}
	ms := s.mesh.settings.Get().Mesh
	switch code {
	case WarnServerOff, WarnPortsBlocked:
		return s.mesh.SetServerAccess(ctx, true, fixServerPorts(!ms.ServerAccessOff, ms.ServerPorts, st.Config))
	default:
		return fmt.Errorf("для «%s» исправления одной кнопкой нет", code)
	}
}

// meshHasNet — есть ли сеть в meshd. ok=false — проверить не удалось.
func (s *Server) meshHasNet(ctx context.Context, id string) (has, ok bool) {
	if s.mesh == nil {
		return false, true
	}
	ms := s.mesh.State(ctx)
	if ms.EffectiveRole != MeshRoleMain || ms.AdminErr != "" {
		return false, false
	}
	for _, n := range ms.Networks {
		if n.ID == id {
			return true, true
		}
	}
	return false, true
}
