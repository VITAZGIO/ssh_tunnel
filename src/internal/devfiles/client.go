package devfiles

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/devnet"
	"sshtunnel/internal/mesh"
)

// Сторона этого устройства: API для страницы (/api/files…) и модуль ядра,
// который держит службу файлов, пока приложение добавлено.

func init() {
	addons.Register("files", func() addons.Module { return &module{} })
	addons.RegisterRoutes("files",
		addons.Route{Path: "/api/files/shares", Methods: "GET, POST",
			Desc: "Мои открытые папки. POST {\"op\":\"add\",\"path\",\"name\"} | {\"op\":\"remove\",\"id\"} | {\"op\":\"write\",\"id\",\"value\":bool}.",
			H:    handleShares},
		addons.Route{Path: "/api/files/devices", Methods: "GET",
			Desc: "Устройства сети, чьи файлы можно смотреть.",
			H:    handleDevices},
		addons.Route{Path: "/api/files/save", Methods: "POST",
			Desc: "Скачать файл другого устройства в «Загрузки» этого {\"peer\",\"s\",\"p\"} (телефон: у WebView нет своих загрузок). С докачкой: прерванное продолжается с места обрыва.",
			H:    handleSave},
		addons.Route{Path: "/api/files/remote", Methods: "GET, POST, PUT",
			Desc: "Файлы другого устройства: ?peer=<адрес в сети>&x=shares|list|get|part|put|op и параметры службы (s, p, off, done, inline). Range передаётся (докачка).",
			H:    handleRemote},
	)
}

type module struct{ srv *server }

var (
	logMu sync.Mutex
	logf  = func(string) {}
)

// SetLog — куда писать, кто и что брал из папок этого устройства.
func SetLog(f func(string)) {
	logMu.Lock()
	logf = f
	logMu.Unlock()
}

func (m *module) Start() error {
	m.srv = newServer(func(s string) {
		logMu.Lock()
		f := logf
		logMu.Unlock()
		f(s)
	})
	return nil
}

func (m *module) Stop() {
	if m.srv != nil {
		m.srv.close()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// suggest — обычные папки этого устройства, которые есть на диске.
func suggest() []string {
	var base string
	var names []string
	if runtime.GOOS == "android" {
		base = "/storage/emulated/0"
		names = []string{"DCIM/Camera", "DCIM", "Pictures", "Download", "Documents", "Movies", "Music"}
	} else {
		base, _ = os.UserHomeDir()
		names = []string{"Desktop", "Documents", "Downloads", "Pictures", "Videos", "Music"}
	}
	out := []string{}
	if base == "" {
		return out
	}
	for _, n := range names {
		p := filepath.Join(base, filepath.FromSlash(n))
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

func handleShares(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Op    string `json:"op"`
			ID    string `json:"id"`
			Path  string `json:"path"`
			Name  string `json:"name"`
			Value bool   `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, map[string]string{"error": "нужен JSON {\"op\"}"})
			return
		}
		var err error
		switch req.Op {
		case "add":
			_, err = AddShare(req.Path, req.Name)
		case "remove":
			err = RemoveShare(req.ID)
		case "write":
			err = SetWrite(req.ID, req.Value)
		default:
			err = errors.New("op: add, remove или write")
		}
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, map[string]any{"shares": Shares(), "suggest": suggest(), "platform": runtime.GOOS})
}

func handleDevices(w http.ResponseWriter, r *http.Request) {
	st := devnet.Status()
	type dev struct {
		Name     string `json:"name"`
		Host     string `json:"host"`
		IP       string `json:"ip"`
		Platform string `json:"platform"`
		Online   bool   `json:"online"`
		Self     bool   `json:"self,omitempty"`
	}
	out := []dev{}
	for _, p := range st.Peers {
		// Сервер — только если на нём есть служба файлов (плагины сервера,
		// ssh_tunnel_node): иначе он в списке был бы пустышкой.
		if p.Platform == "server" && !(p.Online && p.IP.IsValid() && serverHasFiles(p.IP.String())) {
			continue
		}
		out = append(out, dev{p.Name, p.Host, p.IP.String(), p.Platform, p.Online, p.Self})
	}
	writeJSON(w, map[string]any{"state": st.State, "error": st.Error, "devices": out})
}

// serverHasFiles — отвечает ли служба файлов на сервере ip. Ответ
// помнится минуту: список устройств страница спрашивает часто, а лишний
// вызов сервера без плагина — это ошибка в его журнале.
var (
	srvFilesMu sync.Mutex
	srvFiles   = map[string]srvFilesSeen{}
)

type srvFilesSeen struct {
	ok bool
	at time.Time
}

func serverHasFiles(ip string) bool {
	srvFilesMu.Lock()
	s, seen := srvFiles[ip]
	srvFilesMu.Unlock()
	if seen && time.Since(s.at) < time.Minute {
		return s.ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(ip, strconv.Itoa(Port))+"/shares", nil)
	ok := false
	if resp, err := peerClient.Do(req); err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		ok = resp.StatusCode == http.StatusOK
	}
	srvFilesMu.Lock()
	srvFiles[ip] = srvFilesSeen{ok: ok, at: time.Now()}
	srvFilesMu.Unlock()
	return ok
}

var remoteOps = map[string]string{
	"shares": http.MethodGet, "list": http.MethodGet, "get": http.MethodGet,
	"part": http.MethodGet, "put": http.MethodPut, "op": http.MethodPost,
}

// peerClient ходит к службе файлов устройств через сеть устройств.
var peerClient = &http.Client{Transport: &http.Transport{
	DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
		return devnet.DialPeer(addr)
	},
	MaxIdleConnsPerHost:   4,
	IdleConnTimeout:       60 * time.Second,
	ResponseHeaderTimeout: 60 * time.Second,
}}

func handleRemote(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ip, err := netip.ParseAddr(q.Get("peer"))
	if err != nil || !mesh.Range.Contains(ip) {
		writeJSON(w, map[string]string{"error": "нужен адрес устройства в сети (peer)"})
		return
	}
	x := q.Get("x")
	method, ok := remoteOps[x]
	if !ok || method != r.Method {
		writeJSON(w, map[string]string{"error": "x: shares, list, get, part, put или op с нужным методом"})
		return
	}
	rest := url.Values{}
	for _, k := range []string{"s", "p", "off", "done", "inline", "total"} {
		if v := q.Get(k); v != "" {
			rest.Set(k, v)
		}
	}
	u := "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(Port)) + "/" + x + "?" + rest.Encode()
	var body io.Reader
	var tr *transfer
	if r.Method != http.MethodGet {
		body = r.Body
		if x == "put" {
			total, _ := strconv.ParseInt(q.Get("total"), 10, 64)
			tr = track("↑ "+filepath.Base(q.Get("p")), total)
			off, _ := strconv.ParseInt(q.Get("off"), 10, 64)
			tr.add(off)
			body = progressReader{r.Body, tr}
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), method, u, body)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if r.ContentLength > 0 {
		req.ContentLength = r.ContentLength
	}
	for _, h := range []string{"Range", "If-Range", "Content-Type"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := peerClient.Do(req)
	if tr != nil {
		tr.end()
	}
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		writeJSON(w, map[string]string{"error": "устройство не отвечает: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
		"Content-Disposition", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	var src io.Reader = resp.Body
	if x == "get" && q.Get("inline") != "1" {
		total := resp.ContentLength
		dl := track("↓ "+filepath.Base(q.Get("p")), total)
		defer dl.end()
		src = progressReader{resp.Body, dl}
	}
	io.Copy(w, src)
}

// downloadsDir — «Загрузки» этого устройства.
func downloadsDir() string {
	if runtime.GOOS == "android" {
		return "/storage/emulated/0/Download"
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Downloads")
}

// handleSave качает файл в «Загрузки» с докачкой: недокачанное лежит
// рядом как «.имя.sshtunnel-part» и продолжается запросом Range.
func handleSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Peer string `json:"peer"`
		S    string `json:"s"`
		P    string `json:"p"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "нужен JSON {\"peer\",\"s\",\"p\"}"})
		return
	}
	ip, err := netip.ParseAddr(req.Peer)
	if err != nil || !mesh.Range.Contains(ip) {
		writeJSON(w, map[string]string{"error": "нужен адрес устройства в сети (peer)"})
		return
	}
	name := filepath.Base(filepath.FromSlash(cleanRel(req.P)))
	if !validName(name) {
		writeJSON(w, map[string]string{"error": "недопустимое имя файла"})
		return
	}
	dir := downloadsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, map[string]string{"error": "нет доступа к «Загрузкам»: " + err.Error()})
		return
	}
	final := filepath.Join(dir, name)
	part := partOf(final)
	var have int64
	if st, err := os.Stat(part); err == nil {
		have = st.Size()
	}
	u := "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(Port)) + "/get?" +
		url.Values{"s": {req.S}, "p": {req.P}}.Encode()
	hreq, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if have > 0 {
		hreq.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}
	resp, err := peerClient.Do(hreq)
	if err != nil {
		writeJSON(w, map[string]string{"error": "устройство не отвечает: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		flags |= os.O_TRUNC
		have = 0
	case http.StatusRequestedRangeNotSatisfiable:
		// Всё уже скачано в прошлый раз, оборвалось только переименование.
		dst := freeName(final)
		if err := os.Rename(part, dst); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "path": dst})
		return
	default:
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		writeJSON(w, map[string]string{"error": e.Error})
		return
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	tr := track("↓ "+name, have+resp.ContentLength)
	tr.add(have)
	_, err = io.Copy(f, progressReader{resp.Body, tr})
	tr.end()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeJSON(w, map[string]string{"error": "скачивание прервалось, повтори — продолжится с места обрыва"})
		return
	}
	dst := freeName(final)
	if err := os.Rename(part, dst); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "path": dst})
}
