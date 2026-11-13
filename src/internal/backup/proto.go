package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Port — служба резервных копий на сервере сети (рядом с 47835–47837
// других плагинов сервера). На устройствах её нет: копии кладутся только
// на сервер.
const Port = 47838

// Протокол (HTTP поверх соединения сети устройств; кто пришёл — от meshd):
//
//	GET  /sets                                   копии: свои и того же пользователя
//	POST /have {"hashes":[…]}                    → {"missing":[…]}
//	GET  /part?hash=                             → {"have":N}
//	PUT  /put?hash=&off=&size=&set=&keep=        кусок содержимого → {"have":N}
//	POST /commit CommitReq                       → Version
//	GET  /versions?device=&set=                  → [Version]
//	GET  /manifest?device=&set=&v=               → Manifest
//	GET  /blob?hash=                             содержимое (Range — докачка)
//	POST /delete {"device","set","version"}      убрать копию или версию
//
// Ошибки — {"error":"…"}; 507 — места нет, 409 — файл менялся при чтении.

type callerKey struct{}

// WithCaller — кто пришёл (хост плагинов кладёт это в контекст запроса).
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

func callerOf(r *http.Request) (Caller, bool) {
	c, ok := r.Context().Value(callerKey{}).(Caller)
	return c, ok && c.Device != ""
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errOut(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	var full *ErrFull
	switch {
	case errors.As(err, &full):
		code = http.StatusInsufficientStorage
	case errors.Is(err, ErrChanged), errors.Is(err, errOffset):
		code = http.StatusConflict
	}
	jsonOut(w, code, map[string]string{"error": err.Error()})
}

// Handler — протокол сервера.
func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	q := func(r *http.Request, k string) string { return r.URL.Query().Get(k) }
	who := func(h func(w http.ResponseWriter, r *http.Request, c Caller)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c, ok := callerOf(r)
			if !ok {
				jsonOut(w, http.StatusForbidden, map[string]string{"error": "неизвестное устройство"})
				return
			}
			h(w, r, c)
		}
	}
	mux.HandleFunc("/sets", who(func(w http.ResponseWriter, r *http.Request, c Caller) {
		jsonOut(w, 200, map[string]any{"sets": s.Sets(c)})
	}))
	mux.HandleFunc("/have", who(func(w http.ResponseWriter, r *http.Request, c Caller) {
		var in struct {
			Hashes []string `json:"hashes"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&in); err != nil {
			errOut(w, err)
			return
		}
		jsonOut(w, 200, map[string]any{"missing": s.Missing(c, in.Hashes)})
	}))
	mux.HandleFunc("/part", who(func(w http.ResponseWriter, r *http.Request, c Caller) {
		jsonOut(w, 200, map[string]int64{"have": s.PartSize(c, q(r, "hash"))})
	}))
	mux.HandleFunc("/put", who(func(w http.ResponseWriter, r *http.Request, c Caller) {
		if r.Method != http.MethodPut {
			jsonOut(w, http.StatusMethodNotAllowed, map[string]string{"error": "нужен PUT"})
			return
		}
		off, _ := strconv.ParseInt(q(r, "off"), 10, 64)
		size, err := strconv.ParseInt(q(r, "size"), 10, 64)
		if err != nil {
			errOut(w, errors.New("нет размера"))
			return
		}
		keep, _ := strconv.Atoi(q(r, "keep"))
		n, err := s.Put(c, q(r, "hash"), off, size, q(r, "set"), keep, r.Body)
		if err != nil {
			var full *ErrFull
			code := http.StatusInternalServerError
			switch {
			case errors.As(err, &full):
				code = http.StatusInsufficientStorage
			case errors.Is(err, ErrChanged), errors.Is(err, errOffset):
				code = http.StatusConflict
			}
			jsonOut(w, code, map[string]any{"have": n, "error": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]int64{"have": n})
	}))
	mux.HandleFunc("/commit", who(func(w http.ResponseWriter, r *http.Request, c Caller) {
		var req CommitReq
		if err := json.NewDecoder(io.LimitReader(r.Body, 256<<20)).Decode(&req); err != nil {
			errOut(w, err)
			return
		}
		v, err := s.Commit(c, req)
		if err != nil {
			errOut(w, err)
			return
		}
		jsonOut(w, 200, v)
	}))
	mux.HandleFunc("/versions", who(func(w http.ResponseWriter, r *http.Request, c Caller) {
		vs, err := s.Versions(c, q(r, "device"), q(r, "set"))
		if err != nil {
			errOut(w, err)
			return
		}
		jsonOut(w, 200, vs)
	}))
	mux.HandleFunc("/manifest", who(func(w http.ResponseWriter, r *http.Request, c Caller) {
		m, err := s.Manifest(c, q(r, "device"), q(r, "set"), q(r, "v"))
		if err != nil {
			errOut(w, err)
			return
		}
		jsonOut(w, 200, m)
	}))
	mux.HandleFunc("/blob", who(func(w http.ResponseWriter, r *http.Request, c Caller) {
		f, err := s.OpenBlob(c, q(r, "hash"))
		if err != nil {
			jsonOut(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", st.ModTime(), f)
	}))
	mux.HandleFunc("/delete", who(func(w http.ResponseWriter, r *http.Request, c Caller) {
		var in struct {
			Device  string `json:"device"`
			Set     string `json:"set"`
			Version string `json:"version"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
			errOut(w, err)
			return
		}
		if in.Device == "" {
			in.Device = c.Device
		}
		if err := s.Delete(c, in.Device, in.Set, in.Version); err != nil {
			errOut(w, err)
			return
		}
		jsonOut(w, 200, map[string]bool{"ok": true})
	}))
	return mux
}

// ---------- клиент ----------

// Remote — клиент протокола одного сервера.
type Remote struct {
	Base string // http://адрес:порт
	HC   *http.Client
}

// RemoteError — сервер ответил ошибкой (связь есть).
type RemoteError struct {
	Code int
	Text string
}

func (e *RemoteError) Error() string { return "сервер: " + e.Text }

// Full — места на сервере нет.
func (e *RemoteError) Full() bool { return e.Code == http.StatusInsufficientStorage }

func (rm *Remote) do(ctx context.Context, method, p string, q url.Values, body io.Reader, size int64) (*http.Response, error) {
	u := rm.Base + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 && body != nil {
		req.ContentLength = size
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return rm.HC.Do(req)
}

func (rm *Remote) json(ctx context.Context, method, p string, q url.Values, in, out any) error {
	var body io.Reader
	size := int64(-1)
	if in != nil {
		b, _ := json.Marshal(in)
		body, size = strings.NewReader(string(b)), int64(len(b))
	}
	resp, err := rm.do(ctx, method, p, q, body, size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return remoteErr(resp.StatusCode, b, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func remoteErr(code int, b []byte, status string) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	if e.Error == "" {
		e.Error = status
	}
	return &RemoteError{code, e.Error}
}

// Sets — копии на сервере (свои и того же пользователя).
func (rm *Remote) Sets(ctx context.Context) ([]SetInfo, error) {
	var out struct {
		Sets []SetInfo `json:"sets"`
	}
	err := rm.json(ctx, "GET", "/sets", nil, nil, &out)
	return out.Sets, err
}

// Missing — каких содержимых нет на сервере.
func (rm *Remote) Missing(ctx context.Context, hashes []string) ([]string, error) {
	var out struct {
		Missing []string `json:"missing"`
	}
	err := rm.json(ctx, "POST", "/have", nil, map[string]any{"hashes": hashes}, &out)
	return out.Missing, err
}

// Part — сколько уже загружено.
func (rm *Remote) Part(ctx context.Context, hash string) (int64, error) {
	var out struct {
		Have int64 `json:"have"`
	}
	err := rm.json(ctx, "GET", "/part", url.Values{"hash": {hash}}, nil, &out)
	return out.Have, err
}

// Put — кусок содержимого с места off.
func (rm *Remote) Put(ctx context.Context, hash string, off, size int64, set string, keep int, body io.Reader, n int64) (int64, error) {
	q := url.Values{"hash": {hash}, "off": {strconv.FormatInt(off, 10)}, "size": {strconv.FormatInt(size, 10)},
		"set": {set}, "keep": {strconv.Itoa(keep)}}
	resp, err := rm.do(ctx, "PUT", "/put", q, body, n)
	if err != nil {
		return off, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Have int64 `json:"have"`
	}
	_ = json.Unmarshal(b, &out)
	if resp.StatusCode != 200 {
		return out.Have, remoteErr(resp.StatusCode, b, resp.Status)
	}
	return out.Have, nil
}

// Commit — записать версию.
func (rm *Remote) Commit(ctx context.Context, req CommitReq) (Version, error) {
	var v Version
	err := rm.json(ctx, "POST", "/commit", nil, req, &v)
	return v, err
}

// Versions — версии копии.
func (rm *Remote) Versions(ctx context.Context, device, set string) ([]Version, error) {
	var out []Version
	err := rm.json(ctx, "GET", "/versions", url.Values{"device": {device}, "set": {set}}, nil, &out)
	return out, err
}

// Manifest — файлы версии.
func (rm *Remote) Manifest(ctx context.Context, device, set, v string) (Manifest, error) {
	var m Manifest
	err := rm.json(ctx, "GET", "/manifest", url.Values{"device": {device}, "set": {set}, "v": {v}}, nil, &m)
	return m, err
}

// Blob — содержимое с места off (докачка).
func (rm *Remote) Blob(ctx context.Context, hash string, off int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rm.Base+"/blob?hash="+url.QueryEscape(hash), nil)
	if err != nil {
		return nil, err
	}
	if off > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
	}
	resp, err := rm.HC.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != http.StatusPartialContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, remoteErr(resp.StatusCode, b, resp.Status)
	}
	if off > 0 && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, errors.New("сервер не умеет докачку")
	}
	return resp.Body, nil
}

// Delete — убрать копию (version == "") или версию.
func (rm *Remote) Delete(ctx context.Context, device, set, version string) error {
	return rm.json(ctx, "POST", "/delete", nil, map[string]string{"device": device, "set": set, "version": version}, nil)
}
