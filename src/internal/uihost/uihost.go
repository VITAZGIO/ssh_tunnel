// Package uihost — сервер общих страниц интерфейса для Android: тот же HTML
// (internal/uiassets) и то же API дополнительных приложений и Книги, что в
// окне ПК, без настольной части internal/webui. WebView на телефоне ходит
// сюда по 127.0.0.1 с ключом, как окно ПК — в webui.
package uihost

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/book"
	"sshtunnel/internal/uiassets"
)

// Host — работающий сервер страниц.
type Host struct {
	ln    net.Listener
	token string
	svc   *addons.Service
}

// Start поднимает сервер на 127.0.0.1 со случайным портом; dir — папка
// данных приложения (там addons.json).
func Start(dir string) (*Host, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := &Host{ln: ln, token: hex.EncodeToString(b), svc: addons.New(dir)}

	mux := http.NewServeMux()
	mux.HandleFunc("/", uiassets.Static())
	mux.HandleFunc("/addons/", h.guard(h.svc.Page))
	for _, rt := range h.svc.Routes() {
		mux.HandleFunc(rt.Path, h.guard(rt.H))
	}
	mux.HandleFunc("/api/book", h.guard(book.Handler(addons.BookChapters)))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return h, nil
}

// URL — адрес сервера (http://127.0.0.1:порт).
func (h *Host) URL() string { return "http://" + h.ln.Addr().String() }

// Token — ключ, без которого API не отвечает.
func (h *Host) Token() string { return h.token }

// Close останавливает сервер и модули приложений.
func (h *Host) Close() {
	h.ln.Close()
	h.svc.Close()
}

func (h *Host) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(w, "запрещено", http.StatusForbidden)
				return
			}
		}
		tok := r.Header.Get("X-Token")
		if tok == "" {
			tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if tok == "" {
			tok = r.URL.Query().Get("t")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(h.token)) != 1 {
			http.Error(w, "запрещено", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
