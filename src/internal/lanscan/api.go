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
		Desc: "Сканер сети: GET — как идёт и кто найден (ip, mac, name, vendor, rttMs, ports из 22/80/443), POST {} — сканировать заново. Ничего не запоминается.",
		H:    handle,
	})
}

// module — пока приложение добавлено; убрали — результат забывается.
type module struct{}

func (module) Start() error { return nil }
func (module) Stop()        { scanner.Stop() }

func handle(w http.ResponseWriter, r *http.Request) {
	var res Result
	if r.Method == http.MethodPost {
		res = scanner.Start()
	} else {
		res = scanner.Get()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(res)
}
