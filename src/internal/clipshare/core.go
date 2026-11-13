package clipshare

import (
	"context"
	"encoding/json"
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
const Port = 47837

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

	// servers — серверы сети, у которых есть общий буфер (ответили на
	// /missed): им тоже отправляем свои записи. Остальным серверам не шлём —
	// иначе каждая запись давала бы в журнале сервера отказ или ошибку.
	srvMu   sync.Mutex
	servers map[string]bool
	stop    chan struct{}
	pulled  chan struct{} // тесты: прошёл круг забора с серверов
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
	n := &node{h: openHistory(dir), ln: &chanListener{ch: make(chan net.Conn), done: make(chan struct{})},
		servers: map[string]bool{}, stop: make(chan struct{}), pulled: make(chan struct{}, 1)}
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
	go n.pullLoop()
	return n
}

func (n *node) close() {
	mesh.Serve(Port, nil)
	close(n.stop)
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
	if err := n.accept(it, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// accept — чужая запись: в историю и (через наблюдателя или sink) в буфер.
// Повтор (тот же id) — молча пропускается.
func (n *node) accept(it Item, data []byte) error {
	if it.ID != "" && n.h.has(it.ID) {
		return nil
	}
	it.Out, it.Sent, it.Seq, it.Gone = false, 0, 0, false
	it, err := n.h.add(it, data)
	if err != nil {
		return err
	}
	sinkMu.Lock()
	f := sink
	sinkMu.Unlock()
	if f != nil {
		go f(it, n.h.path(it.ID))
	}
	return nil
}

// ---- пропущенное, пока устройства не было в сети (буфер на сервере) ----

// pullEvery — как часто спрашивать серверы о пропущенном; сразу — когда
// сервер (снова) появился на связи.
const (
	pullEvery = time.Minute
	pullCheck = 5 * time.Second
)

// pullLoop — забирает с серверов сети записи, пропущенные без связи. Сервер
// сам устройствам не звонит, поэтому спрашивает устройство.
func (n *node) pullLoop() {
	online := map[string]bool{}
	var last time.Time
	t := time.NewTicker(pullCheck)
	defer t.Stop()
	for {
		now := map[string]bool{}
		fresh := false
		for _, p := range devnet.Status().Peers {
			if p.Self || !p.Online || p.Platform != "server" || !p.IP.IsValid() {
				continue
			}
			ip := p.IP.String()
			now[ip] = true
			if !online[ip] {
				fresh = true
			}
		}
		online = now
		if len(now) > 0 && (fresh || time.Since(last) >= pullEvery) {
			last = time.Now()
			for ip := range now {
				n.pullFrom(ip)
			}
			select {
			case n.pulled <- struct{}{}:
			default:
			}
		}
		select {
		case <-n.stop:
			return
		case <-t.C:
		}
	}
}

// pullFrom — пропущенное с одного сервера, старые первыми (последняя
// окажется в буфере), потом отметка «получено».
func (n *node) pullFrom(ip string) {
	base := "http://" + net.JoinHostPort(ip, strconv.Itoa(Port))
	resp, err := pullClient.Get(base + "/missed")
	if err != nil {
		n.setServer(ip, false)
		return
	}
	var out struct {
		Items []Item `json:"items"`
		Seq   int64  `json:"seq"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil {
		n.setServer(ip, false)
		return
	}
	n.setServer(ip, true)
	for _, it := range out.Items {
		if it.ID == "" || n.h.has(it.ID) {
			continue
		}
		r, err := pullClient.Get(base + "/data?id=" + url.QueryEscape(it.ID))
		if err != nil {
			return // связь пропала — отметку не двигаем, заберём потом
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, MaxBytes+1))
		r.Body.Close()
		if err != nil || r.StatusCode != http.StatusOK || len(data) > MaxBytes {
			continue
		}
		if n.accept(it, data) != nil {
			return
		}
	}
	if r, err := pullClient.Post(base+"/ack?seq="+strconv.FormatInt(out.Seq, 10), "text/plain", nil); err == nil {
		r.Body.Close()
	}
}

func (n *node) setServer(ip string, on bool) {
	n.srvMu.Lock()
	if on {
		n.servers[ip] = true
	} else {
		delete(n.servers, ip)
	}
	n.srvMu.Unlock()
}

func (n *node) hasServer(ip string) bool {
	n.srvMu.Lock()
	defer n.srvMu.Unlock()
	return n.servers[ip]
}

var pullClient = &http.Client{
	Timeout: 2 * time.Minute,
	Transport: &http.Transport{
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			return devnet.DialPeer(addr)
		},
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     30 * time.Second,
	},
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
		if p.Self || !p.Online {
			continue
		}
		// Сервер — только тот, у которого есть общий буфер; в «скольким
		// ушло» он не считается: это устройство, а не человек.
		srv := p.Platform == "server"
		if srv && !n.hasServer(p.IP.String()) {
			continue
		}
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			if sendTo(ip, it, data) == nil && !srv {
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
