package panel

// Профиль устройства в панели (ТЗ 16): одобрение новых устройств, заморозка,
// цепочка выхода, которую панель советует устройству, и сохранение простого
// режима доступа одним запросом. Сами правила — в meshd (одобрение,
// заморозка, цепочка) и в access.go (права).

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
)

func (s *Server) deviceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/mesh/net/approval", s.requireAuth(s.withMesh(s.handleMeshApproval)))
	mux.HandleFunc("/api/mesh/approve", s.requireAuth(s.withMesh(s.handleMeshApprove)))
	mux.HandleFunc("/api/mesh/freeze", s.requireAuth(s.withMesh(s.handleMeshFreeze)))
	mux.HandleFunc("/api/mesh/exit", s.requireAuth(s.withMesh(s.handleMeshExit)))
	mux.HandleFunc("/api/access/simple", s.requireAuth(s.handleAccessSimple))
}

// handleMeshApproval — {"net","on"}: «Новые устройства — после одобрения».
func (s *Server) handleMeshApproval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Net string `json:"net"`
		On  bool   `json:"on"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.mesh.SetApproval(r.Context(), req.Net, req.On), map[string]bool{"ok": true})
}

// handleMeshApprove — {"net","device"}: одобрить ждущее устройство.
// Отклонить — «Убрать» (/api/mesh/forget).
func (s *Server) handleMeshApprove(w http.ResponseWriter, r *http.Request) {
	var req meshDeviceReq
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.mesh.Approve(r.Context(), req.Net, req.Device), map[string]bool{"ok": true})
}

// handleMeshFreeze — {"net","device","on"}: заморозить или разморозить.
func (s *Server) handleMeshFreeze(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Net    string `json:"net"`
		Device string `json:"device"`
		On     bool   `json:"on"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.mesh.Freeze(r.Context(), req.Net, req.Device, req.On), map[string]bool{"ok": true})
}

// handleMeshExit — {"net","device","entry":"адрес:порт","via":[…]}: цепочка
// выхода для устройства (via пусто — без цепочки). Ответ — что ушло
// устройству, с временем: так страница сравнит с тем, что оно применило.
func (s *Server) handleMeshExit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Net    string   `json:"net"`
		Device string   `json:"device"`
		Entry  string   `json:"entry"`
		Via    []string `json:"via"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	o, err := s.mesh.SetExit(r.Context(), req.Net, req.Device, req.Entry, req.Via)
	replyErr(w, err, map[string]any{"ok": true, "exit": o})
}

// SimpleAccess — простой режим доступа одним запросом: настройки прав
// целиком и группы клиентов (id → все его группы). Клиенты, которых нет в
// Clients, остаются в своих группах.
type SimpleAccess struct {
	AccessSettings
	Clients map[string][]string `json:"clients"`
}

// accessMu — права сохраняются по одному: два сохранения сразу смешали бы
// группы одного с правилами другого.
var accessMu sync.Mutex

// handleAccessSimple сохраняет права и группы клиентов вместе: раньше
// страница слала несколько запросов, и закрытая на середине вкладка
// оставляла доступ уже, чем задумано. Всё проверяется до первой записи;
// не записалось что-то — уже записанное возвращается как было, и meshd
// получает политику один раз, в самом конце.
func (s *Server) handleAccessSimple(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "settings_unavailable"})
		return
	}
	var req SimpleAccess
	if !decodePost(w, r, &req) {
		return
	}
	accessMu.Lock()
	defer accessMu.Unlock()
	changes, err := planSimpleAccess(&req, s.clients.List())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var done []groupChange
	undo := func() {
		for _, c := range done {
			old := c.old
			s.clients.Update(c.id, func(cl *Client) { cl.Groups = old })
		}
	}
	for _, c := range changes {
		groups := c.groups
		if err := s.clients.Update(c.id, func(cl *Client) { cl.Groups = groups }); err != nil {
			undo()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		done = append(done, c)
	}
	if err := s.settings.Update(func(st *Settings) error { st.Access = req.AccessSettings; return nil }); err != nil {
		undo()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.respondPushed(w, r)
}

type groupChange struct {
	id          string
	old, groups []string
}

// planSimpleAccess проверяет запрос целиком и возвращает, кому какие группы
// поставить. Ничего не пишет.
func planSimpleAccess(req *SimpleAccess, clients []Client) ([]groupChange, error) {
	if err := req.AccessSettings.clean(); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, g := range req.Groups {
		have[g] = true
	}
	byID := map[string]Client{}
	for _, c := range clients {
		byID[c.ID] = c
	}
	var out []groupChange
	ids := make([]string, 0, len(req.Clients))
	for id := range req.Clients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c, ok := byID[id]
		if !ok {
			return nil, errors.New("нет такого клиента: " + id)
		}
		groups, err := cleanGroups(req.Clients[id])
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			if !have[g] {
				return nil, errors.New("нет такой группы: " + g)
			}
		}
		if strings.Join(groups, "\x00") != strings.Join(c.Groups, "\x00") {
			out = append(out, groupChange{id: id, old: append([]string(nil), c.Groups...), groups: groups})
		}
		delete(byID, id)
	}
	// Остальные клиенты остаются в своих группах — те не должны пропасть.
	for _, c := range byID {
		for _, g := range c.Groups {
			if !have[g] {
				return nil, errors.New("в группе «" + g + "» ещё есть устройства (" + c.Name + ") — сначала убери их из неё")
			}
		}
	}
	return out, nil
}
