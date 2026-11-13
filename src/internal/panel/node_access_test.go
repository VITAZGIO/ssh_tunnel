package panel

import (
	"net/http"
	"strings"
	"testing"

	"sshtunnel/internal/meshsvc"
	"sshtunnel/internal/nodeapi"
)

// ТЗ 18: устройства для галочек зеркальных папок сервера и правило
// обновления.
func TestNodeДоступУстройстваИПравилоОбновления(t *testing.T) {
	netA := strings.Repeat("a", 64)
	dev := func(id, name string, online bool) MeshDevice {
		return MeshDevice{Device: meshsvc.Device{ID: id, Display: name, Online: online}}
	}
	nets := []MeshNetwork{{ID: netA, Devices: []MeshDevice{dev("raw-tv", "ТВ", false), dev("raw-pc", "ПК", true)}}}
	st := &nodeapi.State{Networks: []nodeapi.Network{{ID: netA, Folders: []nodeapi.Folder{
		{ID: "f1", Devices: []string{nodeapi.PeerID("raw-pc"), "gone-id"}},
		{ID: "f2", Devices: []string{nodeapi.PeerID("raw-pc")}, Old: true},
	}}}}

	acc := nodeAccess(nets, st)[netA]
	if len(acc) != 3 || acc[0].Name != "gone-id" || !acc[0].Gone || acc[1].Name != "ПК" || acc[2].Name != "ТВ" {
		t.Fatalf("список: %+v", acc)
	}
	if acc[1].ID != nodeapi.PeerID("raw-pc") || acc[1].Folders != 2 || !acc[1].Online || acc[2].Folders != 0 {
		t.Fatalf("ПК/ТВ: %+v", acc)
	}

	reqs := migrateTargets(nets, st)
	if len(reqs) != 1 || reqs[0].Net != netA || len(reqs[0].Devices) != 2 {
		t.Fatalf("правило обновления: %+v", reqs)
	}
	// Без старых папок и без устройств в meshd — нечего делать.
	st.Networks[0].Folders[1].Old = false
	if len(migrateTargets(nets, st)) != 0 {
		t.Fatal("правило без старых папок")
	}
	st.Networks[0].Folders[1].Old = true
	if len(migrateTargets([]MeshNetwork{{ID: netA}}, st)) != 0 {
		t.Fatal("правило без устройств")
	}
}

// Галочки в демо панели: сохраняются сразу, «*» — у всех папок сети.
func TestNodeДоступВДемо(t *testing.T) {
	h := NewDemoHandler("test")
	demoCall(t, h, http.MethodPost, "/api/node/install", nil)
	_, st := demoCall(t, h, http.MethodGet, "/api/node/state", nil)
	nets := st["state"].(map[string]any)["networks"].([]any)
	netID := nets[0].(map[string]any)["id"].(string)
	for _, name := range []string{"А", "Б"} {
		if code, _ := demoCall(t, h, http.MethodPost, "/api/node/folder/add", map[string]any{"net": netID, "name": name}); code != 200 {
			t.Fatal("папка")
		}
	}
	_, st = demoCall(t, h, http.MethodGet, "/api/node/state", nil)
	devs, _ := st["access"].(map[string]any)[netID].([]any)
	if len(devs) == 0 {
		t.Fatalf("нет устройств для галочек: %v", st["access"])
	}
	id := devs[0].(map[string]any)["id"].(string)
	if code, _ := demoCall(t, h, http.MethodPost, "/api/node/folder/access", map[string]any{"net": netID, "folder": "*", "device": id, "on": true}); code != 200 {
		t.Fatal("галочка")
	}
	if code, _ := demoCall(t, h, http.MethodPost, "/api/node/folder/access", map[string]any{"net": netID, "folder": "demo2", "device": id, "on": false}); code != 200 {
		t.Fatal("снять галочку")
	}
	if code, _ := demoCall(t, h, http.MethodPost, "/api/node/folder/access", map[string]any{"net": netID, "folder": "нет", "device": id, "on": true}); code == 200 {
		t.Fatal("галочка у несуществующей папки")
	}
	_, st = demoCall(t, h, http.MethodGet, "/api/node/state", nil)
	for _, n := range st["state"].(map[string]any)["networks"].([]any) {
		nm := n.(map[string]any)
		if nm["id"] != netID {
			continue
		}
		fs := nm["folders"].([]any)
		d1 := fs[0].(map[string]any)["devices"].([]any)
		d2 := fs[1].(map[string]any)["devices"].([]any)
		if len(d1) != 1 || d1[0] != id || len(d2) != 0 {
			t.Fatalf("галочки: %v %v", d1, d2)
		}
	}
	for _, d := range st["access"].(map[string]any)[netID].([]any) {
		if dm := d.(map[string]any); dm["id"] == id && dm["folders"].(float64) != 1 {
			t.Fatalf("сколько папок у устройства: %v", dm)
		}
	}
}
