// Package clipshare — дополнительное приложение «Общий буфер обмена» (ТЗ 5,
// п. 17): текст, картинки и файлы до 50 МБ между своими устройствами, только
// по сети устройств (шифрование — как у сети).
//
// Две половины:
//   - ядро (где сеть устройств: окно ПК, служба VPN-версии, телефон) —
//     принимает записи от других устройств на порту Port сети устройств,
//     хранит историю (20 последних), рассылает свои;
//   - наблюдатель буфера (Watch) — там, где есть буфер пользователя: в окне
//     ПК/Linux. Служба Windows работает под SYSTEM и буфера пользователя не
//     видит, поэтому в VPN-версии наблюдатель живёт в процессе окна и
//     говорит с ядром службы через API (WatchRemote). На телефоне буфер в
//     фоне читать нельзя: отправка — «Поделиться» и плитка, приём —
//     SetSink (Kotlin кладёт в буфер сам).
//
// Петли нет: полученное только кладётся в буфер и никуда дальше не
// уходит; а то, что мы сами положили, наблюдатель узнаёт по содержимому и не
// отправляет обратно.
package clipshare

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxBytes — больше через буфер не передаём: для этого «Файлы устройства».
const MaxBytes = 50 << 20

// keepItems — сколько записей в истории; keepBytes — сколько места под их
// содержимое (старые теряют содержимое первыми, сама строка остаётся).
const (
	keepItems = 20
	keepBytes = 200 << 20
)

// Виды записей.
const (
	KindText  = "text"
	KindImage = "image" // PNG
	KindFile  = "file"
)

// Item — запись истории.
type Item struct {
	ID      string `json:"id"`
	Seq     int64  `json:"seq"`
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"` // имя файла
	Mime    string `json:"mime,omitempty"`
	Size    int64  `json:"size"`
	Preview string `json:"preview,omitempty"` // начало текста
	From    string `json:"from"`              // имя устройства-источника
	Out     bool   `json:"out,omitempty"`     // отправлено с этого устройства
	At      int64  `json:"at"`                // unix, секунды
	Sent    int    `json:"sent,omitempty"`    // скольким устройствам ушло
	Gone    bool   `json:"gone,omitempty"`    // содержимое удалено (место)
	Hash    string `json:"hash,omitempty"`
}

type history struct {
	dir string

	mu    sync.Mutex
	items []Item // новые в конце
	seq   int64
	wake  chan struct{} // закрывается при каждой новой записи
}

func openHistory(dir string) *history {
	h := &history{dir: filepath.Join(dir, "clipboard"), wake: make(chan struct{})}
	_ = os.MkdirAll(h.dir, 0o700)
	if b, err := os.ReadFile(filepath.Join(h.dir, "history.json")); err == nil {
		var d struct {
			Items []Item `json:"items"`
			Seq   int64  `json:"seq"`
		}
		if json.Unmarshal(b, &d) == nil {
			h.items, h.seq = d.Items, d.Seq
		}
	}
	return h
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hashOf(kind string, data []byte) string {
	s := sha256.New()
	s.Write([]byte(kind))
	s.Write(data)
	return hex.EncodeToString(s.Sum(nil)[:12])
}

// path — где лежит содержимое записи.
func (h *history) path(id string) string { return filepath.Join(h.dir, id+".bin") }

// has — запись с таким id уже есть (повтор от другого устройства).
func (h *history) has(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, it := range h.items {
		if it.ID == id {
			return true
		}
	}
	return false
}

// add сохраняет запись и её содержимое.
func (h *history) add(it Item, data []byte) (Item, error) {
	if it.ID == "" {
		it.ID = newID()
	}
	if err := os.WriteFile(h.path(it.ID), data, 0o600); err != nil {
		return it, err
	}
	it.Size = int64(len(data))
	it.Hash = hashOf(it.Kind, data)
	if it.At == 0 {
		it.At = time.Now().Unix()
	}
	if it.Kind == KindText {
		p := []rune(string(data[:min(len(data), 2000)]))
		if len(p) > 300 {
			p = p[:300]
		}
		it.Preview = string(p)
	}
	h.mu.Lock()
	h.seq++
	it.Seq = h.seq
	h.items = append(h.items, it)
	h.trimLocked()
	h.saveLocked()
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
	return it, nil
}

func (h *history) setSent(id string, n int) {
	h.mu.Lock()
	for i := range h.items {
		if h.items[i].ID == id {
			h.items[i].Sent = n
		}
	}
	h.saveLocked()
	h.mu.Unlock()
}

// trimLocked — не больше keepItems записей и keepBytes содержимого.
func (h *history) trimLocked() {
	for len(h.items) > keepItems {
		os.Remove(h.path(h.items[0].ID))
		h.items = h.items[1:]
	}
	var total int64
	for i := len(h.items) - 1; i >= 0; i-- {
		if h.items[i].Gone {
			continue
		}
		total += h.items[i].Size
		if total > keepBytes {
			os.Remove(h.path(h.items[i].ID))
			h.items[i].Gone = true
		}
	}
}

func (h *history) saveLocked() {
	b, _ := json.Marshal(map[string]any{"items": h.items, "seq": h.seq})
	tmp := filepath.Join(h.dir, "history.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(h.dir, "history.json"))
	}
}

// list — записи новее after (новые первыми) и номер последней.
func (h *history) list(after int64) ([]Item, int64, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []Item{}
	for i := len(h.items) - 1; i >= 0; i-- {
		if h.items[i].Seq > after {
			out = append(out, h.items[i])
		}
	}
	return out, h.seq, h.wake
}

func (h *history) get(id string) (Item, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, it := range h.items {
		if it.ID == id {
			return it, true
		}
	}
	return Item{}, false
}

// remove — одну запись или все (id == "").
func (h *history) remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	keep := h.items[:0]
	for _, it := range h.items {
		if id == "" || it.ID == id {
			os.Remove(h.path(it.ID))
			continue
		}
		keep = append(keep, it)
	}
	h.items = keep
	h.saveLocked()
}
