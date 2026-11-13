package panel

// HTTP-ручки выхода сервера через другой сервер (ТЗ 10) — карточка в
// «Настройках». WithEgress подключается в cmd/ssh_tunnel_panel/main.go,
// как WithMesh и WithSettings.

import "net/http"

// WithEgress подключает выход сервера через другой сервер.
func (s *Server) WithEgress(m *EgressManager) *Server {
	s.egress = m
	return s
}

func (s *Server) egressRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/egress", s.requireAuth(s.withEgress(s.handleEgressStatus)))
	mux.HandleFunc("/api/egress/apply", s.requireAuth(s.withEgress(s.handleEgressApply)))
	mux.HandleFunc("/api/egress/confirm", s.requireAuth(s.withEgress(s.handleEgressConfirm)))
	mux.HandleFunc("/api/egress/off", s.requireAuth(s.withEgress(s.handleEgressOff)))
	mux.HandleFunc("/api/egress/mode", s.requireAuth(s.withEgress(s.handleEgressMode)))
}

func (s *Server) withEgress(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.egress == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "egress_unavailable"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleEgressStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.egress.Status())
}

func (s *Server) handleEgressApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Link string `json:"link"`
		Mode string `json:"mode"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.egress.Apply(req.Link, req.Mode), map[string]bool{"ok": true})
}

func (s *Server) handleEgressConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.egress.Confirm(req.Token), map[string]bool{"ok": true})
}

func (s *Server) handleEgressOff(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.egress.Off(), map[string]bool{"ok": true})
}

func (s *Server) handleEgressMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.egress.SetMode(req.Mode), map[string]bool{"ok": true})
}
