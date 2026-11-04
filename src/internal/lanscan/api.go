package lanscan

import (
	"encoding/json"
	"net/http"

	"sshtunnel/internal/addons"
)

// ID — приложение в каталоге.
const ID = "lanscan"

func init() {
	addons.Register(ID, func() addons.Module { return module{} })
	addons.RegisterRoutes(ID, addons.Route{
		Path: "/api/lanscan", Methods: "GET, POST",
		Desc: "Сканер сети: GET — как идёт и кто найден (ip, mac, name, vendor, rttMs, ports из 22/80/443), POST {\"net\":\"подсеть\"} — сканировать заново (net из nets; пусто — лучшая). Ничего не запоминается.",
		H:    handle,
	}, addons.Route{
		Path: "/api/lanscan/wifi", Methods: "GET",
		Desc: "Сканер сети → Wi-Fi: сети вокруг (ssid, bssid, level дБм, freq, channel, sec, radio) и своя сеть (link). ?now=1 — только своя сеть, быстро. Телефон отдаёт это сам.",
		H:    handleWifi,
	})
}

// module — пока приложение добавлено; убрали — результат забывается.
type module struct{}

func (module) Start() error { return nil }
func (module) Stop()        { scanner.Stop() }

func handle(w http.ResponseWriter, r *http.Request) {
	var res Result
	if r.Method == http.MethodPost {
		var req struct {
			Net string `json:"net"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res = scanner.Start(req.Net)
	} else {
		res = scanner.Get()
	}
	writeJSON(w, res)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
