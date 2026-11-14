package node

// «Проверить сайт» с сервера (ТЗ 15): устройство (или панель) просит
// сервер пройти те же шаги DNS → TCP → TLS → HTTP из его сети
// (diag/probe.CheckFromServer). Это не прокси: в запросе только имя сайта
// и язык, проверка — один GET «/» на 443, локальные и закрытые адреса
// отвергаются, каждое устройство — не чаще siteBurst проверок подряд и
// одной в siteRefill дальше; одновременно — не больше siteParallel.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/diag/probe"
	"sshtunnel/internal/nodeapi"
)

const (
	siteBurst    = 5
	siteRefill   = 10 * time.Second
	siteParallel = 4
	siteTimeout  = 45 * time.Second
)

// siteLimiter — ведро на устройство: siteBurst сразу, дальше по одной в
// siteRefill.
type siteLimiter struct {
	mu   sync.Mutex
	b    map[string]*siteBucket
	sem  chan struct{}
	now  func() time.Time
	seen time.Time
}

type siteBucket struct {
	tokens float64
	at     time.Time
}

func newSiteLimiter(now func() time.Time) *siteLimiter {
	return &siteLimiter{b: map[string]*siteBucket{}, sem: make(chan struct{}, siteParallel), now: now}
}

// take — можно ли устройству key проверить сейчас; нет — через сколько.
func (l *siteLimiter) take(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.seen) > time.Hour {
		// Раз в час выбросить вёдра, которые давно полны.
		for k, b := range l.b {
			if now.Sub(b.at) > siteRefill*siteBurst {
				delete(l.b, k)
			}
		}
		l.seen = now
	}
	b := l.b[key]
	if b == nil {
		b = &siteBucket{tokens: siteBurst, at: now}
		l.b[key] = b
	}
	b.tokens += float64(now.Sub(b.at)) / float64(siteRefill)
	if b.tokens > siteBurst {
		b.tokens = siteBurst
	}
	b.at = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) * float64(siteRefill))
	}
	b.tokens--
	return true, 0
}

// CheckSite — проверить сайт с сервера (панель и устройства).
func (h *Host) CheckSite(ctx context.Context, req nodeapi.SiteReq) (probe.ServerResult, error) {
	site := strings.TrimSpace(req.Site)
	if site == "" || len(site) > 300 || strings.ContainsAny(site, " \t\r\n") {
		return probe.ServerResult{}, errors.New("нужен адрес сайта, например example.com")
	}
	lang := "ru"
	if req.Lang == "en" {
		lang = "en"
	}
	select {
	case h.site.sem <- struct{}{}:
	case <-ctx.Done():
		return probe.ServerResult{}, ctx.Err()
	}
	defer func() { <-h.site.sem }()
	ctx, cancel := context.WithTimeout(ctx, siteTimeout)
	defer cancel()
	if h.opt.SiteCheck != nil {
		return h.opt.SiteCheck(ctx, site, lang)
	}
	return probe.CheckFromServer(ctx, site, lang, probe.ServerOptions{})
}

// serveSitecheck — POST /check {"site","lang"} от устройства сети.
func (h *Host) serveSitecheck(w http.ResponseWriter, r *http.Request, nd *netData) {
	if r.URL.Path != "/check" || r.Method != http.MethodPost {
		writeErr(w, http.StatusNotFound, "нет такого адреса: POST /check {\"site\"}")
		return
	}
	var req nodeapi.SiteReq
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "нужен JSON {\"site\":\"example.com\"}")
		return
	}
	key := nd.id + "/" + nodeapi.PeerID(callerOf(r).Device)
	if ok, wait := h.site.take(key); !ok {
		w.Header().Set("Retry-After", strings.TrimSuffix(wait.Round(time.Second).String(), "s"))
		writeErr(w, http.StatusTooManyRequests, "слишком часто — сервер проверяет не больше 5 сайтов подряд и дальше по одному в 10 секунд")
		return
	}
	res, err := h.CheckSite(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONTo(w, res)
}
