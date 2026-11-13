package panel

// Доступ к зеркальным папкам сервера по устройствам (ТЗ 18): как на
// устройстве-владельце — новое устройство сети не видит папку, пока её не
// отметили для него. Устройства и их id панель берёт у meshd; хост плагинов
// хранит только списки (PeerID — тот же id, что видят устройства).

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"sshtunnel/internal/nodeapi"
)

// NodeDevice — устройство сети в списке «кому видна папка».
type NodeDevice struct {
	ID     string `json:"id"` // nodeapi.PeerID
	Name   string `json:"name"`
	Host   string `json:"host,omitempty"`
	User   string `json:"user,omitempty"`
	Online bool   `json:"online"`
	// Gone — устройства уже нет в meshd, а папки у него остались.
	Gone    bool `json:"gone,omitempty"`
	Folders int  `json:"folders"`
}

// nodeAccess — устройства каждой сети (по id сети) для галочек: все
// устройства meshd, по имени; плюс те, кому папки даны, а в meshd их уже
// нет.
func nodeAccess(nets []MeshNetwork, st *nodeapi.State) map[string][]NodeDevice {
	out := map[string][]NodeDevice{}
	if st == nil {
		return out
	}
	byNet := map[string]MeshNetwork{}
	for _, n := range nets {
		byNet[n.ID] = n
	}
	for _, nn := range st.Networks {
		if len(nn.Folders) == 0 {
			continue
		}
		granted := map[string]int{}
		for _, f := range nn.Folders {
			for _, d := range f.Devices {
				granted[d]++
			}
		}
		list := []NodeDevice{}
		seen := map[string]bool{}
		for _, d := range byNet[nn.ID].Devices {
			id := nodeapi.PeerID(d.ID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			name := d.Display
			if name == "" {
				name = d.Name
			}
			if name == "" {
				name = d.Host
			}
			list = append(list, NodeDevice{ID: id, Name: name, Host: d.Host, User: d.User, Online: d.Online, Folders: granted[id]})
		}
		for id, n := range granted {
			if !seen[id] {
				list = append(list, NodeDevice{ID: id, Name: id, Gone: true, Folders: n})
			}
		}
		sort.SliceStable(list, func(i, j int) bool {
			if a, b := strings.ToLower(list[i].Name), strings.ToLower(list[j].Name); a != b {
				return a < b
			}
			return list[i].ID < list[j].ID
		})
		out[nn.ID] = list
	}
	return out
}

// migrateTargets — сети, где есть папки с прошлой версии, с устройствами
// meshd: правило обновления (их получают все известные сейчас устройства).
func migrateTargets(nets []MeshNetwork, st *nodeapi.State) []nodeapi.MigrateReq {
	if st == nil {
		return nil
	}
	byNet := map[string]MeshNetwork{}
	for _, n := range nets {
		byNet[n.ID] = n
	}
	var out []nodeapi.MigrateReq
	for _, nn := range st.Networks {
		old := false
		for _, f := range nn.Folders {
			old = old || f.Old
		}
		mn, ok := byNet[nn.ID]
		if !old || !ok || len(mn.Devices) == 0 {
			continue
		}
		req := nodeapi.MigrateReq{Net: nn.ID}
		for _, d := range mn.Devices {
			if id := nodeapi.PeerID(d.ID); id != "" {
				req.Devices = append(req.Devices, nodeapi.KnownDevice{ID: id, Name: d.Display})
			}
		}
		out = append(out, req)
	}
	return out
}

// meshNets — сети meshd, если он ответил (иначе nil: не знаем устройств).
func (s *Server) meshNets(ctx context.Context) ([]MeshNetwork, bool) {
	if s.mesh == nil {
		return nil, false
	}
	ms := s.mesh.State(ctx)
	if ms.EffectiveRole != MeshRoleMain || ms.AdminErr != "" {
		return nil, false
	}
	return ms.Networks, true
}

// nodeStatusWithAccess — состояние хоста с устройствами для галочек; по
// дороге применяет правило обновления к папкам прошлой версии.
func (s *Server) nodeStatusWithAccess(ctx context.Context) NodeStatus {
	st := s.node.Status(ctx)
	nets, ok := s.meshNets(ctx)
	if !ok || st.State == nil {
		return st
	}
	if reqs := migrateTargets(nets, st.State); len(reqs) > 0 {
		for _, req := range reqs {
			_ = s.node.MigrateAccess(ctx, req)
		}
		st = s.node.Status(ctx)
		if st.State == nil {
			return st
		}
	}
	st.Access = nodeAccess(nets, st.State)
	return st
}

// handleNodeFolderAccess — POST /api/node/folder/access AccessReq: галочка
// устройства у папки (folder "*" — у всех папок сети). Сохраняется сразу.
func (s *Server) handleNodeFolderAccess(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.AccessReq
	if !decodePost(w, r, &req) {
		return
	}
	if req.Name == "" {
		if nets, ok := s.meshNets(r.Context()); ok {
			for _, n := range nets {
				for _, d := range n.Devices {
					if n.ID == req.Net && nodeapi.PeerID(d.ID) == req.Device {
						req.Name = d.Display
					}
				}
			}
		}
	}
	replyErr(w, s.node.SetFolderAccess(r.Context(), req), map[string]bool{"ok": true})
}
