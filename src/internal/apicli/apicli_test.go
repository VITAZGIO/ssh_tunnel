package apicli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAPI — API ядра в миниатюре: две кнопки, правила, галочка MCP.
type fakeAPI struct {
	mu     sync.Mutex
	tunnel bool
	mesh   bool
	mcp    bool
	direct []string
	posts  []string // тела POST /api/state
}

func (f *fakeAPI) server(t *testing.T) *Client {
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"routes": []Route{
			{Path: "/api/state", Methods: "GET, POST", Desc: "Состояние", Confirm: "выключение сети устройств"},
			{Path: "/api/rules", Methods: "GET, POST", Desc: "Правила"},
			{Path: "/api/diag/site", Methods: "GET, POST", Desc: "Проверить сайт"},
			{Path: "/events", Methods: "GET", Desc: "Поток"},
		}})
	})
	phase := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			f.posts = append(f.posts, string(b))
			var req struct{ Tunnel, Mesh *bool }
			json.Unmarshal(b, &req)
			if req.Tunnel != nil {
				f.tunnel = *req.Tunnel
			}
			if req.Mesh != nil {
				f.mesh = *req.Mesh
			}
		}
		js(w, map[string]any{"tunnel": map[string]any{"phase": phase(f.tunnel)},
			"mesh": map[string]any{"phase": phase(f.mesh)}, "meshAvailable": true, "mode": "bypass"})
	})
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		js(w, map[string]any{"mcpEnabled": f.mcp})
	})
	mux.HandleFunc("/api/rules", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == "POST" {
			var req struct{ Op, List, Value string }
			json.NewDecoder(r.Body).Decode(&req)
			if req.Op == "add" && req.List == "direct" {
				f.direct = append(f.direct, req.Value)
			}
		}
		js(w, map[string]any{"directHosts": f.direct})
	})
	mux.HandleFunc("/api/diag/site", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Write(b) // эхо: видно, что пришло
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, Token: "t", HTTP: srv.Client()}
}

func run(c *Client, args ...string) (int, string, error) {
	var out bytes.Buffer
	r := &runner{c: c, out: &out}
	code, err := r.run(args)
	return code, out.String(), err
}

// TestTunnelOffKeepsMesh — `tunnel off` выключает туннель, сеть устройств
// не трогает (приёмка ТЗ 8).
func TestTunnelOffKeepsMesh(t *testing.T) {
	f := &fakeAPI{tunnel: true, mesh: true}
	c := f.server(t)
	code, out, err := run(c, "tunnel", "off")
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if len(f.posts) != 1 || strings.TrimSpace(f.posts[0]) != `{"tunnel":false}` {
		t.Fatalf("ушло %v", f.posts)
	}
	if f.tunnel || !f.mesh {
		t.Fatalf("tunnel=%v mesh=%v", f.tunnel, f.mesh)
	}
	if !strings.Contains(out, "Туннель:") || !strings.Contains(out, "выключен") {
		t.Fatalf("вывод: %s", out)
	}
}

// TestGenericCommand — команда по самоописанию: слова → путь, ключ=значение → JSON.
func TestGenericCommand(t *testing.T) {
	c := (&fakeAPI{}).server(t)
	_, out, err := run(c, "diag", "site", "site=youtube.com")
	if err != nil || !strings.Contains(out, "youtube.com") {
		t.Fatalf("%v %s", err, out)
	}
	if code, _, _ := run(c, "nosuch"); code != exitUsage {
		t.Fatalf("неизвестная команда: %d", code)
	}
	if code, _, _ := run(c, "events"); code != exitUsage {
		t.Fatal("поток событий как команда")
	}
}

// mcpSession — запросы MCP по строкам и ответы.
func mcpSession(t *testing.T, c *Client, reqs ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := ServeMCP(c, strings.NewReader(strings.Join(reqs, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		res = append(res, m)
	}
	return res
}

// TestMCP — с галочкой нейросеть включает «Обход» и добавляет правило; без
// галочки вызовы отказывают. Опасное помечено.
func TestMCP(t *testing.T) {
	f := &fakeAPI{}
	c := f.server(t)
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`
	notif := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	on := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"set_tunnel","arguments":{"on":true}}}`
	rule := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"add_rule","arguments":{"list":"direct","value":"example.com"}}}`

	// Галочка выключена.
	res := mcpSession(t, c, init, notif, on)
	if len(res) != 2 {
		t.Fatalf("ответов %d (на уведомление не отвечают)", len(res))
	}
	r := res[1]["result"].(map[string]any)
	if r["isError"] != true || !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), "выключено") {
		t.Fatalf("без галочки: %v", r)
	}
	if f.tunnel {
		t.Fatal("без галочки туннель включился")
	}

	// Включена.
	f.mcp = true
	res = mcpSession(t, c, init, list, on, rule)
	tools := res[1]["result"].(map[string]any)["tools"].([]any)
	names := map[string]map[string]any{}
	for _, x := range tools {
		m := x.(map[string]any)
		names[m["name"].(string)] = m
	}
	for _, n := range []string{"get_state", "set_tunnel", "set_mesh", "add_rule", "api_state", "api_diag_site"} {
		if names[n] == nil {
			t.Errorf("нет инструмента %s", n)
		}
	}
	if names["api_events"] != nil {
		t.Error("поток событий стал инструментом")
	}
	if !strings.Contains(names["api_state"]["description"].(string), "ОПАСНО") ||
		!strings.Contains(names["set_mesh"]["description"].(string), "ОПАСНО") {
		t.Error("опасное не помечено")
	}
	if strings.Contains(names["set_tunnel"]["description"].(string), "ОПАСНО") {
		t.Error("включение туннеля помечено опасным")
	}
	for _, i := range []int{2, 3} {
		if res[i]["result"].(map[string]any)["isError"] != false {
			t.Fatalf("вызов %d: %v", i, res[i])
		}
	}
	if !f.tunnel || len(f.direct) != 1 || f.direct[0] != "example.com" {
		t.Fatalf("tunnel=%v direct=%v", f.tunnel, f.direct)
	}
}
