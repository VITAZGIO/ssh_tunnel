package mobile

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fakeCB — приложение без экрана: запоминает состояния.
type fakeCB struct {
	mu     sync.Mutex
	states []string
}

func (f *fakeCB) Protect(int) bool           { return true }
func (f *fakeCB) BindLAN(int) bool           { return true }
func (f *fakeCB) OnLog(string)               {}
func (f *fakeCB) ResolveLocal(string) string { return "" }
func (f *fakeCB) OnState(s, detail, kind string) {
	f.mu.Lock()
	f.states = append(f.states, s)
	f.mu.Unlock()
}

// offlineTunnel — туннель на закрытый порт loopback (сервер «недоступен»)
// с сетью устройств без прямых соединений: в локальную сеть тест не ходит.
func offlineTunnel(t *testing.T, meshOn bool) (*Tunnel, *fakeCB) {
	t.Helper()
	dir := t.TempDir()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "id")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	// Порт, который только что был открыт и закрыт: отказ приходит сразу.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	tun := NewTunnel()
	cb := &fakeCB{}
	tun.SetCallbacks(cb)
	if err := tun.Configure("127.0.0.1", port, "u", keyPath, filepath.Join(dir, "known_hosts"),
		1, "", false, false, "", "", false); err != nil {
		t.Fatal(err)
	}
	if err := tun.ConfigureMesh(meshOn, NewMeshKey(), "тест", NewDeviceID(), false, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tun.Stop)
	return tun, cb
}

// Сервер не ответил, а сеть устройств включена: туннель поднимается без
// него, клиент сети работает, обход выключен с ошибкой сервера.
func TestStartCoreБезСервераССетью(t *testing.T) {
	tun, cb := offlineTunnel(t, true)
	if err := tun.StartCore(); err != nil {
		t.Fatalf("StartCore без сервера при включённой сети устройств: %v", err)
	}
	if tun.ServerDownText() == "" {
		t.Error("ServerDownText пуст, а сервер не ответил")
	}
	if s := tun.State(); s != "reconnecting" {
		t.Errorf("состояние %q, ждали reconnecting (сервер ищется в фоне)", s)
	}
	if tun.ServerHasIPv6() {
		t.Error("без сервера IPv6 объявлять нечем")
	}
	var st struct{ State string }
	json.Unmarshal([]byte(tun.MeshStatusJSON()), &st)
	if st.State == "off" || st.State == "stopped" {
		t.Errorf("клиент сети устройств не запущен: %q", st.State)
	}
	client := tun.meshHost.Client()
	if client == nil {
		t.Fatal("нет клиента сети устройств")
	}

	// Обход без сервера не включается, но режим применяется и клиент тот же.
	if err := tun.SetModes(true, true); err == nil {
		t.Error("SetModes(обход) без сервера должен вернуть ошибку")
	}
	if tun.meshHost.Client() != client {
		t.Error("после SetModes появился второй клиент сети устройств")
	}

	// Кнопка «Сеть устройств» гасит и включает того же хозяина.
	tun.SetModes(false, false)
	if tun.meshHost.Running() {
		t.Error("сеть устройств выключена, а клиент работает")
	}
	tun.SetModes(false, true)
	if !tun.meshHost.Running() {
		t.Error("сеть устройств включена обратно, а клиента нет")
	}

	// Ошибка первого подключения экрану не уходит: «error» стёр бы кнопки.
	cb.mu.Lock()
	for _, s := range cb.states {
		if s == "error" {
			t.Errorf("экран получил состояние error: %v", cb.states)
			break
		}
	}
	cb.mu.Unlock()

	tun.Stop()
	if tun.meshHost.Running() {
		t.Error("после Stop клиент сети устройств ещё работает")
	}
	if tun.ServerDownText() != "" {
		t.Error("после Stop осталась ошибка сервера")
	}
}

// Без сети устройств всё как раньше: сервер не ответил — ошибка.
func TestStartCoreБезСервераБезСети(t *testing.T) {
	tun, _ := offlineTunnel(t, false)
	if err := tun.StartCore(); err == nil {
		t.Fatal("StartCore без сервера и без сети устройств должен вернуть ошибку")
	}
	if tun.meshHost.Running() {
		t.Error("клиент сети устройств запущен, хотя сеть выключена")
	}
	var s struct{ State string }
	json.Unmarshal([]byte(tun.MeshSummary()), &s)
	if s.State != "off" {
		t.Errorf("MeshSummary: %q, ждали off", s.State)
	}
}
