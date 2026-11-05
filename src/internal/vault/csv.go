package vault

// Импорт и экспорт паролей CSV-файлом. Так их отдают Chrome, Яндекс Браузер,
// Edge, Firefox, Bitwarden и KeePass: прочитать базу браузера напрямую нельзя
// (Chrome шифрует её ключом, привязанным к самому браузеру).

import (
	"bytes"
	"crypto/subtle"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// colAliases — как называются колонки у разных программ.
var colAliases = map[string][]string{
	"title": {"name", "title", "название", "имя"},
	"url":   {"url", "login_uri", "website", "web site", "origin", "hostname", "сайт", "адрес"},
	"login": {"username", "login", "login_username", "user", "user name", "email", "логин", "имя пользователя"},
	"pass":  {"password", "login_password", "pass", "пароль"},
	"note":  {"note", "notes", "comment", "comments", "заметка", "примечание"},
}

// ImportCSV — добавить записи из CSV. Такая же (сайт+логин+пароль) —
// пропуск, тот же сайт и логин с другим паролем — обновить.
func ImportCSV(data []byte) (added, updated, skipped int, err error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	rd := csv.NewReader(bytes.NewReader(data))
	rd.FieldsPerRecord = -1
	rd.LazyQuotes = true
	rows, err := rd.ReadAll()
	if err != nil || len(rows) < 2 {
		if err == nil {
			err = errors.New("в файле нет паролей")
		}
		return 0, 0, 0, fmt.Errorf("не CSV: %v", err)
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		h = strings.ToLower(strings.TrimSpace(h))
		for k, names := range colAliases {
			if _, ok := col[k]; ok {
				continue
			}
			for _, n := range names {
				if h == n {
					col[k] = i
				}
			}
		}
	}
	if _, ok := col["pass"]; !ok {
		return 0, 0, 0, errors.New("нет колонки password — это не файл паролей")
	}
	get := func(r []string, k string) string {
		if i, ok := col[k]; ok && i < len(r) {
			return strings.TrimSpace(r[i])
		}
		return ""
	}

	mu.Lock()
	defer mu.Unlock()
	if key == nil {
		return 0, 0, 0, errors.New("хранилище закрыто")
	}
	now := time.Now().UnixMilli()
	for _, r := range rows[1:] {
		pass := get(r, "pass")
		if pass == "" {
			continue
		}
		e := Entry{Title: get(r, "title"), Site: normSite(get(r, "url")), Login: get(r, "login"), Pass: pass, Note: get(r, "note"), Upd: now}
		if e.Site == "" && e.Title != "" && strings.Contains(e.Title, ".") {
			e.Site = normSite(e.Title)
		}
		if e.Title == "" {
			e.Title = e.Site
		}
		found := false
		for i := range entries {
			x := &entries[i]
			if x.Del || x.Site != e.Site || x.Login != e.Login {
				continue
			}
			found = true
			if x.Pass == e.Pass {
				skipped++
			} else {
				x.Pass, x.Upd = e.Pass, now
				updated++
			}
			break
		}
		if !found {
			e.ID = randID()
			entries = append(entries, e)
			added++
		}
	}
	if added+updated > 0 {
		if err := save(); err != nil {
			return 0, 0, 0, err
		}
		go Sync()
	}
	return added, updated, skipped, nil
}

// ExportCSV — все пароли в «Загрузки» файлом в формате Chrome (его понимают
// все браузеры и менеджеры паролей). Нужен главный пароль ещё раз: файл
// лежит открытым текстом.
func ExportCSV(pass string) (string, int, error) {
	mu.Lock()
	if key == nil {
		mu.Unlock()
		return "", 0, errors.New("хранилище закрыто")
	}
	if subtle.ConstantTimeCompare([]byte(pass), []byte(master)) != 1 {
		mu.Unlock()
		return "", 0, errors.New("неверный главный пароль")
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Write([]string{"name", "url", "username", "password", "note"})
	n := 0
	for _, e := range entries {
		if e.Del {
			continue
		}
		u := e.Site
		if u != "" && strings.Contains(u, ".") && !strings.Contains(u, "://") {
			u = "https://" + u + "/"
		}
		w.Write([]string{e.Title, u, e.Login, e.Pass, e.Note})
		n++
	}
	mu.Unlock()
	w.Flush()

	dir := "/storage/emulated/0/Download"
	if runtime.GOOS != "android" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", 0, err
		}
		dir = filepath.Join(h, "Downloads")
	}
	p := filepath.Join(dir, "ssh_tunnel-пароли-"+time.Now().Format("2006-01-02")+".csv")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		return "", 0, err
	}
	return p, n, nil
}
