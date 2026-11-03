package webui

import (
	"encoding/json"
	"net/http"
	"strings"

	"sshtunnel/internal/config"
	"sshtunnel/internal/routing"
)

// Правила активного сервера отдельным адресом: что идёт напрямую, что всегда
// через сервер и какие программы ведутся через туннель. Это те же поля, что
// в /api/config, — но без ключей и остальных настроек: адрес нужен командам,
// MCP и своим приложениям с разрешением «менять правила» (им незачем видеть
// всё). Решает «куда» по-прежнему только ядро (core.Decide).

type rulesResp struct {
	Profile     string   `json:"profile"`
	FilterMode  string   `json:"filterMode"`
	FilterApps  []string `json:"filterApps"`
	DirectHosts []string `json:"directHosts"`
	TunnelHosts []string `json:"tunnelHosts"`
}

func rulesOf(p config.Profile) rulesResp {
	r := rulesResp{Profile: p.ID, FilterMode: p.FilterMode, FilterApps: p.FilterApps,
		DirectHosts: p.DirectHosts, TunnelHosts: p.TunnelHosts}
	for _, l := range []*[]string{&r.FilterApps, &r.DirectHosts, &r.TunnelHosts} {
		if *l == nil {
			*l = []string{}
		}
	}
	return r
}

// handleRules — GET: правила активного сервера. POST:
//
//	{"op":"add"|"remove","list":"direct"|"tunnel"|"apps","value":"youtube.com"}
//	{"filterMode":"all"|"only"|"except"}
//
// Правила применяются сразу, как при сохранении настроек.
func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	cfg := s.app.Config()
	p := cfg.Active()
	if r.Method != http.MethodPost {
		writeJSON(w, rulesOf(p))
		return
	}
	var req struct {
		Op         string `json:"op"`
		List       string `json:"list"`
		Value      string `json:"value"`
		FilterMode string `json:"filterMode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "нужен JSON {\"op\":\"add|remove\",\"list\":\"direct|tunnel|apps\",\"value\"} или {\"filterMode\"}"})
		return
	}
	if req.FilterMode != "" {
		switch req.FilterMode {
		case "all", "only", "except":
			p.FilterMode = req.FilterMode
		default:
			writeJSON(w, map[string]string{"error": "filterMode: all, only или except"})
			return
		}
	}
	if req.Op != "" {
		var l *[]string
		switch req.List {
		case "direct":
			l = &p.DirectHosts
		case "tunnel":
			l = &p.TunnelHosts
		case "apps":
			l = &p.FilterApps
		default:
			writeJSON(w, map[string]string{"error": "list: direct, tunnel или apps"})
			return
		}
		vals := routing.SplitEntries(req.Value)
		if req.List == "apps" {
			// Путь к программе может быть с пробелами — одна запись целиком.
			vals = nil
			if v := strings.TrimSpace(req.Value); v != "" {
				vals = []string{v}
			}
		}
		if len(vals) == 0 {
			writeJSON(w, map[string]string{"error": "пустое value"})
			return
		}
		switch req.Op {
		case "add":
			for _, v := range vals {
				if !containsFold(*l, v) {
					*l = append(*l, v)
				}
			}
		case "remove":
			out := (*l)[:0:0]
			for _, e := range *l {
				if !containsFold(vals, e) {
					out = append(out, e)
				}
			}
			*l = out
		default:
			writeJSON(w, map[string]string{"error": "op: add или remove"})
			return
		}
	}
	cfg.SetProfile(p)
	note, err := s.app.SetConfig(cfg)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "rules": rulesOf(s.app.Config().Active()), "note": note})
}

func containsFold(l []string, v string) bool {
	for _, e := range l {
		if strings.EqualFold(e, v) {
			return true
		}
	}
	return false
}
