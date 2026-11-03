package clipshare

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/devnet"
)

// ID — приложение в каталоге.
const ID = "clipboard"

func init() {
	addons.Register(ID, func() addons.Module { return &module{} })
	addons.RegisterRoutes(ID,
		addons.Route{Path: "/api/clipboard", Methods: "GET",
			Desc: "Общий буфер: история (новые первыми) ?after=<seq> — только новее, &wait=1 — ждать новой до 25 с. В ответе seq, max (байт), board — следит ли это устройство за своим буфером.",
			H:    handleList},
		addons.Route{Path: "/api/clipboard/data", Methods: "GET",
			Desc: "Содержимое записи ?id= (&dl=1 — скачать файлом).",
			H:    handleData},
		addons.Route{Path: "/api/clipboard/push", Methods: "POST",
			Desc: "Отправить всем устройствам: ?kind=text|image|file&name=&mime=, тело — содержимое (до 50 МБ).",
			H:    handlePush},
		addons.Route{Path: "/api/clipboard/delete", Methods: "POST",
			Desc: "Удалить из истории {\"id\"} или всё {\"all\":true}.",
			H:    handleDelete},
	)
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// module — участие этого устройства: приложение добавлено и выключатель на
// плитке включён. Иначе службы на порту нет и другие сюда не шлют.
type module struct {
	n    *node
	stop func()
}

func (m *module) Start() error {
	m.n = startNode(addons.DataDir())
	nodeMu.Lock()
	running = m.n
	nodeMu.Unlock()
	// Буфер пользователя — тут же, если он у этого процесса есть (окно ПК,
	// Linux с рабочим столом). Служба Windows его не видит — там наблюдает
	// окно (WatchRemote).
	if b := localBoard(); b != nil {
		m.stop = watch(b, localLink{})
	}
	return nil
}

func (m *module) Stop() {
	if m.stop != nil {
		m.stop()
	}
	nodeMu.Lock()
	if running == m.n {
		running = nil
	}
	nodeMu.Unlock()
	m.n.close()
}

// Push — отправить запись с этого устройства (телефон: «Поделиться»,
// плитка). Ответ — сколько устройств приняло.
func Push(kind, name, mime string, data []byte) (int, error) {
	n := current()
	if n == nil {
		return 0, errOff
	}
	it, err := n.push(Item{Kind: kind, Name: name, Mime: mime}, data)
	return it.Sent, err
}

type offErr struct{}

func (offErr) Error() string {
	return "«Общий буфер» не добавлен или выключен"
}

var errOff = offErr{}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func handleList(w http.ResponseWriter, r *http.Request) {
	n := current()
	if n == nil {
		writeJSON(w, map[string]string{"error": errOff.Error()})
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	items, seq, wake := n.h.list(after)
	if len(items) == 0 && r.URL.Query().Get("wait") == "1" {
		select {
		case <-wake:
			items, seq, _ = n.h.list(after)
		case <-time.After(25 * time.Second):
		case <-r.Context().Done():
			return
		}
	}
	writeJSON(w, map[string]any{"items": items, "seq": seq, "max": MaxBytes,
		"board": boardHere(), "mesh": devnet.Status().State})
}

func handleData(w http.ResponseWriter, r *http.Request) {
	n := current()
	if n == nil {
		http.NotFound(w, r)
		return
	}
	it, ok := n.h.get(r.URL.Query().Get("id"))
	if !ok || it.Gone {
		http.NotFound(w, r)
		return
	}
	ct := it.Mime
	switch {
	case it.Kind == KindText:
		ct = "text/plain; charset=utf-8"
	case it.Kind == KindImage:
		ct = "image/png"
	case ct == "":
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("dl") == "1" {
		name := it.Name
		if name == "" {
			name = map[string]string{KindText: "clipboard.txt", KindImage: "clipboard.png"}[it.Kind]
			if name == "" {
				name = "clipboard.bin"
			}
		}
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	}
	http.ServeFile(w, r, n.h.path(it.ID))
}

func handlePush(w http.ResponseWriter, r *http.Request) {
	n := current()
	if n == nil || r.Method != http.MethodPost {
		writeJSON(w, map[string]string{"error": errOff.Error()})
		return
	}
	q := r.URL.Query()
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxBytes+1))
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	kind := q.Get("kind")
	if kind != KindImage && kind != KindFile {
		kind = KindText
	}
	it, err := n.push(Item{Kind: kind, Name: q.Get("name"), Mime: q.Get("mime")}, data)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error(), "tooBig": err == ErrTooBig})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "item": it})
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	n := current()
	var req struct {
		ID  string `json:"id"`
		All bool   `json:"all"`
	}
	if n == nil || json.NewDecoder(r.Body).Decode(&req) != nil || (req.ID == "" && !req.All) {
		writeJSON(w, map[string]string{"error": "нужен JSON {\"id\"} или {\"all\":true}"})
		return
	}
	n.h.remove(req.ID)
	writeJSON(w, map[string]any{"ok": true})
}
