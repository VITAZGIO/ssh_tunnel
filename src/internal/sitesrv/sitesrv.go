// Package sitesrv — «Проверить сайт» с сервера сети (ТЗ 15), сторона
// устройства: страница плагина просит здесь, этот пакет — сервер сети с
// включённым плагином (internal/node, порт ServerPort). Сервер проходит те
// же шаги DNS → TCP → TLS → HTTP из своей сети (diag/probe) и отвечает
// тем же словарём; страница показывает это второй колонкой «с сервера».
package sitesrv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/devnet"
	"sshtunnel/internal/diag/probe"
	"sshtunnel/internal/mesh"
)

// ServerPort — порт плагина на сервере (nodeapi.PortSitecheck).
const ServerPort = 47846

func init() {
	addons.RegisterRoutes("sitecheck", addons.Route{
		Path: "/api/sitecheck/server", Methods: "POST",
		Desc: "Проверить сайт с сервера сети: {\"site\",\"server\"?,\"lang\"?} → {\"server\":имя,\"result\":{server:{steps…},verdict,text}}. Сервер должен быть с плагином «Проверить сайт»; не чаще 5 проверок подряд.",
		H:    handle,
	})
}

func meshServers() []mesh.Peer {
	var out []mesh.Peer
	for _, p := range devnet.Status().Peers {
		if !p.Self && p.Online && p.Platform == "server" && p.IP.IsValid() {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// Reply — ответ страницы.
type Reply struct {
	Server string             `json:"server"`
	Result probe.ServerResult `json:"result"`
}

// Check — проверить site с сервера want (имя или адрес; "" — первый с
// плагином).
func Check(ctx context.Context, site, lang, want string) (Reply, error) {
	list := meshServers()
	if len(list) == 0 {
		return Reply{}, errors.New("серверов сети нет на связи")
	}
	body, _ := json.Marshal(map[string]string{"site": site, "lang": lang})
	hc := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext:       func(_ context.Context, _, addr string) (net.Conn, error) { return devnet.DialPeer(addr) },
	}}
	var lastErr error
	for _, p := range list {
		name := p.Name
		if name == "" {
			name = p.Host
		}
		if want != "" && want != p.Host && want != name && want != p.IP.String() {
			continue
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			"http://"+net.JoinHostPort(p.IP.String(), strconv.Itoa(ServerPort))+"/check", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			if s := err.Error(); strings.Contains(s, "не слушает") || strings.Contains(s, "refused") {
				err = errors.New("на сервере «" + name + "» не включён плагин «Проверить сайт»")
			}
			lastErr = err
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			var e struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(data, &e)
			if e.Error == "" {
				e.Error = resp.Status
			}
			// Сервер ответил по делу (часто, неверный адрес) — дальше не ищем.
			return Reply{Server: name}, errors.New(e.Error)
		}
		var r probe.ServerResult
		if err := json.Unmarshal(data, &r); err != nil {
			return Reply{Server: name}, err
		}
		return Reply{Server: name, Result: r}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("такого сервера сейчас нет на связи")
	}
	return Reply{}, lastErr
}

func handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		Site   string `json:"site"`
		Server string `json:"server"`
		Lang   string `json:"lang"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "POST {\"site\"}"})
		return
	}
	lang := "ru"
	if req.Lang == "en" || r.URL.Query().Get("lang") == "en" {
		lang = "en"
	}
	rep, err := Check(r.Context(), req.Site, lang, req.Server)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "server": rep.Server})
		return
	}
	_ = json.NewEncoder(w).Encode(rep)
}
