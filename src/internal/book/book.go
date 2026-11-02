// Package book — Книга, справка внутри программы. Текст пишется один раз в
// docs/book/<язык>/ и копируется сюда (go generate): go:embed не достаёт
// файлы вне модуля, а Книга должна работать без интернета. Тест сверяет копию
// с docs/book.
package book

//go:generate go run ../../tools/bookcopy

import (
	"embed"
	"encoding/json"
	"net/http"
	"path"
	"sort"
	"strings"
)

//go:embed text/ru/*.md text/en/*.md
var text embed.FS

// GitHub — ссылка внизу Книги.
const GitHub = "https://github.com/VITAZGIO/ssh_tunnel"

// Chapter — глава.
type Chapter struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	MD    string `json:"md"`
}

// lastID — глава, которая всегда в конце: главы приложений встают перед ней.
const lastID = "problems"

// Chapters — главы на языке lang (нет такого — русский) по порядку; главы
// приложений apps (их id — имена файлов app-<id>) встают перед «Частыми
// проблемами». Нет файла главы — её нет и в Книге.
func Chapters(lang string, apps []string) []Chapter {
	if lang != "en" {
		lang = "ru"
	}
	dir := "text/" + lang
	ents, _ := text.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var base, tail []Chapter
	for _, n := range names {
		if strings.HasPrefix(n, "app-") {
			continue
		}
		c := read(dir, n)
		if c.ID == lastID {
			tail = append(tail, c)
		} else {
			base = append(base, c)
		}
	}
	for _, id := range apps {
		n := id + ".md"
		if _, err := text.ReadFile(dir + "/" + n); err == nil {
			base = append(base, read(dir, n))
		}
	}
	return append(base, tail...)
}

// read читает главу; id — имя файла без номера и «.md».
func read(dir, name string) Chapter {
	b, _ := text.ReadFile(dir + "/" + name)
	md := strings.ReplaceAll(string(b), "\r\n", "\n")
	id := strings.TrimSuffix(name, path.Ext(name))
	if i := strings.IndexByte(id, '-'); i > 0 && !strings.HasPrefix(id, "app-") {
		id = id[i+1:]
	}
	title := id
	if line, _, _ := strings.Cut(md, "\n"); strings.HasPrefix(line, "# ") {
		title = strings.TrimSpace(line[2:])
	}
	return Chapter{ID: id, Title: title, MD: md}
}

// Handler — GET /api/book?lang=ru: все главы разом (их немного, а поиск идёт
// по всему тексту прямо на странице). apps — главы приложений каталога.
func Handler(apps func() []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := r.URL.Query().Get("lang")
		if lang != "en" {
			lang = "ru"
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"lang":     lang,
			"chapters": Chapters(lang, apps()),
			"github":   GitHub,
		})
	}
}
