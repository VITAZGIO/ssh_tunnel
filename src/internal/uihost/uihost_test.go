package uihost

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHost — страницы открыты, API только с ключом, Книга отдаёт главы.
func TestHost(t *testing.T) {
	h, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	get := func(path, tok string) *http.Response {
		req, _ := http.NewRequest("GET", h.URL()+path, nil)
		if tok != "" {
			req.Header.Set("X-Token", tok)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, p := range []string{"/apps.html", "/book.html", "/theme.css", "/pages.js", "/icons/star.svg"} {
		if r := get(p, ""); r.StatusCode != 200 {
			t.Errorf("%s: %d", p, r.StatusCode)
		}
	}
	if r := get("/api/addons", ""); r.StatusCode != http.StatusForbidden {
		t.Fatalf("API без ключа: %d", r.StatusCode)
	}
	if r := get("/addons/sample.html?t="+h.Token(), ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("страница не добавленного приложения: %d", r.StatusCode)
	}
	r := get("/api/book?lang=en", h.Token())
	var b struct {
		Chapters []struct{ ID string } `json:"chapters"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || len(b.Chapters) < 4 {
		t.Fatalf("Книга: %v, %d глав", err, len(b.Chapters))
	}
}

// TestPlatformAPI — чужие адреса /api/ уходят обработчику платформы, свои
// (Книга) остаются своими; события доходят до /events.
func TestPlatformAPI(t *testing.T) {
	h, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	call := func(path string) (int, string) {
		req, _ := http.NewRequest("POST", h.URL()+path, strings.NewReader(`{"x":1}`))
		req.Header.Set("X-Token", h.Token())
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	if code, _ := call("/api/status"); code != http.StatusServiceUnavailable {
		t.Fatalf("без платформы: %d", code)
	}
	h.SetAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		io.WriteString(w, r.URL.Path+" "+string(b))
	}))
	if code, body := call("/api/status"); code != 200 || body != `/api/status {"x":1}` {
		t.Fatalf("платформа: %d %q", code, body)
	}
	if _, body := call("/api/book?lang=ru"); strings.HasPrefix(body, "/api/book") {
		t.Fatal("Книга ушла платформе")
	}

	req, _ := http.NewRequest("GET", h.URL()+"/events?t="+h.Token(), nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	rd := bufio.NewReader(r.Body)
	if line, _ := rd.ReadString('\n'); !strings.HasPrefix(line, ":") {
		t.Fatalf("начало потока: %q", line)
	}
	go func() {
		for i := 0; i < 50; i++ {
			h.Emit([]byte(`{"kind":"log","text":"привет"}`))
			time.Sleep(20 * time.Millisecond)
		}
	}()
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			if !strings.Contains(line, "привет") {
				t.Fatalf("событие: %q", line)
			}
			return
		}
	}
}
