package diag

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"sshtunnel/internal/updater"
)

// «Отчёт для поддержки»: один zip — журнал за последние дни, настройки с
// *** вместо секретов, свежие результаты проверок, версия, система,
// адаптеры. Перед сохранением экран показывает содержимое. На сервер ничего
// не отправляется.

// ReportFile — один файл отчёта.
type ReportFile struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// reportDays — за сколько дней брать журнал.
const reportDays = 3

// BuildReport собирает файлы отчёта. dir — папка настроек (журнал, api.json,
// service.log).
func BuildReport(h Host, st *Store, dir string) []ReportFile {
	var known []string
	// Ключ API из api.json — секрет, где бы он ни встретился.
	if b, err := os.ReadFile(filepath.Join(dir, "api.json")); err == nil {
		var a struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(b, &a) == nil && a.Token != "" {
			known = append(known, a.Token)
		}
	}
	settings := maskDoc(h.Config(), &known)
	d := st.read()
	results := maskDoc(map[string]any{
		"sites":    d.History,
		"meddlers": d.Meddlers,
		"disabled": d.Disabled,
		"leaks":    d.Leaks,
	}, &known)

	files := []ReportFile{
		{"system.txt", maskText(systemInfo(h), known)},
		{"settings.json", string(settings)},
		{"checks.json", string(results)},
		{"journal.txt", maskText(recentJournal(dir, reportDays), known)},
	}
	if b, err := os.ReadFile(filepath.Join(dir, "service.log")); err == nil {
		if len(b) > 512<<10 {
			b = b[len(b)-512<<10:]
		}
		files = append(files, ReportFile{"service.log", maskText(string(b), known)})
	}
	return files
}

func systemInfo(h Host) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ssh_tunnel %s\n", updater.Version)
	fmt.Fprintf(&b, "Система: %s/%s %s\n", runtime.GOOS, runtime.GOARCH, osVersion())
	fmt.Fprintf(&b, "Время: %s\n", time.Now().Format(time.RFC3339))
	mode := "прокси"
	if h.VPNMode() {
		mode = "VPN"
	}
	fmt.Fprintf(&b, "Режим: %s, туннель %s, обход %v\n", mode, map[bool]string{true: "подключён", false: "не подключён"}[h.TunnelUp()], h.Bypass())
	b.WriteString("\nАдаптеры:\n")
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		up := "выкл"
		if i.Flags&net.FlagUp != 0 {
			up = "вкл"
		}
		var addrs []string
		as, _ := i.Addrs()
		for _, a := range as {
			addrs = append(addrs, a.String())
		}
		fmt.Fprintf(&b, "  %s [%s] mtu %d  %s\n", i.Name, up, i.MTU, strings.Join(addrs, ", "))
	}
	return b.String()
}

// Zip — отчёт одним файлом.
func Zip(files []ReportFile) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f.Name, Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(f.Text)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SaveReport кладёт отчёт в «Загрузки» и возвращает путь.
func SaveReport(files []ReportFile) (string, error) {
	data, err := Zip(files)
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Downloads")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		dir = home
	}
	p := filepath.Join(dir, "ssh_tunnel-report-"+time.Now().Format("20060102-150405")+".zip")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", err
	}
	return p, nil
}
