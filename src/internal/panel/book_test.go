package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func norm(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }

// Встроенная копия совпадает с docs/book-panel: главы правят там, а сюда их
// копирует go generate ./internal/panel.
func TestКнигаПанелиКопияСовпадаетСИсточником(t *testing.T) {
	for _, lang := range []string{"ru", "en"} {
		docs := filepath.Join("..", "..", "..", "docs", "book-panel", lang)
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
			b, err := bookText.ReadFile("booktext/" + lang + "/" + e.Name())
			if err != nil || !bytes.Equal(norm(a), norm(b)) {
				t.Errorf("%s/%s разошёлся с docs/book-panel — запусти: cd src/internal/panel && go generate", lang, e.Name())
			}
		}
		got, _ := bookText.ReadDir("booktext/" + lang)
		for _, e := range got {
			if !want[e.Name()] {
				t.Errorf("%s/%s есть во встроенной копии, но нет в docs/book-panel", lang, e.Name())
			}
		}
	}
}

// Файлы глав в ru и en называются одинаково, и у каждой есть заголовок «# ».
func TestКнигаПанелиИменаРуИEnСовпадают(t *testing.T) {
	ids := func(cs []BookChapter) string {
		var s []string
		for _, c := range cs {
			s = append(s, c.ID)
		}
		return strings.Join(s, ",")
	}
	ru, en := BookChapters("ru"), BookChapters("en")
	if len(ru) == 0 {
		t.Fatal("в Книге нет ни одной главы")
	}
	if ids(ru) != ids(en) {
		t.Fatalf("главы ru (%s) и en (%s) называются по-разному", ids(ru), ids(en))
	}
	for _, c := range append(ru, en...) {
		if !strings.HasPrefix(c.MD, "# ") {
			t.Errorf("%s: первая строка не «# Заголовок»", c.ID)
		}
		if c.Title == c.ID {
			t.Errorf("%s: заголовок не прочитан", c.ID)
		}
	}
}

// Порядок — по имени файла, заголовок — первая строка «# », нет заголовка —
// заголовком служит id, CRLF превращается в LF, чужие файлы не в счёт.
func TestКнигаПанелиПорядокИЗаголовки(t *testing.T) {
	fsys := fstest.MapFS{
		"d/10-b.md":    {Data: []byte("# Вторая\r\nтекст\r\n")},
		"d/02-a.md":    {Data: []byte("# Первая\n")},
		"d/90-last.md": {Data: []byte("без заголовка\n")},
		"d/notes.txt":  {Data: []byte("# не глава")},
		"d/sub/x.md":   {Data: []byte("# вложенная")},
	}
	got := chaptersFrom(fsys, "d")
	if len(got) != 3 || got[0].ID != "02-a" || got[1].ID != "10-b" || got[2].ID != "90-last" {
		t.Fatalf("порядок: %+v", got)
	}
	if got[0].Title != "Первая" || got[1].Title != "Вторая" || got[2].Title != "90-last" {
		t.Fatalf("заголовки: %+v", got)
	}
	if strings.Contains(got[1].MD, "\r") {
		t.Error("CRLF не убран")
	}
	if len(chaptersFrom(fsys, "нет")) != 0 {
		t.Error("нет папки — нет глав")
	}
}

func TestКнигаПанелиЯзыкПоУмолчаниюРусский(t *testing.T) {
	h := NewDemoHandler("test")
	body := func(q string) map[string]any {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/book"+q, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d", q, rec.Code)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	title := func(o map[string]any) string {
		return o["chapters"].([]any)[0].(map[string]any)["title"].(string)
	}
	ru, en := body("?lang=ru"), body("?lang=en")
	if ru["lang"] != "ru" || en["lang"] != "en" {
		t.Fatal("язык в ответе")
	}
	if title(ru) == title(en) {
		t.Fatal("ru и en отдали один текст")
	}
	for _, q := range []string{"", "?lang=xx", "?lang="} {
		if o := body(q); o["lang"] != "ru" || title(o) != title(ru) {
			t.Errorf("%q: нужен русский по умолчанию", q)
		}
	}
	// Глава целиком: id, title, md.
	c := ru["chapters"].([]any)[0].(map[string]any)
	if c["id"] == "" || c["md"] == "" {
		t.Fatalf("глава неполная: %v", c)
	}
}

// Боевая панель отдаёт Книгу только вошедшему.
func TestКнигаПанелиТребуетВхода(t *testing.T) {
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/book", s.requireAuth(s.handleBook))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/book?lang=ru", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("без входа: %d, нужен 401", rec.Code)
	}
}

// Таблица «? → глава» на странице: ссылается только на имена глав из ТЗ
// (01-about … 15-update, 90-problems) — иначе опечатка молча прячет ссылку.
func TestКнигаПанелиТаблицаПодсказок(t *testing.T) {
	page, err := assets.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(page)
	i := strings.Index(s, "const BOOK_HELP = {")
	if i < 0 {
		t.Fatal("на странице нет таблицы BOOK_HELP")
	}
	body := s[i : i+strings.Index(s[i:], "};")]
	known := map[string]bool{"90-problems": true}
	for _, id := range strings.Fields("01-about 02-first-login 03-devices 04-networks 05-access 06-servers " +
		"07-no-server 08-exit-chain 09-server-egress 10-docker 11-plugins 12-backups 13-journal 14-demo 15-update") {
		known[id] = true
	}
	re := regexp.MustCompile(`(hint_[a-z_0-9]+):'([^']+)'`)
	ms := re.FindAllStringSubmatch(body, -1)
	if len(ms) < 20 {
		t.Fatalf("в таблице подозрительно мало строк: %d", len(ms))
	}
	for _, m := range ms {
		if !known[m[2]] {
			t.Errorf("%s ведёт в неизвестную главу %q", m[1], m[2])
		}
		if !strings.Contains(s, "data-hint=\""+m[1]+"\"") && !strings.Contains(s, "data-hint=\\\""+m[1]) {
			t.Errorf("%s: такого «?» на странице нет", m[1])
		}
	}
}

// Страница читается без сети: ни внешних скриптов, ни стилей, ни запросов на
// чужие адреса в блоке Книги.
func TestКнигаПанелиБезСети(t *testing.T) {
	page, _ := assets.ReadFile("assets/index.html")
	s := string(page)
	a, b := strings.Index(s, "/* BOOK-RENDER-BEGIN */"), strings.Index(s, "/* BOOK-RENDER-END */")
	if a < 0 || b < a {
		t.Fatal("нет меток BOOK-RENDER")
	}
	if strings.Contains(s[a:b], "fetch(") || strings.Contains(s[a:b], "innerHTML = s") {
		t.Error("разбор Markdown не должен ходить в сеть")
	}
	if regexp.MustCompile(`<script[^>]+src=|<link[^>]+href="https?:`).MatchString(s) {
		t.Error("страница тянет внешний ресурс")
	}
}

// node --check на скрипте страницы (если node есть на машине).
func TestСтраницаПанелиСинтаксисJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node не найден")
	}
	page, _ := assets.ReadFile("assets/index.html")
	var js []string
	for _, m := range regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(string(page), -1) {
		js = append(js, m[1])
	}
	f := filepath.Join(t.TempDir(), "page.js")
	if err := os.WriteFile(f, []byte(strings.Join(js, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}
