package panel

// Режим показа (-demo) для профиля устройства в панели (ТЗ 16): одобрение,
// заморозка, цепочка выхода и простой доступ одним запросом. Отдельным
// файлом, чтобы не перемешиваться с остальным показом.

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"sshtunnel/internal/meshsvc"
)

// seedDevices — профили устройств, одно ждущее и одно замороженное.
func (d *demoBackend) seedDevices() {
	unix := time.Now().Unix()
	home, _ := d.findNet("9f3c2a71d04e8b65")
	dacha, _ := d.findNet("41be09c7aa13f2d0")
	if home == nil || dacha == nil {
		return
	}
	fra, lon, ams := "demo.example:22", "london.demo.example:22", "ams.demo.example:22"
	set := func(n *MeshNetwork, id string, fn func(x *MeshDevice)) {
		for i := range n.Devices {
			if n.Devices[i].ID == id {
				fn(&n.Devices[i])
			}
		}
	}
	// Рабочий ПК: цепочку задала эта панель, устройство её применило.
	set(home, "e1a04f", func(x *MeshDevice) {
		x.Profiles = []meshsvc.ProfileRef{
			{Key: fra, Name: "Франкфурт", Active: true, Via: []string{lon}, T: unix - 7200, By: "Франкфурт"},
			{Key: lon, Name: "Лондон"}, {Key: ams, Name: "Амстердам"},
		}
		x.ExitSet = &meshsvc.ExitOrder{Entry: fra, Via: []string{lon}, T: unix - 7200, By: "Франкфурт", ByID: "demo"}
	})
	// Ноутбук: панель задала одно, человек на устройстве потом поменял.
	set(home, "7c22d9", func(x *MeshDevice) {
		x.Profiles = []meshsvc.ProfileRef{
			{Key: fra, Name: "Франкфурт", Active: true, Via: []string{ams}, T: unix - 1800},
			{Key: ams, Name: "Амстердам"},
		}
		x.ExitSet = &meshsvc.ExitOrder{Entry: fra, Via: []string{}, T: unix - 86400, By: "Франкфурт", ByID: "demo"}
	})
	// Телефон: цепочку задала панель Лондона.
	set(home, "3b9e10", func(x *MeshDevice) {
		x.Profiles = []meshsvc.ProfileRef{
			{Key: lon, Name: "Лондон", Active: true, Via: []string{fra}, T: unix - 3*86400, By: "Лондон"},
			{Key: fra, Name: "Франкфурт"},
		}
	})
	set(dacha, "a9e002", func(x *MeshDevice) { x.Frozen = true })
	home.Approval = true
	waiting := home.Devices[0]
	waiting.Device = meshsvc.Device{ID: "f00d42", Name: "neznakomyy-noutbuk", Display: "neznakomyy-noutbuk",
		Host: "neznakomyy-noutbuk", IP: "198.19.0.9", User: demoUser(2), Online: true, Platform: "windows",
		OS: "Windows 10", App: "1.9.0", Mode: "proxy", FirstSeen: unix - 600, LastSeen: unix, ConnectedAt: unix - 600,
		Quality: "good", RTTMs: 12, RTTAvgMs: 13, Pending: true}
	waiting.Server = "main"
	home.Devices = append(home.Devices, waiting)
	for _, e := range []meshsvc.Event{
		{Type: "approval", Net: home.ID, Text: "on"},
		{Type: "new-device", Net: home.ID, Device: waiting.ID, Text: waiting.Name},
		{Type: "pending", Net: home.ID, Device: waiting.ID, Text: waiting.Name},
		{Type: "freeze", Net: dacha.ID, Device: "a9e002", Text: "dacha-kamera"},
	} {
		e.Time = unix - 300
		d.add(e)
	}
}

// heldReason — «pending»/«frozen», если одно из двух устройств ждёт или
// заморожено (как у meshd в /v1/check); "" — нет.
func (d *demoBackend) heldReason(netID string, ids ...string) string {
	for _, id := range ids {
		if x := d.device(netID, id); x != nil {
			if x.Frozen {
				return "frozen"
			}
			if x.Pending {
				return "pending"
			}
		}
	}
	return ""
}

// stripDeviceProfile — старый meshd не знает ни одобрения, ни цепочек.
func stripDeviceProfile(n *MeshNetwork) {
	n.Approval = false
	for i := range n.Devices {
		x := &n.Devices[i]
		x.Pending, x.Frozen, x.Profiles, x.ExitSet = false, false, nil, nil
	}
}

func demoKey(k string) bool {
	host, port, err := net.SplitHostPort(k)
	return err == nil && host != "" && port != "" && len(k) <= 80
}

func (d *demoBackend) deviceRoutes(h func(string, func(http.ResponseWriter, *http.Request))) {
	ok := map[string]bool{"ok": true}
	old := func(w http.ResponseWriter) bool {
		if d.oldMeshd {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": meshsvc.ErrUnsupported.Error()})
		}
		return d.oldMeshd
	}
	noDev := func(w http.ResponseWriter) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "нет такого устройства"})
	}
	h("/api/mesh/net/approval", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Net string `json:"net"`
			On  bool   `json:"on"`
		}
		if !decodePost(w, r, &req) || old(w) {
			return
		}
		n, _ := d.findNet(req.Net)
		if n == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "нет такой сети"})
			return
		}
		if n.Approval != req.On {
			n.Approval = req.On
			d.add(meshsvc.Event{Type: "approval", Net: n.ID, Text: map[bool]string{true: "on", false: "off"}[req.On]})
		}
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/mesh/approve", func(w http.ResponseWriter, r *http.Request) {
		var req meshDeviceReq
		if !decodePost(w, r, &req) || old(w) {
			return
		}
		x := d.device(req.Net, req.Device)
		if x == nil {
			noDev(w)
			return
		}
		if x.Pending {
			x.Pending = false
			d.add(meshsvc.Event{Type: "approve", Net: req.Net, Device: x.ID, Text: x.Display})
		}
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/mesh/freeze", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Net    string `json:"net"`
			Device string `json:"device"`
			On     bool   `json:"on"`
		}
		if !decodePost(w, r, &req) || old(w) {
			return
		}
		x := d.device(req.Net, req.Device)
		if x == nil {
			noDev(w)
			return
		}
		if x.Frozen != req.On {
			x.Frozen = req.On
			if req.On {
				x.Direct = nil // прямые соединения рвутся
			}
			d.add(meshsvc.Event{Type: map[bool]string{true: "freeze", false: "unfreeze"}[req.On], Net: req.Net, Device: x.ID, Text: x.Display})
		}
		writeJSON(w, http.StatusOK, ok)
	})
	h("/api/mesh/exit", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Net    string   `json:"net"`
			Device string   `json:"device"`
			Entry  string   `json:"entry"`
			Via    []string `json:"via"`
		}
		if !decodePost(w, r, &req) || old(w) {
			return
		}
		x := d.device(req.Net, req.Device)
		if x == nil {
			noDev(w)
			return
		}
		entry := strings.ToLower(strings.TrimSpace(req.Entry))
		err := error(nil)
		via := []string{}
		seen := map[string]bool{entry: true}
		for _, v := range req.Via {
			v = strings.ToLower(strings.TrimSpace(v))
			if !demoKey(v) || seen[v] {
				err = errors.New("неверный сервер в цепочке")
			}
			seen[v] = true
			via = append(via, v)
		}
		idx := -1
		for i, p := range x.Profiles {
			if p.Key == entry {
				idx = i
			}
		}
		switch {
		case !demoKey(entry):
			err = errors.New("неверный сервер входа: нужно «адрес:порт»")
		case len(via) > 2:
			err = errors.New("в цепочке может быть не больше двух серверов после входа")
		case len(x.Profiles) == 0:
			err = errors.New("устройство не рассказало о своих серверах — нужна новая версия программы на нём")
		case idx < 0:
			err = errors.New("у устройства нет профиля сервера " + entry)
		}
		if err != nil {
			replyErr(w, err, nil)
			return
		}
		t := time.Now().Unix()
		if p := x.Profiles[idx]; p.T >= t {
			t = p.T + 1
		}
		o := meshsvc.ExitOrder{Entry: entry, Via: via, T: t, By: d.servers[0].Name, ByID: "demo"}
		x.ExitSet = &o
		if x.Online {
			// Устройство на связи применяет сразу (в показе — без задержки).
			p := &x.Profiles[idx]
			p.Via, p.T, p.By = append([]string{}, via...), t, o.By
		}
		d.add(meshsvc.Event{Type: "exit", Net: req.Net, Device: x.ID, Text: strings.Join(append([]string{entry}, via...), " → ")})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "exit": o})
	})
	h("/api/access/simple", func(w http.ResponseWriter, r *http.Request) {
		var req SimpleAccess
		if !decodePost(w, r, &req) {
			return
		}
		changes, err := planSimpleAccess(&req, d.clients)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		for _, c := range changes {
			if cl := d.client(c.id); cl != nil {
				cl.Groups = c.groups
			}
		}
		d.access = req.AccessSettings
		writeJSON(w, http.StatusOK, ok)
	})
}
