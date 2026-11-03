// Package uihost — сервер общих страниц интерфейса для Android: тот же HTML
// (internal/uiassets) и то же API дополнительных приложений и Книги, что в
// окне ПК, без настольной части internal/webui. WebView на телефоне ходит
// сюда по 127.0.0.1 с ключом, как окно ПК — в webui.
//
// Остальное API (связь, настройки, серверы, выбор приложений) на телефоне
// живёт у VPN-службы и настроек приложения: его отдаёт обработчик платформы
// (SetAPI) по тем же адресам и в том же виде, что webui на ПК, — поэтому
// главный экран один на обе платформы. Поток событий /events общий: события
// присылает платформа (Emit).
package uihost

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/addons"
	_ "sshtunnel/internal/addons/devapps"
	"sshtunnel/internal/book"
	"sshtunnel/internal/calendar"
	"sshtunnel/internal/uiassets"
)

// Host — работающий сервер страниц.
type Host struct {
	ln    net.Listener
	token string
	svc   *addons.Service

	mu   sync.Mutex
	api  http.Handler
	subs map[chan []byte]struct{}
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
	// Календарь: данные — до запуска модулей приложений; как ходить к
	// Nextcloud, задаёт ядро телефона (calendar.SetDial в android/core/mobile).
	calendar.Init(dir)
	h := &Host{ln: ln, token: hex.EncodeToString(b), svc: addons.New(dir),
		subs: map[chan []byte]struct{}{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/", uiassets.Static())
	mux.HandleFunc("/addons/", h.guard(h.svc.Page))
	mux.HandleFunc("/custom/", h.svc.Files)
	for _, rt := range h.svc.Routes() {
		mux.HandleFunc(rt.Path, h.guard(rt.H))
	}
	mux.HandleFunc("/api/book", h.guard(book.Handler(addons.BookChapters)))
	// Всё остальное API — платформе (ServeMux выбирает самый длинный
	// подходящий путь, поэтому свои адреса выше остаются своими).
	mux.HandleFunc("/api/", h.guard(h.platform))
	mux.HandleFunc("/events", h.guard(h.events))
	srv := &http.Server{Handler: addons.CORS(mux), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return h, nil
}

// URL — адрес сервера (http://127.0.0.1:порт).
func (h *Host) URL() string { return "http://" + h.ln.Addr().String() }

// Token — ключ, без которого API не отвечает.
func (h *Host) Token() string { return h.token }

// SetAPI — обработчик остального API (на телефоне — приложение). Пока его
// нет, такие адреса отвечают ошибкой, а страницы и Книга работают.
func (h *Host) SetAPI(api http.Handler) {
	h.mu.Lock()
	h.api = api
	h.mu.Unlock()
}

// Emit рассылает событие (готовый JSON вида {"kind":…}) всем открытым
// потокам /events. Медленный получатель событие пропускает — экран
// перерисуется по следующему, а служба не ждёт WebView.
func (h *Host) Emit(event []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- event:
		default:
		}
	}
}

// Close останавливает сервер и модули приложений.
func (h *Host) Close() {
	h.ln.Close()
	h.svc.Close()
}

func (h *Host) platform(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	api := h.api
	h.mu.Unlock()
	if api == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"приложение ещё не готово"}`)
		return
	}
	api.ServeHTTP(w, r)
}

// events — поток Server-Sent Events, как /events в webui.
func (h *Host) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "поток не поддерживается", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, ": ok\n\n")
	fl.Flush()
	// Пустая строка раз в 20 с: WebView не закрывает молчащий поток.
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
		case ev := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
		fl.Flush()
	}
}

func (h *Host) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Своё приложение: свой ключ, только разрешённые адреса.
		if addons.IsAppKey(addons.RequestToken(r)) {
			r2, err := h.svc.Authorize(r)
			if err != nil {
				addons.DenyJSON(w, err)
				return
			}
			next(w, r2)
			return
		}
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
