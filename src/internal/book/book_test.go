package book

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCopyMatchesDocs — встроенная копия совпадает с docs/book: Книгу правят
// там, а сюда копирует go generate.
func TestCopyMatchesDocs(t *testing.T) {
	for _, lang := range []string{"ru", "en"} {
		docs := filepath.Join("..", "..", "..", "docs", "book", lang)
		ents, err := os.ReadDir(docs)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			want[e.Name()] = true
			a, _ := os.ReadFile(filepath.Join(docs, e.Name()))
			b, err := text.ReadFile("text/" + lang + "/" + e.Name())
			norm := func(x []byte) []byte { return bytes.ReplaceAll(x, []byte("\r\n"), []byte("\n")) }
			if err != nil || !bytes.Equal(norm(a), norm(b)) {
				t.Errorf("%s/%s разошёлся с docs/book — запусти go generate ./internal/book", lang, e.Name())
			}
		}
		got, _ := text.ReadDir("text/" + lang)
		for _, e := range got {
			if !want[e.Name()] {
				t.Errorf("%s/%s есть во встроенной копии, но нет в docs/book", lang, e.Name())
			}
		}
	}
}

// TestChapters — обе локали, одинаковые главы, «Частые проблемы» в конце,
// главы приложений перед ними.
func TestChapters(t *testing.T) {
	ids := func(cs []Chapter) string {
		var s []string
		for _, c := range cs {
			s = append(s, c.ID)
		}
		return strings.Join(s, ",")
	}
	ru := Chapters("ru", []string{"app-sample", "app-missing"})
	en := Chapters("en", []string{"app-sample"})
	if got, want := ids(ru), "start,main,mesh,commands,own-apps,app-sample,problems"; got != want {
		t.Fatalf("главы ru: %s, нужно %s", got, want)
	}
	if ids(en) != ids(ru) {
		t.Fatalf("главы en (%s) не совпадают с ru (%s)", ids(en), ids(ru))
	}
	if ru[0].Title != "С чего начать" || en[0].Title != "Getting started" {
		t.Fatalf("заголовки: %q, %q", ru[0].Title, en[0].Title)
	}
	if got := ids(Chapters("de", nil)); got != "start,main,mesh,commands,own-apps,problems" {
		t.Fatalf("неизвестный язык → русский без глав приложений: %s", got)
	}
}
