package node

// «Блокировка рекламы» на сервере (ТЗ 15): один список на сервер, который
// ведёт панель (источники, свои «блокировать» и «не блокировать»), — и
// каждое устройство сети с плагином блокировки забирает его отсюда вместо
// того, чтобы качать списки само. Блокирует по-прежнему устройство (его
// DNS в режиме VPN, android/core.BlockList); сервер только хранит и
// раздаёт.
//
// Раздача: GET /list на порт PortAdblock — текст, имя в строке; строка
// «@@имя» — исключение (то же, что в списках adblock). ETag — метка списка:
// устройство спрашивает с If-None-Match и получает 304, пока список тот же.
// Свои «не блокировать» панели уже вычтены из имён, но приходят и строками
// «@@»: исключение действует и на поддомены заблокированного родителя.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"sshtunnel/internal/nodeapi"
)

const (
	maxAdSources = 32
	maxAdOwn     = 10000
	// adEvery — как часто обновлять список с источников.
	adEvery = 24 * time.Hour
	// maxAdSourceBytes — один источник не больше.
	maxAdSourceBytes = 64 << 20
)

// adMeta — adblock/meta.json.
type adMeta struct {
	Updated int64                   `json:"updated"`
	Checked int64                   `json:"checked"`
	Error   string                  `json:"error,omitempty"`
	Fetched []nodeapi.AdblockSource `json:"fetched,omitempty"`
	Version string                  `json:"version,omitempty"`
	Names   int                     `json:"names"`
}

func (h *Host) adDir() string { return filepath.Join(h.opt.Dir, "adblock") }

func (h *Host) loadAdMeta() {
	if b, err := os.ReadFile(filepath.Join(h.adDir(), "meta.json")); err == nil {
		_ = json.Unmarshal(b, &h.ad)
	}
}

// normAdName — имя для списка: нижний регистр, без точки в конце; "" —
// не имя.
func normAdName(s string) string {
	s = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
	s = strings.TrimPrefix(s, "*.")
	if s == "" || len(s) > 253 {
		return ""
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
			return ""
		}
	}
	return s
}

// parseAdList — формат hosts («0.0.0.0 имя»), имя в строке и простые
// правила adblock («||имя^»). Комментарии «#» и «!» пропускаются.
func parseAdList(r io.Reader) []string {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '!' || line[0] == '[' || strings.HasPrefix(line, "@@") {
			continue
		}
		if strings.HasPrefix(line, "||") {
			line = strings.TrimSuffix(strings.TrimPrefix(line, "||"), "^")
			if n := normAdName(line); n != "" {
				out = append(out, n)
			}
			continue
		}
		f := strings.Fields(line)
		if looksIP(f[0]) {
			f = f[1:]
		}
		for _, x := range f {
			if n := normAdName(x); n != "" && n != "localhost" && n != "localhost.localdomain" && n != "broadcasthost" {
				out = append(out, n)
			}
		}
	}
	return out
}

func looksIP(s string) bool {
	if strings.Count(s, ".") == 3 || strings.Contains(s, ":") {
		for _, c := range s {
			if !(c >= '0' && c <= '9' || c == '.' || c == ':' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	}
	return false
}

// underAllow — имя или его родитель в исключениях.
func underAllow(allow map[string]bool, n string) bool {
	for {
		if allow[n] {
			return true
		}
		i := strings.IndexByte(n, '.')
		if i < 0 {
			return false
		}
		n = n[i+1:]
	}
}

func cleanList(in []string, max int, norm func(string) (string, error)) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		n, err := norm(s)
		if err != nil {
			return nil, err
		}
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) > max {
		return nil, fmt.Errorf("не больше %d строк", max)
	}
	return out, nil
}

// SetAdblock — источники и свои записи (панель). Список пересобирается.
func (h *Host) SetAdblock(c nodeapi.AdblockConf) (nodeapi.AdblockState, error) {
	src, err := cleanList(c.Sources, maxAdSources, func(s string) (string, error) {
		s = strings.TrimSpace(s)
		if s == "" {
			return "", nil
		}
		if len(s) > 512 || !(strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) {
			return "", fmt.Errorf("источник — ссылка http:// или https://: %q", s)
		}
		return s, nil
	})
	if err != nil {
		return nodeapi.AdblockState{}, err
	}
	name := func(s string) (string, error) {
		if strings.TrimSpace(s) == "" {
			return "", nil
		}
		n := normAdName(s)
		if n == "" {
			return "", fmt.Errorf("не похоже на имя: %q", s)
		}
		return n, nil
	}
	allow, err := cleanList(c.Allow, maxAdOwn, name)
	if err != nil {
		return nodeapi.AdblockState{}, err
	}
	deny, err := cleanList(c.Deny, maxAdOwn, name)
	if err != nil {
		return nodeapi.AdblockState{}, err
	}
	h.mu.Lock()
	changed := strings.Join(src, "\n") != strings.Join(h.conf.Adblock.Sources, "\n")
	h.conf.Adblock = nodeapi.AdblockConf{Sources: src, Allow: allow, Deny: deny}
	err = h.saveLocked()
	on := h.conf.Config.Adblock
	h.mu.Unlock()
	if err != nil {
		return nodeapi.AdblockState{}, err
	}
	if changed && on && len(src) > 0 {
		// Новые источники — скачать сразу, не ждать суток.
		defer h.startAdUpdate()
	}
	// Свои записи действуют сразу: пересобрать из последних скачанных.
	if err := h.rebuildAd(nil); err != nil {
		return h.adState(), err
	}
	return h.adState(), nil
}

// UpdateAdblock — скачать источники в фоне (панель видит Updating).
func (h *Host) UpdateAdblock() nodeapi.AdblockState {
	h.startAdUpdate()
	return h.adState()
}

func (h *Host) startAdUpdate() bool {
	h.mu.Lock()
	if h.adUpdating || h.stopped {
		h.mu.Unlock()
		return false
	}
	h.adUpdating = true
	h.mu.Unlock()
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.updateAd(h.ctx)
		h.mu.Lock()
		h.adUpdating = false
		h.mu.Unlock()
	}()
	return true
}

// fetchAd — скачать один источник.
func (h *Host) fetchAd(ctx context.Context, url string) ([]string, error) {
	if h.opt.AdFetch != nil {
		rc, err := h.opt.AdFetch(ctx, url)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return parseAdList(io.LimitReader(rc, maxAdSourceBytes)), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ответил %s", resp.Status)
	}
	return parseAdList(io.LimitReader(resp.Body, maxAdSourceBytes)), nil
}

// updateAd — скачать все источники; не скачался ни один — список прежний.
func (h *Host) updateAd(ctx context.Context) {
	h.mu.Lock()
	src := append([]string(nil), h.conf.Adblock.Sources...)
	h.mu.Unlock()
	now := h.opt.Now().Unix()
	if len(src) == 0 {
		h.mu.Lock()
		h.ad.Checked, h.ad.Error = now, "не задан ни один источник"
		h.mu.Unlock()
		h.saveAdMeta()
		return
	}
	var names []string
	var fetched []nodeapi.AdblockSource
	okAny := false
	for _, u := range src {
		list, err := h.fetchAd(ctx, u)
		s := nodeapi.AdblockSource{URL: u, Names: len(list)}
		if err != nil {
			s.Error = shortText(err.Error())
		} else {
			okAny = true
			names = append(names, list...)
		}
		fetched = append(fetched, s)
	}
	h.mu.Lock()
	h.ad.Checked, h.ad.Fetched = now, fetched
	h.mu.Unlock()
	if !okAny {
		h.mu.Lock()
		h.ad.Error = "ни один источник не скачался — раздаётся прежний список"
		h.mu.Unlock()
		h.saveAdMeta()
		h.event(nodeapi.Event{Plugin: nodeapi.PluginAdblock, Error: true, Text: "список блокировки не обновился: ни один источник не скачался"})
		return
	}
	if err := os.MkdirAll(h.adDir(), 0o700); err == nil {
		err = writeAtomic(filepath.Join(h.adDir(), "sources.txt"), []byte(strings.Join(dedup(names), "\n")))
		if err != nil {
			h.event(nodeapi.Event{Plugin: nodeapi.PluginAdblock, Error: true, Text: err.Error()})
		}
	}
	h.mu.Lock()
	h.ad.Updated, h.ad.Error = now, ""
	h.mu.Unlock()
	if err := h.rebuildAd(names); err != nil {
		h.event(nodeapi.Event{Plugin: nodeapi.PluginAdblock, Error: true, Text: err.Error()})
		return
	}
	h.mu.Lock()
	n := h.ad.Names
	h.mu.Unlock()
	h.event(nodeapi.Event{Plugin: nodeapi.PluginAdblock, Text: "список блокировки обновлён: имён — " + strconv.Itoa(n)})
}

func dedup(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, n := range in {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func shortText(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// rebuildAd — итоговый список: скачанное (names; nil — прочитать прежнее)
// + свои «блокировать» − свои «не блокировать».
func (h *Host) rebuildAd(names []string) error {
	if names == nil {
		if b, err := os.ReadFile(filepath.Join(h.adDir(), "sources.txt")); err == nil && len(b) > 0 {
			names = strings.Split(string(b), "\n")
		}
	}
	h.mu.Lock()
	own := h.conf.Adblock
	h.mu.Unlock()
	allow := map[string]bool{}
	for _, a := range own.Allow {
		allow[a] = true
	}
	set := map[string]bool{}
	for _, n := range append(names, own.Deny...) {
		if n != "" && !underAllow(allow, n) {
			set[n] = true
		}
	}
	list := make([]string, 0, len(set))
	for n := range set {
		list = append(list, n)
	}
	sort.Strings(list)
	var b strings.Builder
	b.WriteString("# ssh_tunnel: общий список блокировки сервера\n")
	for _, a := range own.Allow {
		b.WriteString("@@" + a + "\n")
	}
	for _, n := range list {
		b.WriteString(n + "\n")
	}
	data := []byte(b.String())
	sum := sha256.Sum256(data)
	if err := os.MkdirAll(h.adDir(), 0o700); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(h.adDir(), "list.txt"), data); err != nil {
		return err
	}
	h.mu.Lock()
	h.ad.Version, h.ad.Names = hex.EncodeToString(sum[:8]), len(list)
	h.mu.Unlock()
	h.saveAdMeta()
	return nil
}

func (h *Host) saveAdMeta() {
	h.mu.Lock()
	b, _ := json.Marshal(h.ad)
	h.mu.Unlock()
	if os.MkdirAll(h.adDir(), 0o700) == nil {
		_ = writeAtomic(filepath.Join(h.adDir(), "meta.json"), b)
	}
}

// adState — для панели.
func (h *Host) adState() nodeapi.AdblockState {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := nodeapi.AdblockState{AdblockConf: h.conf.Adblock, Updated: h.ad.Updated, Checked: h.ad.Checked,
		Names: h.ad.Names, Error: h.ad.Error, Version: h.ad.Version, Updating: h.adUpdating,
		Fetched: append([]nodeapi.AdblockSource(nil), h.ad.Fetched...)}
	if st.Sources == nil {
		st.Sources = []string{}
	}
	if st.Allow == nil {
		st.Allow = []string{}
	}
	if st.Deny == nil {
		st.Deny = []string{}
	}
	day := h.opt.Now().Add(-24 * time.Hour).Unix()
	for _, t := range h.adPulls {
		if t >= day {
			st.Devices++
		}
	}
	return st
}

// adDue — пора обновить список (раз в сутки, если есть источники).
func (h *Host) adDue() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.conf.Config.Adblock || len(h.conf.Adblock.Sources) == 0 {
		return false
	}
	last := h.ad.Checked
	return h.opt.Now().Unix()-last >= int64(adEvery/time.Second)
}

// serveAdblock — GET /list.
func (h *Host) serveAdblock(w http.ResponseWriter, r *http.Request, nd *netData) {
	if r.URL.Path != "/list" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		writeErr(w, http.StatusNotFound, "нет такого адреса")
		return
	}
	h.mu.Lock()
	h.adPulls[nd.id+"/"+nodeapi.PeerID(callerOf(r).Device)] = h.opt.Now().Unix()
	ver, updated, names := h.ad.Version, h.ad.Updated, h.ad.Names
	h.mu.Unlock()
	if ver == "" {
		writeErr(w, http.StatusNotFound, "на сервере пока нет списка блокировки — задай источники в панели сервера")
		return
	}
	etag := `"` + ver + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Adblock-Updated", strconv.FormatInt(updated, 10))
	w.Header().Set("X-Adblock-Names", strconv.Itoa(names))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	f, err := os.Open(filepath.Join(h.adDir(), "list.txt"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, f)
}

var errAdBusy = errors.New("список уже обновляется")
