package mirror

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
	"time"
)

// Протокол между устройствами — HTTP поверх соединения сети устройств (порт
// Port), как у «Файлов устройства»:
//
//	GET  /folders                          раздаваемые папки
//	GET  /index?f=&since=N[&wait=сек]      изменения после N (долгий опрос)
//	GET  /get?f=&p=&hash=                  содержимое (Range — докачка)
//	GET  /blob?f=&b=                       старая версия (просмотр)
//	GET  /part?f=&p=&hash=                 сколько уже загружено
//	PUT  /put?f=&p=&hash=&off=N            кусок загрузки с места N
//	POST /commit {f,p,hash,base[,size]}    сделать загруженное файлом
//	POST /delete {f,p,base}                удалить
//	POST /resolve {f,path,keep}            решить конфликт
//	GET  /versions?f=[&p=]                 версии и корзина
//	POST /restore {f,blob}                 вернуть версию
//
// Кто пришёл (имя устройства для конфликтов) — из соединения сети устройств,
// подделать нельзя.

// Handler — протокол сервера. who — имя устройства, которое пришло.
func (h *Hub) Handler(who func(*http.Request) string) http.Handler {
	mux := http.NewServeMux()
	q := func(r *http.Request, k string) string { return r.URL.Query().Get(k) }
	mux.HandleFunc("/folders", func(w http.ResponseWriter, r *http.Request) {
		type pub struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		out := []pub{}
		for _, f := range h.Folders() {
			out = append(out, pub{f.ID, f.Name})
		}
		jsonOut(w, 200, out)
	})
	mux.HandleFunc("/index", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(q(r, "since"), 10, 64)
		wait, _ := strconv.Atoi(q(r, "wait"))
		if wait > 60 {
			wait = 60
		}
		rep, err := h.Index(q(r, "f"), since, time.Duration(wait)*time.Second, r.Context().Done())
		if err != nil {
			errOut(w, http.StatusNotFound, err)
			return
		}
		jsonOut(w, 200, rep)
	})
	mux.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) {
		fh, err := h.Open(q(r, "f"), q(r, "p"), q(r, "hash"))
		if err != nil {
			code := http.StatusNotFound
			if errors.Is(err, errChanged) {
				code = http.StatusConflict
			}
			errOut(w, code, err)
			return
		}
		defer fh.Close()
		st, err := fh.Stat()
		if err != nil {
			errOut(w, 500, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", st.ModTime(), fh)
	})
	mux.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		fh, err := h.OpenVersion(q(r, "f"), q(r, "b"))
		if err != nil {
			errOut(w, http.StatusNotFound, err)
			return
		}
		defer fh.Close()
		st, _ := fh.Stat()
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", st.ModTime(), fh)
	})
	mux.HandleFunc("/part", func(w http.ResponseWriter, r *http.Request) {
		n, err := h.PartSize(q(r, "f"), who(r), q(r, "p"), q(r, "hash"))
		if err != nil {
			errOut(w, http.StatusNotFound, err)
			return
		}
		jsonOut(w, 200, map[string]int64{"have": n})
	})
	mux.HandleFunc("/put", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			errOut(w, http.StatusMethodNotAllowed, errors.New("нужен PUT"))
			return
		}
		off, _ := strconv.ParseInt(q(r, "off"), 10, 64)
		if err := h.CheckRoom(q(r, "f"), r.ContentLength); err != nil {
			errOut(w, quotaCode(err, http.StatusNotFound), err)
			return
		}
		n, err := h.WritePart(q(r, "f"), who(r), q(r, "p"), q(r, "hash"), off, r.Body)
		if err != nil {
			code := quotaCode(err, 500)
			if errors.Is(err, errOffset) {
				code = http.StatusConflict
			}
			jsonOut(w, code, map[string]any{"have": n, "error": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]int64{"have": n})
	})
	type body struct {
		F    string `json:"f"`
		P    string `json:"p"`
		Hash string `json:"hash"`
		Base int64  `json:"base"`
		Path string `json:"path"`
		Keep string `json:"keep"`
		Blob string `json:"blob"`
		Size *int64 `json:"size"` // необязательно (ТЗ 13): размер для докачки
	}
	post := func(fn func(r *http.Request, b body) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				errOut(w, http.StatusMethodNotAllowed, errors.New("нужен POST"))
				return
			}
			var b body
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b); err != nil {
				errOut(w, http.StatusBadRequest, err)
				return
			}
			v, err := fn(r, b)
			if err != nil {
				errOut(w, quotaCode(err, 500), err)
				return
			}
			jsonOut(w, 200, v)
		}
	}
	mux.HandleFunc("/commit", post(func(r *http.Request, b body) (any, error) {
		size := int64(-1)
		if b.Size != nil {
			size = *b.Size
		}
		return h.CommitSized(b.F, who(r), b.P, b.Hash, b.Base, size)
	}))
	mux.HandleFunc("/delete", post(func(r *http.Request, b body) (any, error) {
		return h.Delete(b.F, who(r), b.P, b.Base)
	}))
	mux.HandleFunc("/resolve", post(func(r *http.Request, b body) (any, error) {
		return map[string]bool{"ok": true}, h.Resolve(b.F, who(r), b.Path, b.Keep)
	}))
	mux.HandleFunc("/restore", post(func(r *http.Request, b body) (any, error) {
		p, err := h.Restore(b.F, who(r), b.Blob)
		return map[string]string{"path": p}, err
	}))
	mux.HandleFunc("/versions", func(w http.ResponseWriter, r *http.Request) {
		vs, err := h.Versions(q(r, "f"), q(r, "p"))
		if err != nil {
			errOut(w, http.StatusNotFound, err)
			return
		}
		jsonOut(w, 200, vs)
	})
	return mux
}

// quotaCode — 507 для «нет места на сервере», иначе def.
func quotaCode(err error, def int) int {
	if IsQuota(err) {
		return http.StatusInsufficientStorage
	}
	return def
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errOut(w http.ResponseWriter, code int, err error) {
	jsonOut(w, code, map[string]string{"error": err.Error()})
}

// Remote — клиент протокола одного сервера.
type Remote struct {
	Base string // http://адрес:порт
	HC   *http.Client
}

// RemoteError — сервер ответил ошибкой (связь при этом есть).
type RemoteError struct {
	Code int
	Text string
}

func (e *RemoteError) Error() string { return fmt.Sprintf("сервер: %s", e.Text) }

func (rm *Remote) do(ctx context.Context, method, p string, q url.Values, body io.Reader, hdr map[string]string) (*http.Response, error) {
	u := rm.Base + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return rm.HC.Do(req)
}

func (rm *Remote) json(ctx context.Context, method, p string, q url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = strings.NewReader(string(b))
	}
	resp, err := rm.do(ctx, method, p, q, body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &RemoteError{resp.StatusCode, e.Error}
	}
	return json.Unmarshal(b, out)
}

// FolderRef — папка в списке сервера.
type FolderRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Folders — что раздаёт сервер.
func (rm *Remote) Folders(ctx context.Context) ([]FolderRef, error) {
	var out []FolderRef
	err := rm.json(ctx, "GET", "/folders", nil, nil, &out)
	return out, err
}

// Index — изменения после since; wait — долгий опрос.
func (rm *Remote) Index(ctx context.Context, f string, since int64, wait time.Duration) (IndexReply, error) {
	var out IndexReply
	err := rm.json(ctx, "GET", "/index", url.Values{"f": {f}, "since": {strconv.FormatInt(since, 10)},
		"wait": {strconv.Itoa(int(wait / time.Second))}}, nil, &out)
	return out, err
}

func (rm *Remote) post(ctx context.Context, p string, in, out any) error {
	return rm.json(ctx, "POST", p, nil, in, out)
}
