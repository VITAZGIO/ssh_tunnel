// Package netmap — дополнительное приложение «Карта сети» (ТЗ 4, п. 10):
// устройства сети, как на схеме панели главного сервера. Только просмотр:
// имена и значки меняются в панели.
package netmap

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/devnet"
	"sshtunnel/internal/mesh"
)

func init() {
	addons.RegisterRoutes("netmap", addons.Route{
		Path: "/api/netmap", Methods: "GET",
		Desc: "Карта сети: это устройство, остальные (онлайн, напрямую или через сервер, задержка rttMs, был в сети lastSeen).",
		H:    handle,
	})
}

// Node — устройство на карте.
type Node struct {
	mesh.Peer
	// RTTMs — задержка до устройства: по прямому соединению, если оно есть,
	// иначе через сервер (ping через meshd). 0 — не измерено.
	RTTMs float64 `json:"rttMs,omitempty"`
	// Server — это сервер сети, а не устройство.
	Server bool `json:"server,omitempty"`
}

// Map — ответ /api/netmap.
type Map struct {
	State string `json:"state"` // off, connecting, online, error, stopped
	Error string `json:"error,omitempty"`
	Self  *Node  `json:"self,omitempty"`
	Nodes []Node `json:"nodes"`
}

// Задержку меряем не чаще раза в rttEvery на устройство: карта
// обновляется раз в несколько секунд, а ping через сервер — строка по
// управляющему соединению.
const rttEvery = 10 * time.Second

var (
	rttMu  sync.Mutex
	rtts   = map[string]rttEntry{}
	inPing = map[string]bool{}
)

type rttEntry struct {
	ms float64
	at time.Time
}

// ping — кэшированная задержка; устаревшую обновляет в фоне.
func ping(p mesh.Peer) float64 {
	key := p.IP.String()
	rttMu.Lock()
	e, ok := rtts[key]
	stale := !ok || time.Since(e.at) > rttEvery
	if stale && !inPing[key] {
		inPing[key] = true
		go func() {
			d, err := devnet.Ping(p.IP)
			rttMu.Lock()
			delete(inPing, key)
			if err == nil {
				ms := float64(d.Microseconds()) / 1000
				if ms <= 0 {
					ms = 0.1
				}
				rtts[key] = rttEntry{ms, time.Now()}
			} else {
				delete(rtts, key)
			}
			rttMu.Unlock()
		}()
	}
	rttMu.Unlock()
	return e.ms
}

// Build — карта сейчас.
func Build() Map {
	st := devnet.Status()
	m := Map{State: st.State, Error: st.Error, Nodes: []Node{}}
	if st.State == "off" || st.State == "stopped" {
		return m
	}
	self := Node{Peer: st.Self}
	self.Self = true
	m.Self = &self
	for _, p := range st.Peers {
		if p.Self {
			continue
		}
		n := Node{Peer: p, Server: p.Platform == "server"}
		switch {
		case p.Direct && p.DirectRTTMs > 0:
			n.RTTMs = p.DirectRTTMs
		case p.Online:
			n.RTTMs = ping(p)
		}
		m.Nodes = append(m.Nodes, n)
	}
	return m
}

func handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(Build())
}
