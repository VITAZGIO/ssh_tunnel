package panel

// Книга панели сервера: главы пишутся один раз в docs/book-panel/<язык>/
// и копируются сюда (go generate): go:embed не достаёт файлы вне модуля, а
// Книга должна работать без интернета. Тест сверяет копию с docs/book-panel.
// Устройство Книги то же, что у Книги в программе (internal/book): те же
// файлы NN-имя.md, первая строка «# Заголовок», все главы отдаются разом, а
// поиск идёт прямо на странице. Отличие одно: id главы здесь — имя файла без
// «.md» целиком (с номером), потому что по нему «?» в панели находит главу.

//go:generate go run ../../tools/bookcopy -panel

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
)

//go:embed booktext/ru/*.md booktext/en/*.md
var bookText embed.FS

// BookChapter — глава Книги панели.
type BookChapter struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	MD    string `json:"md"`
}

// BookChapters — главы на языке lang (нет такого — русский) в порядке имён
// файлов.
func BookChapters(lang string) []BookChapter {
	if lang != "en" {
		lang = "ru"
	}
	return chaptersFrom(bookText, "booktext/"+lang)
}

// chaptersFrom читает главы из папки dir файловой системы fsys.
func chaptersFrom(fsys fs.FS, dir string) []BookChapter {
	ents, _ := fs.ReadDir(fsys, dir)
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]BookChapter, 0, len(names))
	for _, n := range names {
		b, _ := fs.ReadFile(fsys, dir+"/"+n)
		md := strings.ReplaceAll(string(b), "\r\n", "\n")
		id := strings.TrimSuffix(n, path.Ext(n))
		title := id
		if line, _, _ := strings.Cut(md, "\n"); strings.HasPrefix(line, "# ") {
			if t := strings.TrimSpace(line[2:]); t != "" {
				title = t
			}
		}
		out = append(out, BookChapter{ID: id, Title: title, MD: md})
	}
	return out
}

// handleBook — GET /api/book?lang=ru|en: {"lang","chapters":[{id,title,md}]}.
// Незнакомый язык — русский. Так отвечает и боевая панель, и режим показа.
func (s *Server) handleBook(w http.ResponseWriter, r *http.Request) {
	serveBook(w, r)
}

func serveBook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	lang := r.URL.Query().Get("lang")
	if lang != "en" {
		lang = "ru"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"lang": lang, "chapters": BookChapters(lang)})
}
