package docker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeKernel struct {
	mu sync.Mutex
	ts []Target
}

func (k *fakeKernel) SetTargets(ts []Target) { k.mu.Lock(); k.ts = ts; k.mu.Unlock() }
func (k *fakeKernel) Status() KernelStatus   { return KernelStatus{Up: true, Full: true} }
func (k *fakeKernel) get() []Target          { k.mu.Lock(); defer k.mu.Unlock(); return k.ts }

func TestServiceAPI(t *testing.T) {
	dockerAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/json":
			w.Write([]byte(`[{"Id":"aaa111","Names":["/web"],"Image":"nginx","State":"running","Labels":{},
			  "HostConfig":{"NetworkMode":"bridge"},"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}},
			 {"Id":"bbb222","Names":["/h"],"Image":"x","State":"running","Labels":{},
			  "HostConfig":{"NetworkMode":"host"},"NetworkSettings":{"Networks":{}}}]`))
		case "/events":
			<-r.Context().Done() // поток событий: молчит
		default:
			http.NotFound(w, r)
		}
	}))
	defer dockerAPI.Close()

	k := &fakeKernel{}
	SetKernel(k)
	defer SetKernel(nil)
	s := &Service{st: OpenStore(t.TempDir()), cli: newHTTPClient(dockerAPI.URL)}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	call := func(h http.HandlerFunc, body string) (int, map[string]any) {
		method := "GET"
		if body != "" {
			method = "POST"
		}
		req := httptest.NewRequest(method, "/api/docker", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h(rec, req)
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}

	if code, m := call(handleSet, `{"container":"web","route":"tunnel"}`); code != 200 {
		t.Fatalf("set: %d %v", code, m)
	}
	if ts := k.get(); len(ts) != 1 || ts[0].Addr.String() != "172.17.0.2" || ts[0].Route != ChoiceTunnel {
		t.Fatalf("в ядро ушло %v", ts)
	}
	if code, _ := call(handleSet, `{"container":"h","route":"tunnel"}`); code != 400 {
		t.Fatalf("сеть хоста не должна переключаться: %d", code)
	}
	if code, _ := call(handleSet, `{"container":"web","route":"куда-то"}`); code != 400 {
		t.Fatalf("плохой route: %d", code)
	}
	code, m := call(handleList, "")
	if code != 200 || m["available"] != true || m["vpn"] != true {
		t.Fatalf("list: %d %v", code, m)
	}
	cs := m["containers"].([]any)
	if len(cs) != 2 || cs[1].(map[string]any)["effective"] != "tunnel" {
		t.Fatalf("контейнеры: %v", cs)
	}

	// Выключили плагин — правил нет.
	s.Stop()
	if ts := k.get(); len(ts) != 0 {
		t.Fatalf("после Stop остались %v", ts)
	}
	s.cancel = nil // второй Stop из defer — без ожидания
}

func TestServiceNoDocker(t *testing.T) {
	s := &Service{st: OpenStore(t.TempDir()), cli: NewClient(t.TempDir() + "/нет.sock")}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	req := httptest.NewRequest("GET", "/api/docker", nil)
	rec := httptest.NewRecorder()
	handleList(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	if m["available"] != false || !strings.Contains(m["error"].(string), "Docker не найден") {
		t.Fatalf("ответ без Docker: %v", m)
	}
}
