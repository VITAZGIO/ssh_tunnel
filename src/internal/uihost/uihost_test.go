package uihost

import (
	"encoding/json"
	"net/http"
	"testing"
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
