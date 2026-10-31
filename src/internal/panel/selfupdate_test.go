package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sshtunnel/internal/updater"
)

// fakeRelease поднимает «GitHub» с файлом панели и контрольными суммами.
// sum — что записать в SHA256SUMS.txt; пусто — настоящая сумма файла.
func fakeRelease(t *testing.T, body []byte, sum string) updater.Release {
	t.Helper()
	if sum == "" {
		h := sha256.Sum256(body)
		sum = hex.EncodeToString(h[:])
	}
	name := panelAssetName("amd64")
	mux := http.NewServeMux()
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("0000  other\n" + sum + "  " + name + "\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// Сборка в тестах — "dev"; для сравнения нужна настоящая старая версия.
	old := updater.Version
	updater.Version = "v1.0.0"
	t.Cleanup(func() { updater.Version = old })
	return updater.Release{Tag: "v9.9.9", Assets: []updater.Asset{
		{Name: panelAssetName("amd64"), URL: srv.URL + "/bin"},
		{Name: panelAssetName("arm64"), URL: srv.URL + "/bin"},
		{Name: "SHA256SUMS.txt", URL: srv.URL + "/sums"},
	}}
}

func runUpdate(t *testing.T, s *Server, rel updater.Release) (installed [2]string, called bool) {
	t.Helper()
	oldLatest, oldInstall := selfUpdateLatest, selfUpdateInstall
	selfUpdateLatest = func(context.Context) (updater.Release, error) { return rel, nil }
	selfUpdateInstall = func(src, exe string) error { installed, called = [2]string{src, exe}, true; return nil }
	t.Cleanup(func() { selfUpdateLatest, selfUpdateInstall = oldLatest, oldInstall })
	s.update.running = true
	s.runSelfUpdate()
	return
}

// «Обновить»: скачанный файл сверен с суммой и передан на установку, сессии
// сохранены для новой копии.
func TestSelfUpdateInstallsVerifiedBinary(t *testing.T) {
	s, _ := newTestServer(t)
	s.WithDataDir(t.TempDir())
	id, _ := s.sessions.create("admin", false)
	body := append([]byte("\x7fELF"), make([]byte, 1000)...)

	inst, called := runUpdate(t, s, fakeRelease(t, body, ""))
	if s.update.err != "" {
		t.Fatalf("ошибка: %s", s.update.err)
	}
	if !called {
		t.Fatal("установка не вызвана")
	}
	got, err := os.ReadFile(inst[0])
	if err != nil || string(got) != string(body) {
		t.Fatalf("на установку ушёл не тот файл (%v)", err)
	}
	if s.update.target != "9.9.9" || s.update.stage != updStageRestart {
		t.Fatalf("target=%q stage=%q", s.update.target, s.update.stage)
	}

	// Новая копия панели подхватывает сессию — вход не нужен.
	next, _ := newTestServer(t)
	next.WithDataDir(s.dataDir)
	if _, ok := next.sessions.get(id); !ok {
		t.Fatal("сессия не пережила обновление")
	}
	if _, err := os.Stat(handoffPath(s.dataDir)); !os.IsNotExist(err) {
		t.Fatal("файл с сессиями не удалён после чтения")
	}
}

// Файл не совпал с контрольной суммой — ничего не ставим.
func TestSelfUpdateRejectsBadChecksum(t *testing.T) {
	s, _ := newTestServer(t)
	s.WithDataDir(t.TempDir())
	body := append([]byte("\x7fELF"), make([]byte, 100)...)

	_, called := runUpdate(t, s, fakeRelease(t, body, "deadbeef"))
	if called {
		t.Fatal("поставлен файл с неверной суммой")
	}
	if s.update.err == "" || s.update.running {
		t.Fatalf("ошибка не показана: %+v", s.update.err)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "ssh_tunnel_panel.new")); !os.IsNotExist(err) {
		t.Fatal("отвергнутый файл остался на диске")
	}
}

// Вместо программы скачалась страница — не ставим.
func TestSelfUpdateRejectsNonBinary(t *testing.T) {
	s, _ := newTestServer(t)
	s.WithDataDir(t.TempDir())
	_, called := runUpdate(t, s, fakeRelease(t, []byte("<html>not found</html>"), ""))
	if called || s.update.err == "" {
		t.Fatal("поставлена не программа")
	}
}

// Устаревший файл сессий (обновление сорвалось давно) не подхватывается.
func TestHandoffIgnoresStaleFile(t *testing.T) {
	dir := t.TempDir()
	m := newSessionManager()
	id, _ := m.create("admin", false)
	if err := m.saveHandoff(handoffPath(dir)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	os.Chtimes(handoffPath(dir), old, old)
	n := newSessionManager()
	n.loadHandoff(handoffPath(dir))
	if _, ok := n.get(id); ok {
		t.Fatal("подхвачены сессии из старого файла")
	}
}
