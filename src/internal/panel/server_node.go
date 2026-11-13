package panel

import (
	"errors"
	"net/http"

	"sshtunnel/internal/nodeapi"
)

// WithNode подключает хост плагинов сервера.
func (s *Server) WithNode(m *NodeManager) *Server {
	s.node = m
	return s
}

func (s *Server) nodeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/node/state", s.requireAuth(s.withNode(s.handleNodeState)))
	mux.HandleFunc("/api/node/install", s.requireAuth(s.withNode(s.handleNodeInstall)))
	mux.HandleFunc("/api/node/remove", s.requireAuth(s.withNode(s.handleNodeRemove)))
	mux.HandleFunc("/api/node/config", s.requireAuth(s.withNode(s.handleNodeConfig)))
	mux.HandleFunc("/api/node/folder/add", s.requireAuth(s.withNode(s.handleNodeFolderAdd)))
	mux.HandleFunc("/api/node/folder/remove", s.requireAuth(s.withNode(s.handleNodeFolderRemove)))
	mux.HandleFunc("/api/node/folder/quota", s.requireAuth(s.withNode(s.handleNodeFolderQuota)))
	mux.HandleFunc("/api/node/clipboard/clear", s.requireAuth(s.withNode(s.handleNodeClipboardClear)))
	mux.HandleFunc("/api/node/backup/delete", s.requireAuth(s.withNode(s.handleNodeBackupDelete)))
	mux.HandleFunc("/api/node/net/delete", s.requireAuth(s.withNode(s.handleNodeNetDelete)))
	mux.HandleFunc("/api/node/fix", s.requireAuth(s.withNode(s.handleNodeFix)))
}

func (s *Server) withNode(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.node == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "node_unavailable"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleNodeState(w http.ResponseWriter, r *http.Request) {
	st := s.node.Status(r.Context())
	st.Warnings = s.nodeWarnings(r.Context(), st.State)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleNodeBackupDelete(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.BackupReq
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.node.DeleteBackup(r.Context(), req), map[string]bool{"ok": true})
}

// handleNodeNetDelete — стереть данные плагинов сети, которой больше нет в
// meshd. Сеть ещё есть (или проверить нельзя) — отказ: данные живой сети
// так не стираются.
func (s *Server) handleNodeNetDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Net string `json:"net"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	has, ok := s.meshHasNet(r.Context(), req.Net)
	switch {
	case !ok:
		replyErr(w, errors.New("не удалось спросить meshd, есть ли эта сеть, — данные не тронуты"), nil)
		return
	case has:
		replyErr(w, errors.New("эта сеть ещё есть в сети устройств — сначала удали её в «Сетях»"), nil)
		return
	}
	replyErr(w, s.node.DeleteNet(r.Context(), req.Net), map[string]bool{"ok": true})
}

// handleNodeFix — безопасное исправление того, что мешает плагинам.
func (s *Server) handleNodeFix(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.fixNode(r.Context(), req.Code), map[string]bool{"ok": true})
}

func (s *Server) handleNodeInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	replyErr(w, s.node.Install(), map[string]bool{"ok": true})
}

func (s *Server) handleNodeRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeleteData bool `json:"deleteData"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.node.Remove(req.DeleteData), map[string]bool{"ok": true})
}

func (s *Server) handleNodeConfig(w http.ResponseWriter, r *http.Request) {
	var p nodeapi.ConfigPatch
	if !decodePost(w, r, &p) {
		return
	}
	st, err := s.node.SetConfig(r.Context(), p)
	replyErr(w, err, st)
}

func (s *Server) handleNodeFolderAdd(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.FolderReq
	if !decodePost(w, r, &req) {
		return
	}
	f, err := s.node.AddFolder(r.Context(), req)
	replyErr(w, err, f)
}

func (s *Server) handleNodeFolderRemove(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.FolderReq
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.node.RemoveFolder(r.Context(), req), map[string]bool{"ok": true})
}

func (s *Server) handleNodeFolderQuota(w http.ResponseWriter, r *http.Request) {
	var req nodeapi.FolderReq
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.node.SetFolderQuota(r.Context(), req), map[string]bool{"ok": true})
}

func (s *Server) handleNodeClipboardClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Net string `json:"net"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	replyErr(w, s.node.ClearClipboard(r.Context(), req.Net), map[string]bool{"ok": true})
}
