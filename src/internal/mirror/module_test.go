package mirror

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sshtunnel/internal/addons"
)

// Модуль через API страницы: приложение работает, только пока добавлено;
// раздать папку, исключения, версии, снять с раздачи (папка не тронута).
func TestModuleAPI(t *testing.T) {
	svc := addons.New(t.TempDir())
	defer svc.Close()
	routes := map[string]http.HandlerFunc{}
	for _, r := range svc.Routes() {
		routes[r.Path] = r.H
	}
	call := func(path, body string) map[string]any {
		t.Helper()
		method := "GET"
		if body != "" {
			method = "POST"
		}
		w := httptest.NewRecorder()
		routes[path](w, httptest.NewRequest(method, path, strings.NewReader(body)))
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		out["_code"] = w.Code
		return out
	}
	if r := call("/api/mirror", ""); r["_code"] != 403 {
		t.Fatalf("не добавленное приложение ответило: %v", r)
	}
	if err := svc.Store().Add("mirror"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	svc.Routes()[0].H(w, httptest.NewRequest("POST", "/api/addons", strings.NewReader(`{"op":"seen","id":"mirror"}`)))

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("a"), 0o644)
	body, _ := json.Marshal(map[string]string{"op": "serve", "path": dir, "name": "Заметки"})
	r := call("/api/mirror", string(body))
	if r["error"] != nil {
		t.Fatal(r["error"])
	}
	serve := r["serve"].([]any)
	if len(serve) != 1 || serve[0].(map[string]any)["files"].(float64) != 1 {
		t.Fatalf("раздача: %v", r)
	}
	id := "hub:" + serve[0].(map[string]any)["id"].(string)
	body, _ = json.Marshal(map[string]any{"op": "serve", "path": filepath.Join(dir, "x"), "name": "x"})
	os.Mkdir(filepath.Join(dir, "x"), 0o755)
	if r := call("/api/mirror", string(body)); r["error"] == nil {
		t.Fatal("вложенная папка принята")
	}
	body, _ = json.Marshal(map[string]any{"op": "ignore", "id": id, "list": []string{"*.log"}})
	if r := call("/api/mirror", string(body)); r["error"] != nil {
		t.Fatal(r["error"])
	}
	w = httptest.NewRecorder()
	routes["/api/mirror/versions"](w, httptest.NewRequest("GET", "/api/mirror/versions?id="+id, nil))
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("версии: %s", w.Body.String())
	}
	body, _ = json.Marshal(map[string]any{"op": "unserve", "id": id})
	if r := call("/api/mirror", string(body)); r["error"] != nil || len(r["serve"].([]any)) != 0 {
		t.Fatalf("снять с раздачи: %v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.md")); string(b) != "a" {
		t.Fatal("снятие с раздачи тронуло папку")
	}
}
