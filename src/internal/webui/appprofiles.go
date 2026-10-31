package webui

import (
	"encoding/json"
	"net/http"

	"sshtunnel/internal/appprofiles"
	"sshtunnel/internal/config"
)

// Профили приложения (см. appprofiles): полоска «Профиль 1, 2, … +» вверху
// настроек. Хранятся рядом с config.json той стороны, что держит туннель
// (на Windows — служба VPN, окно лишь показывает её страницу).
var profilesStore = appprofiles.New(config.Dir())

func (s *Server) handleAppProfiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"state": profilesStore.Load()})
}

func (s *Server) handleAppProfilesAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		On bool   `json:"on"`
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	var (
		st   appprofiles.State
		next *config.Config
		err  error
	)
	cur := s.app.Config()
	switch r.URL.Path {
	case "/api/appprofiles/enable":
		st, err = profilesStore.SetEnabled(req.On)
	case "/api/appprofiles/add":
		var c config.Config
		if st, c, err = profilesStore.Add(cur); err == nil {
			next = &c
		}
	case "/api/appprofiles/switch":
		var c config.Config
		if st, c, err = profilesStore.Switch(req.ID, cur); err == nil {
			next = &c
		}
	case "/api/appprofiles/delete":
		st, next, err = profilesStore.Delete(req.ID, cur)
	default:
		http.NotFound(w, r)
		return
	}
	if err == nil && next != nil {
		err = s.app.ReplaceConfig(*next)
	}
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error(), "state": st})
		return
	}
	writeJSON(w, map[string]any{"state": st})
}
