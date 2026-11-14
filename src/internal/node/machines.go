package node

// «Машины» на сервере (ТЗ 15): круглосуточная история показаний машин сети.
//
// Откуда показания. Сервер сам машины не читает: адреса, пароли и токены
// машин лежат только на тех устройствах, где машины заведены, а сервер не
// может первым позвонить устройству (meshd: «server cannot initiate»).
// Поэтому показания присылает устройство-читатель: каждые 30 с, после
// своего обычного опроса (internal/machines), оно отправляет их сюда —
// POST /push на порт PortMachines. Значит, история без дыр будет у той
// машины, которую читает устройство, включённое круглосуточно (домашний
// сервер, сама машина с ssh_tunnel и источником «это устройство»); ноутбук,
// который выключали, получает дыры своего отсутствия закрытыми с сервера.
//
// Хранение: <сеть>/machines/<PeerID читателя>/ГГГГММДД.jsonl — строка на
// точку (не чаще раза в minGap на машину), meta.json — имена машин и «видно
// всем». Старше MachinesDays дней — стирается, место считается в общей
// квоте хоста (room/grow), как у остальных плагинов.
//
// Кто что видит: устройство — историю своих машин всегда, чужих — только
// если читатель «показывает машины всем устройствам сети» (как SharePort на
// устройствах). Сырой id устройства наружу не уходит — только PeerID.

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/nodeapi"
)

const (
	// minGap — не чаще одной точки на машину: устройства шлют раз в 30 с.
	minGap = 50 * time.Second
	// maxMachinesPush — сколько машин в одной посылке.
	maxMachinesPush = 64
	// maxHistoryPoints — сколько точек отдаём на машину за раз.
	maxHistoryPoints = 1000
)

// MachineReading — показания одной машины в посылке устройства (поля — как
// machines.Reading, лишние не нужны).
type MachineReading struct {
	Time     int64   `json:"time"`
	Error    string  `json:"error,omitempty"`
	CPU      float64 `json:"cpu"`
	MemUsed  int64   `json:"memUsed,omitempty"`
	MemTotal int64   `json:"memTotal,omitempty"`
	DiskUsed int64   `json:"diskUsed,omitempty"`
	DiskTot  int64   `json:"diskTotal,omitempty"`
	Temp     float64 `json:"temp,omitempty"`
	Online   bool    `json:"online"`
}

// MachinePush — машина в посылке.
type MachinePush struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Src     string          `json:"src,omitempty"`
	Reading *MachineReading `json:"reading,omitempty"`
}

// MachinesPush — посылка устройства: его машины и показывает ли оно их всем.
type MachinesPush struct {
	Shared   bool          `json:"shared"`
	Machines []MachinePush `json:"machines"`
}

// point — строка истории.
type point struct {
	T    int64   `json:"t"`
	M    string  `json:"m"`
	CPU  float64 `json:"c"`
	Mem  float64 `json:"mem,omitempty"`  // %
	Disk float64 `json:"disk,omitempty"` // %
	Temp float64 `json:"tp,omitempty"`
	On   bool    `json:"on,omitempty"`
	Err  bool    `json:"e,omitempty"` // показаний не было (машина не ответила)
}

// machMeta — meta.json читателя.
type machMeta struct {
	Name     string            `json:"name"`
	Shared   bool              `json:"shared"`
	Last     int64             `json:"last"`
	Machines map[string]string `json:"machines"` // id → имя
}

// machStore — история машин одной сети.
type machStore struct {
	dir string

	mu   sync.Mutex
	meta map[string]*machMeta // по PeerID читателя
	last map[string]int64     // читатель/машина → время последней точки
}

func (h *Host) machOf(nd *netData) *machStore {
	nd.mu.Lock()
	defer nd.mu.Unlock()
	if nd.mach == nil {
		nd.mach = &machStore{dir: filepath.Join(nd.dir, "machines"), meta: map[string]*machMeta{}, last: map[string]int64{}}
		nd.mach.loadMeta()
	}
	return nd.mach
}

func (m *machStore) loadMeta() {
	des, _ := os.ReadDir(m.dir)
	for _, d := range des {
		if !d.IsDir() || !validPeer(d.Name()) {
			continue
		}
		var mm machMeta
		if b, err := os.ReadFile(filepath.Join(m.dir, d.Name(), "meta.json")); err == nil && json.Unmarshal(b, &mm) == nil {
			if mm.Machines == nil {
				mm.Machines = map[string]string{}
			}
			m.meta[d.Name()] = &mm
		}
	}
}

// validPeer — PeerID: 24 шестнадцатеричных знака (из него — путь на диске).
func validPeer(id string) bool { return len(id) == 24 && nodeapi.ValidNet(id) }

// validMachineID — id машины от устройства (из него ничего не собирается на
// диске, но в строку истории — да).
func validMachineID(id string) bool {
	if id == "" || len(id) > 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func cleanName(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return s
}

func pct(a, b int64) float64 {
	if b <= 0 || a < 0 {
		return 0
	}
	return math.Round(float64(a)*1000/float64(b)) / 10
}

func round1(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10) / 10
}

func dayFile(t time.Time) string { return t.UTC().Format("20060102") + ".jsonl" }

// push — принять показания устройства c.
func (h *Host) machinesPush(w http.ResponseWriter, r *http.Request, nd *netData) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	var req MachinesPush
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "нужен JSON {\"machines\":[…]}")
		return
	}
	if len(req.Machines) > maxMachinesPush {
		writeErr(w, http.StatusBadRequest, "не больше 64 машин в посылке")
		return
	}
	c := callerOf(r)
	peer := nodeapi.PeerID(c.Device)
	now := h.opt.Now()
	ms := h.machOf(nd)

	var lines []byte
	ms.mu.Lock()
	mm := ms.meta[peer]
	if mm == nil {
		mm = &machMeta{Machines: map[string]string{}}
	}
	names := map[string]string{}
	for _, m := range req.Machines {
		if !validMachineID(m.ID) {
			continue
		}
		names[m.ID] = cleanName(m.Name, 64)
		if m.Reading == nil {
			continue
		}
		k := peer + "/" + m.ID
		if last := ms.last[k]; last != 0 && now.Unix()-last < int64(minGap/time.Second) {
			continue
		}
		rd := m.Reading
		p := point{T: now.Unix(), M: m.ID, On: rd.Online}
		if rd.Error != "" {
			p.Err = true
		} else {
			p.CPU, p.Mem, p.Disk, p.Temp = round1(rd.CPU), pct(rd.MemUsed, rd.MemTotal), pct(rd.DiskUsed, rd.DiskTot), round1(rd.Temp)
		}
		b, _ := json.Marshal(p)
		lines = append(append(lines, b...), '\n')
		ms.last[k] = p.T
	}
	metaChanged := mm.Name != c.Name || mm.Shared != req.Shared || len(mm.Machines) != len(names) || ms.meta[peer] == nil
	for id, n := range names {
		if mm.Machines[id] != n {
			metaChanged = true
		}
	}
	mm.Name, mm.Shared, mm.Machines, mm.Last = cleanName(c.Name, 64), req.Shared, names, now.Unix()
	ms.meta[peer] = mm
	ms.mu.Unlock()

	if len(lines) > 0 {
		if left, why := h.room(nd, "", -1); left < int64(len(lines)) {
			h.event(nodeapi.Event{Plugin: nodeapi.PluginMachines, Net: nd.id, Error: true, Text: why})
			writeErr(w, http.StatusInsufficientStorage, why)
			return
		}
	}
	dir := filepath.Join(ms.dir, peer)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if metaChanged || len(lines) > 0 {
		ms.mu.Lock()
		b, _ := json.Marshal(mm)
		ms.mu.Unlock()
		if err := writeAtomic(filepath.Join(dir, "meta.json"), b); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if len(lines) > 0 {
		ms.mu.Lock()
		err := appendFile(filepath.Join(dir, dayFile(now)), lines)
		ms.mu.Unlock()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		h.grow(nd, "", int64(len(lines)))
	}
	writeJSONTo(w, map[string]any{"ok": true, "every": 30})
}

func appendFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if e := f.Close(); err == nil {
		err = e
	}
	return err
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Series — история одной машины (точки по времени, равные шаги).
type Series struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	T    []int64   `json:"t"`
	CPU  []float64 `json:"cpu"`
	Mem  []float64 `json:"mem"`
	Disk []float64 `json:"disk"`
	Temp []float64 `json:"temp"`
	On   []bool    `json:"on"`
}

// HistoryReporter — история машин одного читателя.
type HistoryReporter struct {
	ID       string   `json:"id"` // PeerID
	Name     string   `json:"name,omitempty"`
	Self     bool     `json:"self,omitempty"` // это тот, кто спрашивает
	Shared   bool     `json:"shared"`
	Last     int64    `json:"last,omitempty"`
	Machines []Series `json:"machines"`
}

// History — ответ GET /history.
type History struct {
	Since    int64             `json:"since"`
	Step     int64             `json:"step"`
	Days     int               `json:"days"`
	Reporter []HistoryReporter `json:"reporters"`
}

// machinesHistory — GET /history?from=<PeerID|self>&machine=<id>&since=<unix>&step=<с>.
func (h *Host) machinesHistory(w http.ResponseWriter, r *http.Request, nd *netData) {
	q := r.URL.Query()
	me := nodeapi.PeerID(callerOf(r).Device)
	now := h.opt.Now().Unix()
	days := h.machinesDays()
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	if since <= 0 {
		since = now - 24*3600
	}
	if min := now - int64(days)*86400; since < min {
		since = min
	}
	step, _ := strconv.ParseInt(q.Get("step"), 10, 64)
	if span := (now - since) / maxHistoryPoints; step < span {
		step = span
	}
	if step < 30 {
		step = 30
	}
	from, machine := q.Get("from"), q.Get("machine")
	if from == "self" {
		from = me
	}
	ms := h.machOf(nd)
	ms.mu.Lock()
	type rep struct {
		id string
		m  machMeta
	}
	var reps []rep
	for id, mm := range ms.meta {
		if (from != "" && id != from) || (id != me && !mm.Shared) {
			continue
		}
		cp := *mm
		cp.Machines = map[string]string{}
		for k, v := range mm.Machines {
			cp.Machines[k] = v
		}
		reps = append(reps, rep{id, cp})
	}
	ms.mu.Unlock()
	sort.Slice(reps, func(i, j int) bool { return reps[i].id < reps[j].id })
	out := History{Since: since, Step: step, Days: days, Reporter: []HistoryReporter{}}
	for _, rp := range reps {
		hr := HistoryReporter{ID: rp.id, Name: rp.m.Name, Self: rp.id == me, Shared: rp.m.Shared, Last: rp.m.Last, Machines: []Series{}}
		pts := ms.read(rp.id, since, machine)
		ids := make([]string, 0, len(rp.m.Machines))
		for id := range rp.m.Machines {
			if machine == "" || id == machine {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			hr.Machines = append(hr.Machines, bucket(id, rp.m.Machines[id], pts[id], since, step))
		}
		out.Reporter = append(out.Reporter, hr)
	}
	writeJSONTo(w, out)
}

// read — точки читателя с since (по машинам).
func (m *machStore) read(peer string, since int64, machine string) map[string][]point {
	out := map[string][]point{}
	dir := filepath.Join(m.dir, peer)
	first := dayFile(time.Unix(since, 0))
	des, _ := os.ReadDir(dir)
	for _, d := range des {
		n := d.Name()
		if d.IsDir() || !strings.HasSuffix(n, ".jsonl") || n < first {
			continue
		}
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var p point
			if json.Unmarshal(sc.Bytes(), &p) != nil || p.T < since || (machine != "" && p.M != machine) {
				continue
			}
			out[p.M] = append(out[p.M], p)
		}
		f.Close()
	}
	return out
}

// bucket — точки в равные шаги: среднее по шагу, «в сети» — хоть раз.
// Шаг без точек пропускается (на графике — разрыв).
func bucket(id, name string, pts []point, since, step int64) Series {
	s := Series{ID: id, Name: name, T: []int64{}, CPU: []float64{}, Mem: []float64{}, Disk: []float64{}, Temp: []float64{}, On: []bool{}}
	sort.Slice(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
	for i := 0; i < len(pts); {
		b := (pts[i].T - since) / step
		var n, cpu, mem, disk, temp float64
		on := false
		j := i
		for ; j < len(pts) && (pts[j].T-since)/step == b; j++ {
			on = on || pts[j].On
			if pts[j].Err {
				continue
			}
			n++
			cpu += pts[j].CPU
			mem += pts[j].Mem
			disk += pts[j].Disk
			temp += pts[j].Temp
		}
		s.T = append(s.T, since+b*step+step/2)
		if n > 0 {
			cpu, mem, disk, temp = round1(cpu/n), round1(mem/n), round1(disk/n), round1(temp/n)
		}
		s.CPU, s.Mem, s.Disk, s.Temp, s.On = append(s.CPU, cpu), append(s.Mem, mem), append(s.Disk, disk), append(s.Temp, temp), append(s.On, on)
		i = j
	}
	return s
}

func (h *Host) machinesDays() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d := h.conf.Config.MachinesDays; d > 0 {
		return d
	}
	return nodeapi.DefaultMachinesDays
}

// pruneMachines — стереть дни старше срока хранения.
func (h *Host) pruneMachines(nd *netData) {
	if _, err := os.Stat(filepath.Join(nd.dir, "machines")); err != nil {
		return
	}
	ms := h.machOf(nd)
	first := dayFile(h.opt.Now().Add(-time.Duration(h.machinesDays()) * 24 * time.Hour))
	ms.mu.Lock()
	defer ms.mu.Unlock()
	var freed int64
	for peer := range ms.meta {
		dir := filepath.Join(ms.dir, peer)
		des, _ := os.ReadDir(dir)
		for _, d := range des {
			if n := d.Name(); strings.HasSuffix(n, ".jsonl") && n < first {
				if info, err := d.Info(); err == nil {
					freed += info.Size()
				}
				os.Remove(filepath.Join(dir, n))
			}
		}
	}
	if freed > 0 {
		h.grow(nd, "", -freed)
	}
}

// machinesState — для панели.
func (h *Host) machinesState(nd *netData) *nodeapi.MachinesNet {
	dir := filepath.Join(nd.dir, "machines")
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	ms := h.machOf(nd)
	out := &nodeapi.MachinesNet{Bytes: du(dir), Reporters: []nodeapi.MachinesReporter{}}
	ms.mu.Lock()
	for id, mm := range ms.meta {
		r := nodeapi.MachinesReporter{ID: id, Name: mm.Name, Shared: mm.Shared, Last: mm.Last, Machines: []string{}}
		for _, n := range mm.Machines {
			r.Machines = append(r.Machines, n)
		}
		sort.Strings(r.Machines)
		out.Reporters = append(out.Reporters, r)
		des, _ := os.ReadDir(filepath.Join(dir, id))
		for _, d := range des {
			if n := d.Name(); strings.HasSuffix(n, ".jsonl") {
				if t, err := time.Parse("20060102", strings.TrimSuffix(n, ".jsonl")); err == nil && (out.Oldest == 0 || t.Unix() < out.Oldest) {
					out.Oldest = t.Unix()
				}
			}
		}
	}
	ms.mu.Unlock()
	sort.Slice(out.Reporters, func(i, j int) bool { return out.Reporters[i].Name < out.Reporters[j].Name })
	return out
}

// ClearMachines — стереть историю машин сети (панель).
func (h *Host) ClearMachines(netID string) error {
	nd, err := h.netFor(netID)
	if err != nil {
		return err
	}
	nd.mu.Lock()
	ms := nd.mach
	nd.mach = nil
	nd.mu.Unlock()
	if ms != nil {
		ms.mu.Lock()
		defer ms.mu.Unlock()
	}
	if err := os.RemoveAll(filepath.Join(nd.dir, "machines")); err != nil {
		return err
	}
	h.event(nodeapi.Event{Plugin: nodeapi.PluginMachines, Net: nd.id, Text: "история машин стёрта из панели"})
	h.recount()
	return nil
}

func (h *Host) serveMachines(w http.ResponseWriter, r *http.Request, nd *netData) {
	switch r.URL.Path {
	case "/push":
		h.machinesPush(w, r, nd)
	case "/history":
		h.machinesHistory(w, r, nd)
	default:
		writeErr(w, http.StatusNotFound, "нет такого адреса")
	}
}

func writeJSONTo(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, text string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": text})
}

var errNoNet = errors.New("неверный id сети")
