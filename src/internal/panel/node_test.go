package panel

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"sshtunnel/internal/nodeapi"
)

// fakeNodeSystem — MeshSystem для хоста плагинов: RunScript ставит нужный
// юнит, когда в скрипте видно его имя.
type fakeNodeSystem struct {
	mu      sync.Mutex
	units   map[string]UnitState
	scripts []string
	err     error
}

func newFakeNodeSystem() *fakeNodeSystem { return &fakeNodeSystem{units: map[string]UnitState{}} }

func (f *fakeNodeSystem) UnitState(unit string) UnitState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.units[unit]
}

func (f *fakeNodeSystem) RunScript(script string, onLine func(string)) error {
	f.mu.Lock()
	f.scripts = append(f.scripts, script)
	err := f.err
	if err == nil && strings.Contains(script, nodeUnit) {
		if strings.Contains(script, "disable --now") {
			delete(f.units, nodeUnit)
		} else {
			f.units[nodeUnit] = UnitState{Installed: true, Active: true, Enabled: true}
		}
	}
	f.mu.Unlock()
	onLine("готово")
	return err
}

func (f *fakeNodeSystem) InstallUnit(string, string) error {
	return errors.New("не используется")
}
func (f *fakeNodeSystem) DisableUnit(string) error     { return nil }
func (f *fakeNodeSystem) UnitLog(string, int) []string { return nil }
func (f *fakeNodeSystem) EnsureLinkUser() error        { return nil }
func (f *fakeNodeSystem) WriteLinkKeys([]string) error { return nil }
func (f *fakeNodeSystem) KillLinkSessions() error      { return nil }
func (f *fakeNodeSystem) EnsureLinkSSHD() error        { return nil }
func (f *fakeNodeSystem) HostKeys() []string           { return nil }
func (f *fakeNodeSystem) LocalAddrs() []string         { return nil }

// fakeNodeAPI — подделка nodeapi.API: без настоящего хоста.
type fakeNodeAPI struct {
	mu      sync.Mutex
	cfg     nodeapi.Config
	folders map[string][]nodeapi.Folder
	cleared []string
	down    bool
	// ТЗ 18: что просили сделать с доступом к папкам.
	access   []nodeapi.AccessReq
	migrated []nodeapi.MigrateReq
}

func newFakeNodeAPI() *fakeNodeAPI {
	return &fakeNodeAPI{cfg: nodeapi.Config{QuotaMB: nodeapi.DefaultQuotaMB}, folders: map[string][]nodeapi.Folder{}}
}

func (a *fakeNodeAPI) State(context.Context) (nodeapi.State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.down {
		return nodeapi.State{}, nodeapi.ErrNotRunning
	}
	st := nodeapi.State{Config: a.cfg, Meshd: "ok"}
	for id, fs := range a.folders {
		st.Networks = append(st.Networks, nodeapi.Network{ID: id, Folders: fs})
	}
	return st, nil
}

func (a *fakeNodeAPI) SetConfig(_ context.Context, p nodeapi.ConfigPatch) (nodeapi.State, error) {
	a.mu.Lock()
	if p.Mirror != nil {
		a.cfg.Mirror = *p.Mirror
	}
	if p.QuotaMB != nil {
		a.cfg.QuotaMB = *p.QuotaMB
	}
	a.mu.Unlock()
	return a.State(context.Background())
}

func (a *fakeNodeAPI) AddFolder(_ context.Context, r nodeapi.FolderReq) (nodeapi.Folder, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := nodeapi.Folder{ID: "f1", Name: r.Name, QuotaMB: r.QuotaMB}
	a.folders[r.Net] = append(a.folders[r.Net], f)
	return f, nil
}

func (a *fakeNodeAPI) RemoveFolder(_ context.Context, r nodeapi.FolderReq) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	list := a.folders[r.Net]
	out := list[:0]
	for _, f := range list {
		if f.ID != r.ID {
			out = append(out, f)
		}
	}
	a.folders[r.Net] = out
	return nil
}

func (a *fakeNodeAPI) SetFolderQuota(context.Context, nodeapi.FolderReq) error { return nil }

func (a *fakeNodeAPI) ClearClipboard(_ context.Context, netID string) error {
	a.mu.Lock()
	a.cleared = append(a.cleared, netID)
	a.mu.Unlock()
	return nil
}

func (a *fakeNodeAPI) DeleteBackup(context.Context, nodeapi.BackupReq) error { return nil }

func (a *fakeNodeAPI) SetFolderAccess(_ context.Context, r nodeapi.AccessReq) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.access = append(a.access, r)
	for i, f := range a.folders[r.Net] {
		if r.Folder != "*" && f.ID != r.Folder {
			continue
		}
		devs := []string{}
		for _, d := range f.Devices {
			if d != r.Device {
				devs = append(devs, d)
			}
		}
		if r.On {
			devs = append(devs, r.Device)
		}
		a.folders[r.Net][i].Devices, a.folders[r.Net][i].Old = devs, false
	}
	return nil
}

func (a *fakeNodeAPI) MigrateAccess(_ context.Context, r nodeapi.MigrateReq) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.migrated = append(a.migrated, r)
	for i, f := range a.folders[r.Net] {
		if f.Old {
			for _, d := range r.Devices {
				a.folders[r.Net][i].Devices = append(a.folders[r.Net][i].Devices, d.ID)
			}
			a.folders[r.Net][i].Old = false
		}
	}
	return nil
}

func (a *fakeNodeAPI) DeleteNet(_ context.Context, netID string) error {
	a.mu.Lock()
	delete(a.folders, netID)
	a.mu.Unlock()
	return nil
}

func TestNodeНедоступенБезМенеджера(t *testing.T) {
	s, _ := newTestServer(t)
	id, _ := s.sessions.create("admin", false)
	cookies := []*http.Cookie{{Name: cookieName, Value: id}}
	rec, out := doJSON(t, s.Handler(), http.MethodGet, "/api/node/state", nil, cookies)
	if rec.Code != http.StatusNotFound || out["error"] != "node_unavailable" {
		t.Fatalf("%d %v", rec.Code, out)
	}
}

func TestNodeУстановкаИСостояние(t *testing.T) {
	s, _ := newTestServer(t)
	sys := newFakeNodeSystem()
	api := newFakeNodeAPI()
	s.WithNode(NewNodeManager(sys, api, "v1.9.0"))
	id, _ := s.sessions.create("admin", false)
	cookies := []*http.Cookie{{Name: cookieName, Value: id}}
	h := s.Handler()

	rec, out := doJSON(t, h, http.MethodGet, "/api/node/state", nil, cookies)
	if rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if out["unit"].(map[string]any)["installed"] != false {
		t.Fatalf("уже установлен: %v", out)
	}

	rec, out = doJSON(t, h, http.MethodPost, "/api/node/install", map[string]any{}, cookies)
	if rec.Code != 200 || out["ok"] != true {
		t.Fatalf("install: %d %v", rec.Code, out)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !sys.UnitState(nodeUnit).Installed {
		if time.Now().After(deadline) {
			t.Fatal("не установился")
		}
		time.Sleep(10 * time.Millisecond)
	}

	rec, out = doJSON(t, h, http.MethodGet, "/api/node/state", nil, cookies)
	if rec.Code != 200 || out["unit"].(map[string]any)["installed"] != true {
		t.Fatalf("после установки: %d %v", rec.Code, out)
	}
	if out["state"] == nil {
		t.Fatalf("нет живого состояния: %v", out)
	}
}

func TestNodeВключитьПлагинИДобавитьПапку(t *testing.T) {
	s, _ := newTestServer(t)
	sys := newFakeNodeSystem()
	sys.units[nodeUnit] = UnitState{Installed: true, Active: true}
	api := newFakeNodeAPI()
	s.WithNode(NewNodeManager(sys, api, "v1.9.0"))
	id, _ := s.sessions.create("admin", false)
	cookies := []*http.Cookie{{Name: cookieName, Value: id}}
	h := s.Handler()

	on := true
	rec, out := doJSON(t, h, http.MethodPost, "/api/node/config", nodeapi.ConfigPatch{Mirror: &on}, cookies)
	if rec.Code != 200 || out["config"].(map[string]any)["mirror"] != true {
		t.Fatalf("config: %d %v", rec.Code, out)
	}

	netID := strings.Repeat("a", 64)
	rec, out = doJSON(t, h, http.MethodPost, "/api/node/folder/add",
		nodeapi.FolderReq{Net: netID, Name: "Общая", QuotaMB: 500}, cookies)
	if rec.Code != 200 || out["name"] != "Общая" {
		t.Fatalf("add folder: %d %v", rec.Code, out)
	}
	fid, _ := out["id"].(string)

	rec, out = doJSON(t, h, http.MethodPost, "/api/node/folder/remove",
		nodeapi.FolderReq{Net: netID, ID: fid}, cookies)
	if rec.Code != 200 || out["ok"] != true {
		t.Fatalf("remove folder: %d %v", rec.Code, out)
	}

	rec, out = doJSON(t, h, http.MethodPost, "/api/node/clipboard/clear", map[string]string{"net": netID}, cookies)
	if rec.Code != 200 || out["ok"] != true {
		t.Fatalf("clear: %d %v", rec.Code, out)
	}
	if len(api.cleared) != 1 || api.cleared[0] != netID {
		t.Fatalf("очистка не прошла: %v", api.cleared)
	}
}

func TestNodeОшибкаХостаВидна(t *testing.T) {
	s, _ := newTestServer(t)
	sys := newFakeNodeSystem()
	sys.units[nodeUnit] = UnitState{Installed: true, Active: false}
	api := newFakeNodeAPI()
	api.down = true
	s.WithNode(NewNodeManager(sys, api, "v1.9.0"))
	id, _ := s.sessions.create("admin", false)
	cookies := []*http.Cookie{{Name: cookieName, Value: id}}

	rec, out := doJSON(t, s.Handler(), http.MethodGet, "/api/node/state", nil, cookies)
	if rec.Code != 200 || out["error"] == nil || out["error"] == "" {
		t.Fatalf("ожидали ошибку хоста: %d %v", rec.Code, out)
	}
}
