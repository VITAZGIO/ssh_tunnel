package traffic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/devnet"
)

// ID — приложение в каталоге.
const ID = "traffic"

func init() {
	addons.Register(ID, func() addons.Module { return &module{} })
	addons.RegisterRoutes(ID,
		addons.Route{Path: "/api/traffic", Methods: "GET",
			Desc: "Расход трафика: ?period=day|week|month&net=all|wifi|mobile — по дням (server, direct, mesh), топ программ, лимит.",
			H:    handleGet},
		addons.Route{Path: "/api/traffic/limit", Methods: "POST",
			Desc: "Лимит {\"on\":bool,\"gb\":число,\"period\":\"month|day\",\"scope\":\"all|server|mobile\"} — только предупреждения на 80% и 100%.",
			H:    handleLimit},
	)
}

var (
	modMu  sync.Mutex
	active *store
)

// module — учёт, пока приложение добавлено и включено.
type module struct {
	st   *store
	stop chan struct{}
	done chan struct{}
}

func (m *module) Start() error {
	m.st = openStore(addons.DataDir())
	m.stop, m.done = make(chan struct{}), make(chan struct{})
	modMu.Lock()
	active = m.st
	modMu.Unlock()
	setSink(m.st)
	go m.loop()
	return nil
}

func (m *module) Stop() {
	setSink(nil)
	modMu.Lock()
	if active == m.st {
		active = nil
	}
	modMu.Unlock()
	close(m.stop)
	<-m.done
	_ = m.st.save()
}

// loop — раз в полминуты проверить лимит, раз в минуту записать файл, раз
// в сутки удалить старое.
func (m *module) loop() {
	defer close(m.done)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-m.stop:
			return
		case <-tick.C:
		}
		n++
		if level, l, used := m.st.checkLimit(); level > 0 {
			devnet.Warn(limitText(level, l, used))
		}
		if n%2 == 0 {
			_ = m.st.save()
		}
		if n%2880 == 0 {
			m.st.prune()
		}
	}
}

func limitText(level int, l Limit, used int64) string {
	per := "в этом месяце"
	if l.Period == "day" {
		per = "сегодня"
	}
	if level >= 100 {
		return fmt.Sprintf("Расход трафика: лимит %s исчерпан — %s из %s. VPN продолжает работать.", per, human(used), human(int64(l.GB*1e9)))
	}
	return fmt.Sprintf("Расход трафика: %s израсходовано %s из %s (80%%).", per, human(used), human(int64(l.GB*1e9)))
}

func human(b int64) string {
	switch {
	case b >= 1e9:
		return fmt.Sprintf("%.2f ГБ", float64(b)/1e9)
	case b >= 1e6:
		return fmt.Sprintf("%.1f МБ", float64(b)/1e6)
	}
	return fmt.Sprintf("%d КБ", b/1000)
}

func cur0() *store {
	modMu.Lock()
	defer modMu.Unlock()
	return active
}

// Day — столбик графика.
type Day struct {
	Date   string `json:"date"`
	Server int64  `json:"server"`
	Direct int64  `json:"direct"`
	Mesh   int64  `json:"mesh"`
}

// Prog — программа в топе: байты по путям за период.
type Prog struct {
	Name   string `json:"name"`
	Server int64  `json:"server"`
	Direct int64  `json:"direct"`
	Mesh   int64  `json:"mesh"`
	Up     int64  `json:"up"`
	Down   int64  `json:"down"`
	Total  int64  `json:"total"`
}

// Report — ответ GET /api/traffic.
type Report struct {
	Period string `json:"period"`
	Net    string `json:"net"`
	// HasNet — есть деление Wi-Fi / мобильная (телефон).
	HasNet bool   `json:"hasNet"`
	Days   []Day  `json:"days"`
	// Hours — 24 столбика по часам для периода «день» (Date — «15:00»).
	Hours []Day `json:"hours,omitempty"`
	Top    []Prog `json:"top"`
	Total  Day    `json:"total"`
	Limit  struct {
		Limit
		Used int64 `json:"used"`
		Pct  int   `json:"pct"`
	} `json:"limit"`
}

func periodLen(p string) int {
	switch p {
	case "day":
		return 1
	case "week":
		return 7
	}
	return 30
}

func build(st *store, period, netf string) Report {
	if period != "day" && period != "week" {
		period = "month"
	}
	if netf != "wifi" && netf != "mobile" {
		netf = "all"
	}
	r := Report{Period: period, Net: netf, HasNet: runtime.GOOS == "android", Days: []Day{}, Top: []Prog{}}
	now := st.now()
	progs := map[string]*Prog{}
	for i := periodLen(period) - 1; i >= 0; i-- {
		d := Day{Date: dayKey(now.AddDate(0, 0, -i))}
		for _, rec := range st.recs(d.Date) {
			if netf != "all" && rec.Net != netf {
				continue
			}
			b := rec.Up + rec.Down
			p := progs[rec.Proc]
			if p == nil {
				p = &Prog{Name: rec.Proc}
				progs[rec.Proc] = p
			}
			switch rec.Route {
			case RouteServer:
				d.Server += b
				p.Server += b
			case RouteDirect:
				d.Direct += b
				p.Direct += b
			case RouteMesh:
				d.Mesh += b
				p.Mesh += b
			}
			p.Up += rec.Up
			p.Down += rec.Down
			p.Total += b
		}
		r.Total.Server += d.Server
		r.Total.Direct += d.Direct
		r.Total.Mesh += d.Mesh
		r.Days = append(r.Days, d)
	}
	if period == "day" {
		y, mo, dd := now.Date()
		for h := 0; h < 24; h++ {
			t := time.Date(y, mo, dd, h, 0, 0, 0, now.Location())
			d := st.hour(t, netf)
			d.Date = t.Format("15:04")
			r.Hours = append(r.Hours, d)
		}
	}
	for _, p := range progs {
		r.Top = append(r.Top, *p)
	}
	sort.Slice(r.Top, func(i, j int) bool { return r.Top[i].Total > r.Top[j].Total })
	if len(r.Top) > 30 {
		r.Top = r.Top[:30]
	}
	l := st.getLimit()
	r.Limit.Limit = l
	r.Limit.Used = st.used(l)
	if l.GB > 0 {
		r.Limit.Pct = int(float64(r.Limit.Used) * 100 / (l.GB * 1e9))
	}
	return r
}

func handleGet(w http.ResponseWriter, r *http.Request) {
	st := cur0()
	if st == nil {
		writeJSON(w, map[string]string{"error": "учёт не запущен"})
		return
	}
	q := r.URL.Query()
	writeJSON(w, build(st, q.Get("period"), q.Get("net")))
}

func handleLimit(w http.ResponseWriter, r *http.Request) {
	st := cur0()
	if st == nil || r.Method != http.MethodPost {
		writeJSON(w, map[string]string{"error": "нужен POST, учёт должен работать"})
		return
	}
	var l Limit
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		writeJSON(w, map[string]string{"error": "нужен JSON {\"on\",\"gb\",\"period\",\"scope\"}"})
		return
	}
	st.setLimit(l)
	_ = st.save()
	writeJSON(w, build(st, r.URL.Query().Get("period"), r.URL.Query().Get("net")))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
