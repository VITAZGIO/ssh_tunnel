// bookcopy копирует Книгу из docs/book/<язык>/ в src/internal/book/text/,
// откуда её встраивает сборка. Запуск: cd src && go generate ./internal/book.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	// go generate запускает из папки пакета (src/internal/book).
	src := filepath.Join("..", "..", "..", "docs", "book")
	dst := "text"
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
