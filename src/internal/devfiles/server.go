package devfiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/mesh"
)

// Служба файлов — HTTP поверх соединений сети устройств. Протокол между
// устройствами (не путать с /api/files этого устройства):
//
//	GET  /shares                         открытые папки
//	GET  /list?s=<id>&p=<путь>           содержимое папки
//	GET  /get?s=&p=[&inline=1]           файл (Range — докачка)
//	GET  /part?s=&p=                     сколько уже загружено (докачка загрузки)
//	PUT  /put?s=&p=&off=N[&done=1]       загрузка куском с места N
//	POST /op {"op":"mkdir|rename|delete","s","p","to","recursive"}
//
// Кто звонит — mesh.Caller из контекста; доступ решает только «видна» и
// «можно менять» папки.

type callerKey struct{}

// Entry — файл или папка в списке.
type Entry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix, секунды
}

// meshListener превращает соединения службы в net.Listener для http.Server.
type meshListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

type callerConn struct {
	net.Conn
	from mesh.Caller
}

func (l *meshListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *meshListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *meshListener) Addr() net.Addr { return &net.TCPAddr{Port: Port} }

// server — работающая служба.
type server struct {
	ln  *meshListener
	srv *http.Server
	log func(string)
	// src — открытые папки (на устройстве — Shares, у плагинов сервера —
	// своя папка каждой сети).
	src func() []Share
}

// NewHandler — протокол службы файлов отдельно от программы (плагины на
// сервере, ssh_tunnel_node): папки даёт src, кто пришёл — WithCaller в
// контексте запроса. Поведение то же, что у службы на устройстве.
func NewHandler(src func() []Share, log func(string)) http.Handler {
	if log == nil {
		log = func(string) {}
	}
	return (&server{log: log, src: src}).mux()
}

// WithCaller кладёт в контекст, какое устройство сети пришло.
func WithCaller(ctx context.Context, c mesh.Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

func (s *server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares", s.shares)
	mux.HandleFunc("/list", s.list)
	mux.HandleFunc("/get", s.get)
	mux.HandleFunc("/part", s.part)
	mux.HandleFunc("/put", s.put)
	mux.HandleFunc("/op", s.op)
	return mux
}

func newServer(log func(string)) *server {
	s := &server{ln: &meshListener{ch: make(chan net.Conn), done: make(chan struct{})}, log: log, src: Shares}
	mux := s.mux()
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 20 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if cc, ok := c.(callerConn); ok {
				return context.WithValue(ctx, callerKey{}, cc.from)
			}
			return ctx
		}}
	go s.srv.Serve(s.ln)
	mesh.Serve(Port, func(c net.Conn, from mesh.Caller) {
		select {
		case s.ln.ch <- callerConn{c, from}:
		case <-s.ln.done:
			c.Close()
		}
	})
	return s
}

func (s *server) close() {
	mesh.Serve(Port, nil)
	s.srv.Close()
	s.ln.Close()
}

func caller(r *http.Request) mesh.Caller {
	c, _ := r.Context().Value(callerKey{}).(mesh.Caller)
	return c
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errOut(w http.ResponseWriter, code int, text string) {
	jsonOut(w, code, map[string]string{"error": text})
}

// findShare — открытая папка по id.
func (s *server) findShare(id string) (Share, bool) {
	for _, sh := range s.src() {
		if sh.ID == id {
			return sh, true
		}
	}
	return Share{}, false
}

// target — папка и настоящий путь из ?s=&p=.
func (s *server) target(w http.ResponseWriter, r *http.Request) (Share, string, bool) {
	q := r.URL.Query()
	sh, ok := s.findShare(q.Get("s"))
	if !ok {
		errOut(w, http.StatusNotFound, "папка больше не открыта")
		return sh, "", false
	}
	full, err := resolve(sh, q.Get("p"))
	if err != nil {
		errOut(w, http.StatusForbidden, err.Error())
		return sh, "", false
	}
	return sh, full, true
}

func (s *server) shares(w http.ResponseWriter, r *http.Request) {
	type pub struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Write bool   `json:"write"`
	}
	out := []pub{}
	for _, sh := range s.src() {
		out = append(out, pub{sh.ID, sh.Name, sh.Write})
	}
	jsonOut(w, 200, map[string]any{"shares": out})
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	sh, full, ok := s.target(w, r)
	if !ok {
		return
	}
	des, err := os.ReadDir(full)
	if err != nil {
		errOut(w, http.StatusNotFound, "не удалось открыть папку")
		return
	}
	out := []Entry{}
	for _, d := range des {
		info, err := d.Info()
		if err != nil {
			continue
		}
		e := Entry{Name: d.Name(), Dir: info.IsDir(), MTime: info.ModTime().Unix()}
		if info.Mode()&os.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(full, d.Name())); err == nil {
				e.Dir = st.IsDir()
				info = st
			}
		}
		if !e.Dir {
			e.Size = info.Size()
		}
		if strings.HasSuffix(e.Name, partSuffix) {
			continue // недогруженное
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	jsonOut(w, 200, map[string]any{"entries": out, "write": sh.Write})
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	_, full, ok := s.target(w, r)
	if !ok {
		return
	}
	f, err := os.Open(full)
	if err != nil {
		errOut(w, http.StatusNotFound, "файла нет")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		errOut(w, http.StatusBadRequest, "это папка")
		return
	}
	name := filepath.Base(full)
	disp := "attachment"
	if r.URL.Query().Get("inline") == "1" {
		disp = "inline"
	}
	w.Header().Set("Content-Disposition", disp+"; filename*=UTF-8''"+urlEscape(name))
	s.log(fmt.Sprintf("Файлы устройства: %s.mesh скачивает %s", caller(r).Host, name))
	tr := track("↑ "+name, st.Size())
	defer tr.end()
	http.ServeContent(w, r, name, st.ModTime(), progressFile{f, tr})
}

// partSuffix — недогруженный файл (докачка загрузки): «.имя.sshtunnel-part».
const partSuffix = ".sshtunnel-part"

func partOf(full string) string {
	return filepath.Join(filepath.Dir(full), "."+filepath.Base(full)+partSuffix)
}

func needWrite(w http.ResponseWriter, sh Share) bool {
	if !sh.Write {
		errOut(w, http.StatusForbidden, "в этой папке можно только смотреть: на том устройстве не стоит «можно менять»")
		return false
	}
	return true
}

func (s *server) part(w http.ResponseWriter, r *http.Request) {
	sh, full, ok := s.target(w, r)
	if !ok || !needWrite(w, sh) {
		return
	}
	var size int64
	if st, err := os.Stat(partOf(full)); err == nil {
		size = st.Size()
	}
	jsonOut(w, 200, map[string]any{"size": size})
}

func (s *server) put(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		errOut(w, http.StatusMethodNotAllowed, "нужен PUT")
		return
	}
	sh, full, ok := s.target(w, r)
	if !ok || !needWrite(w, sh) {
		return
	}
	if !validName(filepath.Base(full)) {
		errOut(w, http.StatusBadRequest, "недопустимое имя файла")
		return
	}
	off, _ := strconv.ParseInt(r.URL.Query().Get("off"), 10, 64)
	part := partOf(full)
	var have int64
	if st, err := os.Stat(part); err == nil {
		have = st.Size()
	}
	if off != have {
		jsonOut(w, http.StatusConflict, map[string]any{"error": "кусок не с того места", "size": have})
		return
	}
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		errOut(w, http.StatusForbidden, "не удалось записать: "+err.Error())
		return
	}
	total, _ := strconv.ParseInt(r.URL.Query().Get("total"), 10, 64)
	tr := track("↓ "+filepath.Base(full), total)
	tr.add(have)
	_, err = io.Copy(f, progressReader{r.Body, tr})
	tr.end()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		errOut(w, http.StatusInternalServerError, "загрузка прервалась: "+err.Error())
		return
	}
	if r.URL.Query().Get("done") != "1" {
		st, _ := os.Stat(part)
		jsonOut(w, 200, map[string]any{"size": st.Size()})
		return
	}
	dst := freeName(full)
	if err := os.Rename(part, dst); err != nil {
		errOut(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log(fmt.Sprintf("Файлы устройства: %s.mesh загрузил %s", caller(r).Host, filepath.Base(dst)))
	jsonOut(w, 200, map[string]any{"ok": true, "name": filepath.Base(dst)})
}

// freeName — путь, который ещё не занят: «фото.jpg» → «фото (2).jpg».
func freeName(full string) string {
	if _, err := os.Lstat(full); errors.Is(err, os.ErrNotExist) {
		return full
	}
	ext := filepath.Ext(full)
	base := strings.TrimSuffix(full, ext)
	for i := 2; ; i++ {
		p := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
	}
}

func (s *server) op(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errOut(w, http.StatusMethodNotAllowed, "нужен POST")
		return
	}
	var req struct {
		Op        string `json:"op"`
		S         string `json:"s"`
		P         string `json:"p"`
		To        string `json:"to"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		errOut(w, http.StatusBadRequest, "нужен JSON")
		return
	}
	sh, ok := s.findShare(req.S)
	if !ok {
		errOut(w, http.StatusNotFound, "папка больше не открыта")
		return
	}
	if !needWrite(w, sh) {
		return
	}
	full, err := resolve(sh, req.P)
	if err != nil {
		errOut(w, http.StatusForbidden, err.Error())
		return
	}
	if cleanRel(req.P) == "/" && req.Op != "mkdir" {
		errOut(w, http.StatusForbidden, "саму открытую папку так не изменить")
		return
	}
	who := caller(r).Host
	switch req.Op {
	case "mkdir":
		if !validName(req.To) {
			errOut(w, http.StatusBadRequest, "недопустимое имя")
			return
		}
		err = os.Mkdir(filepath.Join(full, req.To), 0o755)
	case "rename":
		if !validName(req.To) {
			errOut(w, http.StatusBadRequest, "недопустимое имя")
			return
		}
		dst := filepath.Join(filepath.Dir(full), req.To)
		if _, e := os.Lstat(dst); e == nil {
			errOut(w, http.StatusConflict, "такое имя уже есть")
			return
		}
		err = os.Rename(full, dst)
	case "delete":
		st, e := os.Lstat(full)
		if e != nil {
			errOut(w, http.StatusNotFound, "уже нет")
			return
		}
		if st.IsDir() && !req.Recursive {
			if des, _ := os.ReadDir(full); len(des) > 0 {
				// Как на vitazgio.ru: непустую папку с первого раза не удаляем.
				jsonOut(w, http.StatusConflict, map[string]any{"error": "папка не пустая", "needsRecursive": true})
				return
			}
		}
		err = os.RemoveAll(full)
	default:
		errOut(w, http.StatusBadRequest, "op: mkdir, rename или delete")
		return
	}
	if err != nil {
		errOut(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log(fmt.Sprintf("Файлы устройства: %s.mesh — %s %s", who, req.Op, filepath.Base(full)))
	jsonOut(w, 200, map[string]any{"ok": true})
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
