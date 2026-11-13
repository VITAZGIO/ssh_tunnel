package clipshare

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Общий буфер на сервере (ТЗ 9): сервер — участник сети устройств, который
// всегда на связи. Он хранит последние keepItems записей сети (те же
// пределы, что на устройствах: MaxBytes на запись, keepBytes на всё) и
// отдаёт устройству то, что оно пропустило, пока было не в сети.
//
// Сервер сам устройствам не звонит (meshd не даёт серверу начинать
// соединения), поэтому устройство забирает пропущенное само:
//
//	POST /clip?id=&kind=&name=&mime=   устройство кладёт свою запись (как
//	                                    другим устройствам)
//	GET  /missed                        что пропущено (старые первыми)
//	GET  /data?id=                      содержимое записи
//	POST /ack?seq=N                     всё до N получено
//
// «Пропущено» — записи после отметки этого устройства (ack), не от него
// самого и не старше Keep. Отметка — на каждое устройство (id в сети,
// подделать нельзя). Устройство, которое сервер видит впервые, получает всю
// хранящуюся историю: именно это нужно телефону, который был выключен,
// пока на ПК что-то скопировали.

// ServerCaller — устройство, которое пришло к буферу сервера.
type ServerCaller struct {
	Device string // id устройства в сети
	Name   string // имя для людей
}

type serverCallerKey struct{}

// WithServerCaller кладёт в контекст, кто пришёл.
func WithServerCaller(ctx context.Context, c ServerCaller) context.Context {
	return context.WithValue(ctx, serverCallerKey{}, c)
}

func serverCaller(r *http.Request) ServerCaller {
	c, _ := r.Context().Value(serverCallerKey{}).(ServerCaller)
	return c
}

// ServerKeep — сколько сервер хранит запись буфера по умолчанию.
const ServerKeep = 24 * time.Hour

// ServerBoard — буфер одной сети на сервере.
type ServerBoard struct {
	h    *history
	keep time.Duration
	now  func() time.Time

	mu      sync.Mutex
	cursors map[string]int64  // id устройства → последняя полученная запись
	senders map[string]string // id записи → id устройства-источника
	last    time.Time         // последнее обращение устройства
}

type boardState struct {
	Cursors map[string]int64  `json:"cursors"`
	Senders map[string]string `json:"senders"`
}

// OpenServerBoard — буфер сети в папке dir (там будет clipboard/).
// keep <= 0 — ServerKeep.
func OpenServerBoard(dir string, keep time.Duration) *ServerBoard {
	if keep <= 0 {
		keep = ServerKeep
	}
	b := &ServerBoard{h: openHistory(dir), keep: keep, now: time.Now,
		cursors: map[string]int64{}, senders: map[string]string{}}
	var st boardState
	if data, err := os.ReadFile(b.statePath()); err == nil && json.Unmarshal(data, &st) == nil {
		if st.Cursors != nil {
			b.cursors = st.Cursors
		}
		if st.Senders != nil {
			b.senders = st.Senders
		}
	}
	b.Prune()
	return b
}

func (b *ServerBoard) statePath() string { return filepath.Join(b.h.dir, "devices.json") }

func (b *ServerBoard) saveLocked() {
	data, _ := json.Marshal(boardState{Cursors: b.cursors, Senders: b.senders})
	tmp := b.statePath() + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, b.statePath())
	}
}

// Prune убирает записи старше keep (и их содержимое с диска).
func (b *ServerBoard) Prune() {
	edge := b.now().Add(-b.keep).Unix()
	b.h.mu.Lock()
	keep := b.h.items[:0]
	for _, it := range b.h.items {
		if it.At < edge {
			os.Remove(b.h.path(it.ID))
			continue
		}
		keep = append(keep, it)
	}
	b.h.items = keep
	b.h.saveLocked()
	ids := map[string]bool{}
	for _, it := range b.h.items {
		ids[it.ID] = true
	}
	b.h.mu.Unlock()
	b.mu.Lock()
	for id := range b.senders {
		if !ids[id] {
			delete(b.senders, id)
		}
	}
	b.saveLocked()
	b.mu.Unlock()
}

// Clear — удалить всю историю (кнопка в панели).
func (b *ServerBoard) Clear() {
	b.h.remove("")
	b.mu.Lock()
	b.senders = map[string]string{}
	b.saveLocked()
	b.mu.Unlock()
}

// BoardStats — для панели.
type BoardStats struct {
	Items      int   `json:"items"`
	Bytes      int64 `json:"bytes"`
	Devices    int   `json:"devices"`
	LastActive int64 `json:"lastActive,omitempty"` // unix
}

// Stats — сколько записей и места.
func (b *ServerBoard) Stats() BoardStats {
	items, _, _ := b.h.list(0)
	var st BoardStats
	for _, it := range items {
		st.Items++
		if !it.Gone {
			st.Bytes += it.Size
		}
	}
	b.mu.Lock()
	st.Devices = len(b.cursors)
	if !b.last.IsZero() {
		st.LastActive = b.last.Unix()
	}
	b.mu.Unlock()
	return st
}

func (b *ServerBoard) touch() {
	b.mu.Lock()
	b.last = b.now()
	b.mu.Unlock()
}

// Handler — протокол буфера сервера.
func (b *ServerBoard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/clip", b.receive)
	mux.HandleFunc("/missed", b.missed)
	mux.HandleFunc("/data", b.data)
	mux.HandleFunc("/ack", b.ack)
	return mux
}

func (b *ServerBoard) receive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	who := serverCaller(r)
	if who.Device == "" {
		http.Error(w, "кто пришёл — неизвестно", http.StatusForbidden)
		return
	}
	b.touch()
	q := r.URL.Query()
	it := Item{ID: q.Get("id"), Kind: q.Get("kind"), Name: q.Get("name"), Mime: q.Get("mime"), From: who.Name}
	switch it.Kind {
	case KindText, KindImage, KindFile:
	default:
		http.Error(w, "kind", http.StatusBadRequest)
		return
	}
	if len(it.ID) > 64 || len(it.Name) > 255 || len(it.Mime) > 128 {
		http.Error(w, "слишком длинные поля", http.StatusBadRequest)
		return
	}
	if it.ID != "" && b.h.has(it.ID) {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.ContentLength > MaxBytes {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxBytes+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(data) > MaxBytes {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	if len(data) == 0 {
		http.Error(w, "пусто", http.StatusBadRequest)
		return
	}
	it.At = b.now().Unix()
	it, err = b.h.add(it, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	b.mu.Lock()
	b.senders[it.ID] = who.Device
	b.saveLocked()
	b.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (b *ServerBoard) missed(w http.ResponseWriter, r *http.Request) {
	who := serverCaller(r)
	if who.Device == "" {
		http.Error(w, "кто пришёл — неизвестно", http.StatusForbidden)
		return
	}
	b.touch()
	edge := b.now().Add(-b.keep).Unix()
	items, seq, _ := b.h.list(0)
	b.mu.Lock()
	cur, known := b.cursors[who.Device]
	if !known {
		// Устройство видим впервые — отдаём всю хранящуюся историю (в
		// пределах Keep), кроме его же записей: это и есть «пропущенное,
		// пока был не в сети» (его скопировали на ПК, пока телефон был
		// выключен, — телефон должен получить это при первом появлении).
		b.cursors[who.Device] = 0
		b.saveLocked()
		cur = 0
	}
	out := []Item{}
	for i := len(items) - 1; i >= 0; i-- { // list — новые первыми
		it := items[i]
		if it.Seq <= cur || it.Gone || it.At < edge || b.senders[it.ID] == who.Device {
			continue
		}
		it.Out, it.Sent = false, 0
		out = append(out, it)
	}
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"items": out, "seq": seq})
}

func (b *ServerBoard) data(w http.ResponseWriter, r *http.Request) {
	who := serverCaller(r)
	if who.Device == "" {
		http.Error(w, "кто пришёл — неизвестно", http.StatusForbidden)
		return
	}
	it, ok := b.h.get(r.URL.Query().Get("id"))
	if !ok || it.Gone {
		http.Error(w, "нет такой записи", http.StatusNotFound)
		return
	}
	f, err := os.Open(b.h.path(it.ID))
	if err != nil {
		http.Error(w, "нет содержимого", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, f)
}

func (b *ServerBoard) ack(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	who := serverCaller(r)
	if who.Device == "" {
		http.Error(w, "кто пришёл — неизвестно", http.StatusForbidden)
		return
	}
	n, err := strconv.ParseInt(r.URL.Query().Get("seq"), 10, 64)
	if err != nil || n < 0 {
		http.Error(w, "seq", http.StatusBadRequest)
		return
	}
	_, last, _ := b.h.list(0)
	if n > last {
		n = last
	}
	b.mu.Lock()
	if n > b.cursors[who.Device] {
		b.cursors[who.Device] = n
		b.saveLocked()
	}
	b.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}
