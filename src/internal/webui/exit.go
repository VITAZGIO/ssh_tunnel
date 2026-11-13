package webui

import (
	"encoding/json"
	"net/http"
	"strings"

	"sshtunnel/internal/config"
	"sshtunnel/internal/share"
)

// handleProfileExit — «Выход через» (цепочка выхода, ТЗ 8):
// {"id":"<сервер>","via":["<id>",…]} — по порядку, пусто — без цепочки.
func (s *Server) handleProfileExit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID  string   `json:"id"`
		Via []string `json:"via"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "не разобрал запрос: " + err.Error()})
		return
	}
	note, err := s.app.SetExitVia(req.ID, req.Via)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "note": note, "config": s.app.Config()})
}

// exportExitRefs — цепочка профиля ссылками для файла обмена. Сервер из
// цепочки уже удалён — цепочка не выгружается совсем: укороченная вывела бы
// интернет не там.
func exportExitRefs(cfg config.Config, p config.Profile) []share.ExitRef {
	var out []share.ExitRef
	for _, id := range p.ExitVia {
		h, ok := cfg.ProfileByID(id)
		if !ok || h.Host == "" {
			return nil
		}
		out = append(out, share.ExitRef{Name: h.Name, Host: h.Host, SSHPort: h.SSHPort, User: h.User})
	}
	return out
}

// resolveExitRefs находит серверы цепочки среди profiles (по адресу, порту
// и пользователю). Не нашёлся хоть один — цепочки нет, missing — чего не
// хватило (подписи для сообщения).
func resolveExitRefs(profiles []config.Profile, selfID string, refs []share.ExitRef) (ids, missing []string) {
	for _, r := range refs {
		port := r.SSHPort
		if port <= 0 {
			port = 22
		}
		found := ""
		for _, p := range profiles {
			if p.ID != selfID && strings.EqualFold(p.Host, r.Host) && p.SSHPort == port &&
				(r.User == "" || p.User == r.User) {
				found = p.ID
				break
			}
		}
		if found == "" {
			label := r.Name
			if label == "" {
				label = r.Host
			}
			missing = append(missing, label)
			continue
		}
		ids = append(ids, found)
	}
	if len(missing) > 0 {
		return nil, missing
	}
	return ids, nil
}

// exitImportNote — что сказать человеку, если цепочка не перенеслась.
func exitImportNote(missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	return "«Выход через» не перенесён: на этом устройстве нет сервера " + strings.Join(missing, ", ") +
		". Добавь его и задай цепочку в настройках сервера"
}
