package vault

// Расширение для браузера на ПК (Яндекс Браузер, Chrome, Edge): подставляет
// логин и пароль на сайтах. Ходит к программе на свой порт 127.0.0.1:ExtPort
// — только с этого компьютера и только с ключом сопряжения, который
// показывает плагин (новый ключ — старые расширения больше не пускаются).
// Отдаёт пароли, только пока хранилище открыто.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/addons"
)

// ExtPort — порт для расширения браузера.
const ExtPort = 47840

var (
	extMu  sync.Mutex
	extSrv *http.Server
)

func extKeyPath() string { return filepath.Join(addons.DataDir(), "vault-ext.key") }

func extKey() string {
	b, _ := os.ReadFile(extKeyPath())
	return strings.TrimSpace(string(b))
}

func newExtKey() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	k := base64.RawURLEncoding.EncodeToString(b)
	_ = os.WriteFile(extKeyPath(), []byte(k), 0o600)
	return k
}

func extInfo() map[string]any {
	if runtime.GOOS == "android" {
		return nil
	}
	extMu.Lock()
	up := extSrv != nil
	extMu.Unlock()
	return map[string]any{"port": ExtPort, "running": up, "paired": extKey() != ""}
}

func startExt() {
	if runtime.GOOS == "android" {
		return
	}
	extMu.Lock()
	defer extMu.Unlock()
	if extSrv != nil {
		return
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "47840"))
	if err != nil {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/fill", extFill)
	mux.HandleFunc("/save", extSave)
	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		if !extAuth(w, r) {
			return
		}
		writeJSON(w, map[string]bool{"unlocked": Unlocked()})
	})
	extSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go extSrv.Serve(ln)
}

func stopExt() {
	extMu.Lock()
	defer extMu.Unlock()
	if extSrv != nil {
		extSrv.Close()
		extSrv = nil
	}
}

// extAuth — ключ сопряжения и CORS для расширения (origin chrome-extension://).
func extAuth(w http.ResponseWriter, r *http.Request) bool {
	o := r.Header.Get("Origin")
	if strings.HasPrefix(o, "chrome-extension://") || strings.HasPrefix(o, "moz-extension://") {
		w.Header().Set("Access-Control-Allow-Origin", o)
		w.Header().Set("Access-Control-Allow-Headers", "X-Vault-Key, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
	} else if o != "" {
		http.Error(w, "нельзя", http.StatusForbidden) // обычный сайт сюда не ходит
		return false
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	k := extKey()
	if k == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Vault-Key")), []byte(k)) != 1 {
		http.Error(w, "нужен ключ сопряжения из плагина «Пароли»", http.StatusUnauthorized)
		return false
	}
	return true
}

// extFill — логины для сайта ?origin=https://…; с паролями, если открыто.
func extFill(w http.ResponseWriter, r *http.Request) {
	if !extAuth(w, r) {
		return
	}
	if !Unlocked() {
		writeJSON(w, map[string]any{"locked": true})
		return
	}
	type item struct {
		Title string `json:"title"`
		Login string `json:"login"`
		Pass  string `json:"pass"`
	}
	var out []item
	for _, e := range Match(r.URL.Query().Get("origin")) {
		out = append(out, item{e.Title, e.Login, e.Pass})
	}
	writeJSON(w, map[string]any{"items": out})
}

// extSave — расширение предлагает запомнить введённый на сайте пароль.
func extSave(w http.ResponseWriter, r *http.Request) {
	if !extAuth(w, r) {
		return
	}
	var req struct {
		Origin string `json:"origin"`
		Login  string `json:"login"`
		Pass   string `json:"pass"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req) != nil || req.Pass == "" {
		http.Error(w, "нужен POST {origin, login, pass}", http.StatusBadRequest)
		return
	}
	site := normSite(req.Origin)
	// Тот же логин на том же сайте — обновить, а не плодить.
	id := ""
	for _, e := range Match(site) {
		if e.Login == req.Login {
			id = e.ID
		}
	}
	if _, err := Put(Entry{ID: id, Site: site, Login: req.Login, Pass: req.Pass}); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
