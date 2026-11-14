// bookcopy копирует Книгу из docs/book/<язык>/ в src/internal/book/text/,
// откуда её встраивает сборка. Запуск: cd src && go generate ./internal/book.
// С флагом -panel — Книгу панели сервера: docs/book-panel/<язык>/ →
// src/internal/panel/booktext/ (go generate ./internal/panel).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	panel := flag.Bool("panel", false, "копировать Книгу панели (docs/book-panel → internal/panel/booktext)")
	flag.Parse()
	// go generate запускает из папки пакета (src/internal/book или
	// src/internal/panel).
	src := filepath.Join("..", "..", "..", "docs", "book")
	dst := "text"
	if *panel {
		src = filepath.Join("..", "..", "..", "docs", "book-panel")
		dst = "booktext"
	}
	for _, lang := range []string{"ru", "en"} {
		out := filepath.Join(dst, lang)
		if err := os.RemoveAll(out); err != nil {
			fail(err)
		}
		if err := os.MkdirAll(out, 0o755); err != nil {
			fail(err)
		}
		ents, err := os.ReadDir(filepath.Join(src, lang))
		if err != nil {
			fail(err)
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(src, lang, e.Name()))
			if err != nil {
				fail(err)
			}
			if err := os.WriteFile(filepath.Join(out, e.Name()), b, 0o644); err != nil {
				fail(err)
			}
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "bookcopy:", err)
	os.Exit(1)
}
