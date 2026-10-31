package panel

// Обновление панели из её же настроек: «Проверить обновления» нашла новую
// версию — «Обновить» скачивает файл панели этого релиза, сверяет его с
// SHA256SUMS.txt того же релиза, подменяет работающий файл и перезапускает
// службу. В консоль сервера ходить не нужно.
//
// Подмену делает не сама панель: служба запущена с ProtectSystem, и
// /usr/local/bin для неё только на чтение. Как и установка meshd
// (scriptCommand), она идёт отдельной временной службой systemd-run — уже
// без ограничений панели. Та же временная служба перезапускает панель: свой
// перезапуск процесс сделать не может, его останавливают первым.
//
// Чтобы обновление не выкидывало на страницу входа, сессии перед
// перезапуском сохраняются в папку данных и новая копия подхватывает их
// (см. sessionManager.saveHandoff).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"sshtunnel/internal/updater"
)

// Шаги обновления — их показывает страница, пока ждёт.
const (
	updStageDownload = "download"
	updStageVerify   = "verify"
	updStageInstall  = "install"
	updStageRestart  = "restart"
)

// panelUnit — имя службы панели (так её ставит packaging/panel/install.sh).
const panelUnit = "ssh_tunnel_panel"

// selfUpdate — состояние идущего обновления.
type selfUpdate struct {
	mu      sync.Mutex
	running bool
	stage   string
	target  string
	err     string
}

func (u *selfUpdate) set(stage string) {
	u.mu.Lock()
	u.stage = stage
	u.mu.Unlock()
}

func (u *selfUpdate) fail(err error) {
	u.mu.Lock()
	u.running, u.err = false, err.Error()
	u.mu.Unlock()
}

// Подменяемые в тестах части: откуда качать и как ставить.
var (
	selfUpdateHTTP    = &http.Client{Timeout: 5 * time.Minute}
	selfUpdateLatest  = updater.Latest
	selfUpdateInstall = installAndRestart
)

// handleUpdateInstall — кнопка «Обновить». Работа идёт фоном, страница
// следит за ней через /api/update/progress, а потом ждёт, пока /api/status
// ответит новой версией.
func (s *Server) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "нужен POST"})
		return
	}
	if s.dataDir == "" {
		writeJSON(w, http.StatusOK, map[string]string{"error": "у панели нет папки данных — обнови её вручную"})
		return
	}
	u := &s.update
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"error": "обновление уже идёт"})
		return
	}
	u.running, u.stage, u.err, u.target = true, updStageDownload, "", ""
	u.mu.Unlock()

	go s.runSelfUpdate()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUpdateProgress(w http.ResponseWriter, r *http.Request) {
	u := &s.update
	u.mu.Lock()
	defer u.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"running": u.running, "stage": u.stage, "target": u.target, "error": u.err,
		"version": updater.Version,
	})
}

func (s *Server) runSelfUpdate() {
	u := &s.update
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	rel, err := selfUpdateLatest(ctx)
	if err != nil {
		u.fail(err)
		return
	}
	if updater.Compare(updater.Version, rel.Tag) >= 0 {
		u.fail(fmt.Errorf("уже стоит последняя версия (%s)", strings.TrimPrefix(rel.Tag, "v")))
		return
	}
	u.mu.Lock()
	u.target = strings.TrimPrefix(rel.Tag, "v")
	u.mu.Unlock()

	name := panelAssetName(runtime.GOARCH)
	var bin, sums updater.Asset
	for _, a := range rel.Assets {
		switch a.Name {
		case name:
			bin = a
		case "SHA256SUMS.txt":
			sums = a
		}
	}
	if bin.URL == "" || sums.URL == "" {
		u.fail(fmt.Errorf("в релизе %s нет файла %s или контрольных сумм — обнови вручную", rel.Tag, name))
		return
	}

	dst := filepath.Join(s.dataDir, "ssh_tunnel_panel.new")
	got, err := downloadFile(ctx, bin.URL, dst)
	if err != nil {
		os.Remove(dst)
		u.fail(fmt.Errorf("не скачалось: %w", err))
		return
	}

	u.set(updStageVerify)
	want, err := expectedSum(ctx, sums.URL, name)
	if err == nil && !strings.EqualFold(want, got) {
		err = errors.New("контрольная сумма не совпала — файл повреждён или подменён")
	}
	if err == nil {
		err = checkELF(dst)
	}
	if err != nil {
		os.Remove(dst)
		u.fail(err)
		return
	}

	u.set(updStageInstall)
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		os.Remove(dst)
		u.fail(fmt.Errorf("не нашёл свой файл: %w", err))
		return
	}
	// Сессии — новой копии: обновление не должно выкидывать на вход.
	if err := s.sessions.saveHandoff(handoffPath(s.dataDir)); err != nil {
		// Не страшно: просто придётся войти заново.
		fmt.Fprintln(os.Stderr, "сессии не сохранились:", err)
	}
	u.set(updStageRestart)
	if err := selfUpdateInstall(dst, exe); err != nil {
		os.Remove(handoffPath(s.dataDir))
		os.Remove(dst)
		u.fail(err)
	}
}

// panelAssetName — файл панели в релизе под эту архитектуру.
func panelAssetName(goarch string) string {
	if goarch == "arm64" {
		return "ssh_tunnel_panel_arm64"
	}
	return "ssh_tunnel_panel"
}

// downloadFile качает url в dst и возвращает SHA-256 скачанного.
func downloadFile(ctx context.Context, url, dst string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ssh_tunnel_panel/"+updater.Version)
	resp, err := selfUpdateHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub ответил %s", resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	// Файл панели — около 8 МБ; 200 МБ — заведомо больше любого настоящего.
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 200<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// expectedSum достаёт из SHA256SUMS.txt строку «<сумма>  <имя>» этого файла.
func expectedSum(ctx context.Context, url, name string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := selfUpdateHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("не скачались контрольные суммы: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("контрольные суммы: GitHub ответил %s", resp.Status)
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("в контрольных суммах нет файла %s", name)
}

// checkELF — скачанное похоже на программу для Linux, а не на страницу
// ошибки.
func checkELF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil || !bytes.Equal(head, []byte("\x7fELF")) {
		return errors.New("скачанный файл — не программа для Linux")
	}
	return nil
}

// installAndRestart подменяет файл панели и перезапускает её. Под systemd —
// временной службой без ограничений панели (ProtectSystem не даёт писать в
// /usr): она дожидается, пока панель отдаст ответ, ставит файл и
// перезапускает службу. Панель, запущенная руками, подменяет файл сама
// (переименование поверх работающей программы в Linux разрешено) и
// перезапускает себя тем же процессом.
func installAndRestart(src, exe string) error {
	if os.Getenv("INVOCATION_ID") != "" {
		path, err := exec.LookPath("systemd-run")
		if err != nil {
			return errors.New("нет systemd-run — обнови панель вручную")
		}
		unit := "ssh_tunnel_panel_update_" + strconv.FormatInt(time.Now().UnixNano(), 36)
		script := `sleep 1; install -m 755 "$1" "$2" && rm -f "$1" && systemctl restart ` + panelUnit
		out, err := exec.Command(path, "--quiet", "--collect", "--unit="+unit,
			"--", "/bin/sh", "-c", script, "sh", src, exe).CombinedOutput()
		if err != nil {
			return fmt.Errorf("не запустилась установка: %v %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := os.Rename(src, exe); err != nil {
		return fmt.Errorf("не удалось подменить файл: %w", err)
	}
	go func() {
		time.Sleep(time.Second)
		syscall.Exec(exe, os.Args, os.Environ())
	}()
	return nil
}

func handoffPath(dataDir string) string { return filepath.Join(dataDir, "sessions.handoff") }
