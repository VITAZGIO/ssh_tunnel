package clipshare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"sshtunnel/internal/devnet"
	"sshtunnel/internal/mesh"
)

// Port — порт буфера в сети устройств (как у «Файлов устройства», настоящий
// порт не занимает). Нет службы на этом порту — устройство не участвует.
const Port = 47836

// Протокол между устройствами:
//
//	POST /clip?id=&kind=text|image|file&name=&mime=   тело — содержимое
//
// Отвечает 200 — принято (или уже было), 413 — слишком большое.

// node — работающая половина «ядро»: история, приём, рассылка.
type node struct {
	h   *history
	srv *http.Server
	ln  *chanListener
}

var (
	nodeMu  sync.Mutex
	running *node

	sinkMu sync.Mutex
	sink   func(it Item, path string)
)

// SetSink — кто кладёт полученное в буфер этого устройства, если
// наблюдателя нет (телефон: Kotlin). path — файл с содержимым.
func SetSink(f func(it Item, path string)) {
	sinkMu.Lock()
	sink = f
	sinkMu.Unlock()
}

func current() *node {
	nodeMu.Lock()
	defer nodeMu.Unlock()
	return running
}

type chanListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *chanListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{Port: Port} }

type fromKey struct{}

type callerConn struct {
	net.Conn
	from mesh.Caller
}

func startNode(dir string) *node {
	n := &node{h: openHistory(dir), ln: &chanListener{ch: make(chan net.Conn), done: make(chan struct{})}}
	mux := http.NewServeMux()
	mux.HandleFunc("/clip", n.receive)
	n.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 20 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if cc, ok := c.(callerConn); ok {
				return context.WithValue(ctx, fromKey{}, cc.from)
			}
			return ctx
		}}
	go n.srv.Serve(n.ln)
	mesh.Serve(Port, func(c net.Conn, from mesh.Caller) {
		select {
		case n.ln.ch <- callerConn{c, from}:
		case <-n.ln.done:
			c.Close()
		}
	})
	return n
}

func (n *node) close() {
	mesh.Serve(Port, nil)
	n.srv.Close()
	n.ln.Close()
}

// peerName — имя устройства по адресу сети устройств.
func peerName(ip, fallback string) string {
	for _, p := range devnet.Status().Peers {
		if p.IP.String() == ip {
			if p.Name != "" {
				return p.Name
			}
			return p.Host
		}
	}
	return fallback
}

func (n *node) receive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	from, _ := r.Context().Value(fromKey{}).(mesh.Caller)
	it := Item{ID: q.Get("id"), Kind: q.Get("kind"), Name: q.Get("name"), Mime: q.Get("mime"),
		From: peerName(from.IP, from.Host)}
	switch it.Kind {
	case KindText, KindImage, KindFile:
	default:
		http.Error(w, "kind", http.StatusBadRequest)
		return
	}
	if it.ID != "" && n.h.has(it.ID) {
		w.WriteHeader(http.StatusOK)
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
	it, err = n.h.add(it, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sinkMu.Lock()
	f := sink
	sinkMu.Unlock()
	if f != nil {
		go f(it, n.h.path(it.ID))
	}
	w.WriteHeader(http.StatusOK)
}

var peerClient = &http.Client{
	Timeout: 5 * time.Minute,
	Transport: &http.Transport{
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			return devnet.DialPeer(addr)
		},
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     30 * time.Second,
	},
}

// ErrTooBig — больше MaxBytes.
var ErrTooBig = fmt.Errorf("больше %d МБ — для этого «Файлы устройства»", MaxBytes>>20)

// push — своя запись: в историю и всем устройствам сети, кто в сети.
func (n *node) push(it Item, data []byte) (Item, error) {
	if len(data) > MaxBytes {
		return it, ErrTooBig
	}
	if len(data) == 0 {
		return it, errors.New("пусто")
	}
	st := devnet.Status()
	if st.State == "off" || st.State == "stopped" {
		return it, devnet.ErrOff
	}
	it.Out = true
	it.From = st.Self.Name
	if it.From == "" {
		it.From = st.Self.Host
	}
	it, err := n.h.add(it, data)
	if err != nil {
		return it, err
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	sent := 0
	for _, p := range st.Peers {
		if p.Self || !p.Online || p.Platform == "server" {
			continue
		}
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			if sendTo(ip, it, data) == nil {
				mu.Lock()
				sent++
				mu.Unlock()
			}
		}(p.IP.String())
	}
	wg.Wait()
	n.h.setSent(it.ID, sent)
	it.Sent = sent
	return it, nil
}

func sendTo(ip string, it Item, data []byte) error {
	q := url.Values{"id": {it.ID}, "kind": {it.Kind}, "name": {it.Name}, "mime": {it.Mime}}
	req, err := http.NewRequest(http.MethodPost, "http://"+net.JoinHostPort(ip, strconv.Itoa(Port))+"/clip?"+q.Encode(), bytesReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	resp, err := peerClient.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s ответил %s", ip, resp.Status)
	}
	return nil
}
