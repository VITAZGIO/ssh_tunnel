package machines

// История машин на сервере сети (ТЗ 15). После каждого опроса (раз в 30 с)
// показания своих машин уходят на серверы сети, где включён плагин
// «Машины» (internal/node, порт ServerPort): сервер держит их сутками,
// пока это устройство выключено, — и график на любом устройстве сети не
// рвётся. Страница рисует историю сервера, а последние минуты — свои
// показания; сервера с плагином нет — только свои (~30 минут).
//
// На сервер уходят только показания и имена машин (как в SharePort):
// адреса, пароли и токены остаются здесь. «Показывать мои машины всем» —
// и другие устройства сети видят их историю; иначе — только это устройство.
// «Не хранить историю на сервере» (Settings.NoServer) — не отправлять вовсе.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/devnet"
	"sshtunnel/internal/mesh"
)

// ServerPort — порт плагина «Машины» на сервере (nodeapi.PortMachines).
const ServerPort = 47844

// historyWindow — сколько истории просить у сервера.
const historyWindow = 24 * time.Hour

// srvState — как прошла последняя отправка на сервер.
type srvState struct {
	Name  string `json:"name"`
	IP    string `json:"ip"`
	On    bool   `json:"on"`              // плагин на сервере принимает
	Error string `json:"error,omitempty"` // почему нет
	At    int64  `json:"at,omitempty"`
}

var (
	srvMu    sync.Mutex
	srvSeen  = map[string]*srvState{} // по адресу сервера
	histMu   sync.Mutex
	histAt   time.Time
	histFrom string
	histData *serverHistory
)

// serverHistory — ответ сервера на GET /history (поля — как node.History).
type serverHistory struct {
	Since     int64 `json:"since"`
	Step      int64 `json:"step"`
	Days      int   `json:"days"`
	Reporters []struct {
		ID       string      `json:"id"`
		Name     string      `json:"name"`
		Self     bool        `json:"self"`
		Shared   bool        `json:"shared"`
		Last     int64       `json:"last"`
		Machines []srvSeries `json:"machines"`
	} `json:"reporters"`
}

type srvSeries struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	T    []int64   `json:"t"`
	CPU  []float64 `json:"cpu"`
	Mem  []float64 `json:"mem"`
	On   []bool    `json:"on"`
}

// Series — график машины на странице: история сервера + свои последние
// показания. Src: "server" — с сервера (Server — его имя), "local" — только
// свои.
type Series struct {
	Src    string    `json:"src"`
	Server string    `json:"server,omitempty"`
	Since  int64     `json:"since,omitempty"` // с какого времени
	T      []int64   `json:"t"`
	CPU    []float64 `json:"cpu"`
	Join   int64     `json:"join,omitempty"` // с этой точки — свои показания
}

func meshServers() []mesh.Peer {
	var out []mesh.Peer
	for _, p := range devnet.Status().Peers {
		if !p.Self && p.Online && p.Platform == "server" && p.IP.IsValid() {
			out = append(out, p)
		}
	}
	return out
}

var srvClient = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
	DialContext:         func(_ context.Context, _, addr string) (net.Conn, error) { return devnet.DialPeer(addr) },
	MaxIdleConnsPerHost: 1, IdleConnTimeout: 90 * time.Second,
}}

func srvName(p mesh.Peer) string {
	if p.Name != "" {
		return p.Name
	}
	return p.Host
}

// offErr — плагин на сервере выключен (порт не слушает) или сервер старый.
func offErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "ничего не слушает") || strings.Contains(s, "refused") || strings.Contains(s, "не слушает")
}

// pushServers — отправить показания своих машин на серверы сети.
func pushServers() {
	c := loadConf()
	if c.Settings.NoServer || len(c.Machines) == 0 {
		return
	}
	type pm struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Src     string   `json:"src,omitempty"`
		Reading *Reading `json:"reading,omitempty"`
	}
	body := struct {
		Shared   bool `json:"shared"`
		Machines []pm `json:"machines"`
	}{Shared: c.Settings.Share}
	mu.Lock()
	for _, m := range c.Machines {
		var r *Reading
		if x := readings[m.ID]; x != nil {
			cp := *x
			cp.Hist, cp.HistT, cp.Extra = nil, nil, nil
			r = &cp
		}
		body.Machines = append(body.Machines, pm{ID: m.ID, Name: m.Name, Src: m.Src, Reading: r})
	}
	mu.Unlock()
	b, _ := json.Marshal(body)
	var wg sync.WaitGroup
	for _, p := range meshServers() {
		wg.Add(1)
		go func(p mesh.Peer) {
			defer wg.Done()
			st := &srvState{Name: srvName(p), IP: p.IP.String(), At: time.Now().Unix()}
			err := postJSON("http://"+net.JoinHostPort(st.IP, strconv.Itoa(ServerPort))+"/push", b)
			switch {
			case err == nil:
				st.On = true
			case offErr(err):
				st.Error = "на сервере не включён плагин «Машины» (панель сервера → «Плагины»)"
			default:
				st.Error = err.Error()
			}
			srvMu.Lock()
			srvSeen[st.IP] = st
			srvMu.Unlock()
		}(p)
	}
	wg.Wait()
}

func postJSON(url string, b []byte) error {
	resp, err := srvClient.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// historyServer — сервер, который принимает показания (первый по имени).
func historyServer() *srvState {
	online := map[string]bool{}
	for _, p := range meshServers() {
		online[p.IP.String()] = true
	}
	srvMu.Lock()
	defer srvMu.Unlock()
	var list []*srvState
	for ip, s := range srvSeen {
		if s.On && online[ip] {
			list = append(list, s)
		}
	}
	if len(list) == 0 {
		return nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	cp := *list[0]
	return &cp
}

// fetchHistory — история с сервера (своя и тех, кто показывает всем);
// не чаще раза в минуту.
func fetchHistory() (*serverHistory, *srvState) {
	s := historyServer()
	if s == nil {
		return nil, nil
	}
	histMu.Lock()
	defer histMu.Unlock()
	if histData != nil && histFrom == s.IP && time.Since(histAt) < time.Minute {
		return histData, s
	}
	since := time.Now().Add(-historyWindow).Unix()
	resp, err := srvClient.Get(fmt.Sprintf("http://%s/history?since=%d", net.JoinHostPort(s.IP, strconv.Itoa(ServerPort)), since))
	if err != nil {
		return histData, s
	}
	defer resp.Body.Close()
	var h serverHistory
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&h) != nil {
		return histData, s
	}
	histData, histFrom, histAt = &h, s.IP, time.Now()
	return histData, s
}

// seriesOf — график машины id читателя rep ("" — это устройство): точки
// сервера до первой своей, дальше — свои (local — Hist/HistT показаний).
func seriesOf(h *serverHistory, srv *srvState, self bool, rep, id string, local *Reading) Series {
	out := Series{Src: "local", T: []int64{}, CPU: []float64{}}
	var lt []int64
	var lc []float64
	if local != nil && len(local.HistT) == len(local.Hist) {
		lt, lc = local.HistT, local.Hist
	}
	if h != nil {
		for _, r := range h.Reporters {
			if (self && !r.Self) || (!self && r.ID != rep) {
				continue
			}
			for _, m := range r.Machines {
				if m.ID != id || len(m.T) == 0 {
					continue
				}
				out.Src, out.Server, out.Since = "server", srv.Name, h.Since
				cut := int64(1 << 62)
				if len(lt) > 0 {
					cut = lt[0]
				}
				for i, t := range m.T {
					if t < cut && i < len(m.CPU) {
						out.T, out.CPU = append(out.T, t), append(out.CPU, m.CPU[i])
					}
				}
			}
		}
	}
	if len(lt) > 0 {
		if out.Src == "server" {
			out.Join = lt[0]
		}
		out.T, out.CPU = append(out.T, lt...), append(out.CPU, lc...)
	}
	return out
}
