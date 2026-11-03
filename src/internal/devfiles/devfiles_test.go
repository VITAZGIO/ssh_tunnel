package devfiles

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/mesh"
)

func setup(t *testing.T) (*server, Share, string) {
	t.Helper()
	data := t.TempDir()
	addons.New(data).Close()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644)
	os.Mkdir(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("b"), 0o644)
	sh, err := AddShare(root, "Фото")
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(func(string) {})
	t.Cleanup(s.close)
	return s, sh, root
}

func call(s *server, method, target string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), callerKey{}, mesh.Caller{IP: "198.19.0.5", Host: "pc"}))
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, r)
	return w
}

func TestListAndGet(t *testing.T) {
	s, sh, _ := setup(t)
	w := call(s, "GET", "/shares", nil)
	if !strings.Contains(w.Body.String(), sh.ID) || strings.Contains(w.Body.String(), "path") {
		t.Fatalf("shares: %s", w.Body)
	}
	w = call(s, "GET", "/list?s="+sh.ID+"&p=/", nil)
	var l struct{ Entries []Entry }
	json.Unmarshal(w.Body.Bytes(), &l)
	if len(l.Entries) != 2 || !l.Entries[0].Dir || l.Entries[1].Name != "a.txt" {
		t.Fatalf("list: %s", w.Body)
	}
	r := httptest.NewRequest("GET", "/get?s="+sh.ID+"&p=/a.txt", nil)
	r.Header.Set("Range", "bytes=1-")
	rw := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rw, r)
	if rw.Code != http.StatusPartialContent || rw.Body.String() != "ello" {
		t.Fatalf("range: %d %q", rw.Code, rw.Body)
	}
}

func TestEscape(t *testing.T) {
	s, sh, root := setup(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err == nil {
		if w := call(s, "GET", "/get?s="+sh.ID+"&p=/link/secret", nil); w.Code != http.StatusForbidden {
			t.Fatalf("ссылка наружу: %d", w.Code)
		}
	}
	// «..» отрезается: остаётся внутри папки.
	if w := call(s, "GET", "/get?s="+sh.ID+"&p=/../../a.txt", nil); w.Code != 200 || w.Body.String() != "hello" {
		t.Fatalf("..: %d %s", w.Code, w.Body)
	}
	if w := call(s, "GET", "/list?s=nope&p=/", nil); w.Code != http.StatusNotFound {
		t.Fatalf("чужая папка: %d", w.Code)
	}
}

func TestWriteNeedsPermission(t *testing.T) {
	s, sh, root := setup(t)
	op := func(body string) *httptest.ResponseRecorder { return call(s, "POST", "/op", []byte(body)) }
	if w := op(`{"op":"delete","s":"` + sh.ID + `","p":"/a.txt"}`); w.Code != http.StatusForbidden {
		t.Fatalf("без «можно менять» удалилось: %d", w.Code)
	}
	if w := call(s, "PUT", "/put?s="+sh.ID+"&p=/n.txt&off=0&done=1", []byte("x")); w.Code != http.StatusForbidden {
		t.Fatalf("без «можно менять» загрузилось: %d", w.Code)
	}
	SetWrite(sh.ID, true)
	if w := op(`{"op":"delete","s":"` + sh.ID + `","p":"/sub"}`); w.Code != http.StatusConflict {
		t.Fatalf("непустая папка удалилась с первого раза: %d", w.Code)
	}
	if w := op(`{"op":"delete","s":"` + sh.ID + `","p":"/sub","recursive":true}`); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := op(`{"op":"rename","s":"` + sh.ID + `","p":"/a.txt","to":"../x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("имя с ../: %d", w.Code)
	}
	if w := op(`{"op":"mkdir","s":"` + sh.ID + `","p":"/","to":"new"}`); w.Code != 200 {
		t.Fatalf("mkdir: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "new")); err != nil {
		t.Fatal(err)
	}
}

func TestUploadResume(t *testing.T) {
	s, sh, root := setup(t)
	SetWrite(sh.ID, true)
	base := "/put?s=" + sh.ID + "&p=/a.txt"
	if w := call(s, "PUT", base+"&off=0", []byte("abc")); w.Code != 200 {
		t.Fatalf("кусок 1: %d %s", w.Code, w.Body)
	}
	// Обрыв: страница спрашивает, сколько уже есть.
	w := call(s, "GET", "/part?s="+sh.ID+"&p=/a.txt", nil)
	if !strings.Contains(w.Body.String(), `"size":3`) {
		t.Fatalf("part: %s", w.Body)
	}
	if w := call(s, "PUT", base+"&off=1", []byte("zz")); w.Code != http.StatusConflict {
		t.Fatalf("кусок не с того места принят: %d", w.Code)
	}
	if w := call(s, "PUT", base+"&off=3&done=1", []byte("def")); w.Code != 200 || !strings.Contains(w.Body.String(), "a (2).txt") {
		t.Fatalf("кусок 2: %d %s", w.Code, w.Body)
	}
	got, _ := os.ReadFile(filepath.Join(root, "a (2).txt"))
	if string(got) != "abcdef" {
		t.Fatalf("собрано %q", got)
	}
	if old, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(old) != "hello" {
		t.Fatal("загрузка затёрла существующий файл")
	}
}
