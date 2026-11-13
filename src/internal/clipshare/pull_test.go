package clipshare

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"sshtunnel/internal/mesh"
)

// fakeNet — сеть устройств для узла буфера: список устройств задаёт тест,
// «звонок» на сервер ведёт на httptest-сервер с общим буфером сервера.
type fakeNet struct {
	mu     sync.Mutex
	peers  []mesh.Peer
	server string // адрес httptest-сервера
	dials  int
}

func (f *fakeNet) Status() mesh.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return mesh.Status{State: "online", Self: mesh.Peer{Name: "Телефон", Host: "phone", Self: true},
		Peers: append([]mesh.Peer(nil), f.peers...)}
}

func (f *fakeNet) DialPeer(addr string) (net.Conn, error) {
	f.mu.Lock()
	f.dials++
	srv := f.server
	f.mu.Unlock()
	return net.Dial("tcp", srv)
}

func (f *fakeNet) setServerOnline(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers = nil
	if on {
		f.peers = []mesh.Peer{{Name: "VPS", Host: "vps", IP: netip.MustParseAddr("198.19.255.254"), Platform: "server", Online: true}}
	}
}

// boardAs — буфер сервера, который считает каждый запрос пришедшим от dev.
func boardAs(t *testing.T, b *ServerBoard, dev, name string) *httptest.Server {
	h := b.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(WithServerCaller(r.Context(), ServerCaller{Device: dev, Name: name})))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitPulled(t *testing.T, n *node) {
	t.Helper()
	select {
	case <-n.pulled:
	case <-time.After(5 * time.Second):
		t.Fatal("круг забора с сервера не прошёл")
	}
}

// Устройство не было в сети, пока на ПК копировали; сервер появился — узел
// сам забирает пропущенное (одно — один раз), отмечает «получено» и дальше
// шлёт свои записи и на сервер.
func TestУстройствоЗабираетПропущенноеССервера(t *testing.T) {
	old := pullCheck
	pullCheck = 20 * time.Millisecond
	t.Cleanup(func() { pullCheck = old })

	board := OpenServerBoard(t.TempDir(), 0)
	fromPC := boardAs(t, board, "pc", "ПК")
	fromPhone := boardAs(t, board, "phone", "Телефон")

	// ПК кладёт запись на сервер, телефона нет.
	resp, err := http.Post(fromPC.URL+"/clip?id=pc-1&kind=text", "text/plain", strings.NewReader("скопировано на ПК"))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("push с ПК: %v %v", err, resp)
	}
	resp.Body.Close()

	var got []Item
	var gmu sync.Mutex
	SetSink(func(it Item, path string) { gmu.Lock(); got = append(got, it); gmu.Unlock() })
	t.Cleanup(func() { SetSink(nil) })

	fn := &fakeNet{server: strings.TrimPrefix(fromPhone.URL, "http://")}
	n := startNodeWith(t.TempDir(), fn)
	t.Cleanup(n.close)

	// Сервера нет на связи — никуда не звоним.
	time.Sleep(100 * time.Millisecond)
	fn.mu.Lock()
	if fn.dials != 0 {
		t.Fatalf("звонили на сервер, которого нет: %d", fn.dials)
	}
	fn.mu.Unlock()

	// Сервер появился — забор сразу.
	fn.setServerOnline(true)
	waitPulled(t, n)
	if !n.h.has("pc-1") {
		t.Fatal("пропущенное не забрано")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		gmu.Lock()
		k := len(got)
		gmu.Unlock()
		if k == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	gmu.Lock()
	if len(got) != 1 || got[0].From != "ПК" {
		t.Fatalf("в буфер телефона попало: %+v", got)
	}
	gmu.Unlock()
	if !n.hasServer("198.19.255.254") {
		t.Fatal("сервер с буфером не запомнен")
	}

	// Отметка «получено» дошла до сервера: missed для телефона пуст.
	resp, _ = http.Get(fromPhone.URL + "/missed")
	var out struct {
		Items []Item `json:"items"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Items) != 0 {
		t.Fatalf("после забора сервер ещё считает пропущенным: %+v", out.Items)
	}

	// Сервер пропал и вернулся — повторный забор не дублирует запись.
	fn.setServerOnline(false)
	time.Sleep(60 * time.Millisecond)
	fn.setServerOnline(true)
	waitPulled(t, n)
	items, _, _ := n.h.list(0)
	if len(items) != 1 {
		t.Fatalf("запись задвоилась: %d", len(items))
	}

	// Своя запись телефона уходит и на сервер: ПК заберёт её оттуда.
	if _, err := n.push(Item{ID: "phone-1", Kind: KindText}, []byte("с телефона")); err != nil {
		t.Fatal(err)
	}
	resp, _ = http.Get(fromPC.URL + "/missed")
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Items) != 1 || out.Items[0].ID != "phone-1" {
		t.Fatalf("ПК не получит запись телефона: %+v", out.Items)
	}
}
