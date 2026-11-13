package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/devnet"
	"sshtunnel/internal/mesh"
)

// Модуль приложения «Резервные копии» на устройстве: список копий,
// расписание, кнопки «сделать сейчас» и «восстановить». Копии кладутся
// только на сервер сети и только по сети устройств (devnet.DialPeer) — не
// через туннель в интернет.

func init() {
	addons.Register("backup", func() addons.Module { return &module{} })
	addons.RegisterRoutes("backup",
		addons.Route{Path: "/api/backup", Methods: "GET, POST",
			Desc: "Резервные копии этого устройства: jobs (настройки и состояние), servers (серверы сети). POST {\"op\":\"add\",\"job\":{name,path,schedule:\"daily|weekly|change\",delayMin,keep,metered,server}} | {\"op\":\"update\",\"job\":{id,…}} | {\"op\":\"remove\",\"id\"} | {\"op\":\"run\",\"id\"} | {\"op\":\"cancel\",\"id\"}.",
			H:    handleAPI},
		addons.Route{Path: "/api/backup/remote", Methods: "GET",
			Desc: "Копии на сервере: свои и других устройств того же пользователя ?server=<имя или адрес>.",
			H:    handleRemote},
		addons.Route{Path: "/api/backup/versions", Methods: "GET",
			Desc: "Версии копии ?device=&set=[&server=].",
			H:    handleVersions},
		addons.Route{Path: "/api/backup/files", Methods: "GET",
			Desc: "Файлы версии ?device=&set=&version=[&server=].",
			H:    handleFiles},
		addons.Route{Path: "/api/backup/restore", Methods: "POST",
			Desc: "Восстановить {device,set,version,path (файл или папка внутри копии, пусто — всё),target (полный путь),mode:\"rename|skip|overwrite\",server}. Молча не перезаписывает: по умолчанию другой файл на месте остаётся, восстановленное ложится рядом.",
			H:    handleRestore},
		addons.Route{Path: "/api/backup/delete", Methods: "POST",
			Desc: "Убрать копию с сервера {device,set[,version],server}.",
			H:    handleDelete},
	)
}

var (
	meteredMu sync.Mutex
	meteredFn = func() bool { return false }
)

// SetMetered — как узнать, что связь лимитная (мобильная сеть телефона):
// тогда копии без разрешения «можно по мобильной сети» ждут. nil — никогда.
func SetMetered(f func() bool) {
	meteredMu.Lock()
	if f == nil {
		f = func() bool { return false }
	}
	meteredFn = f
	meteredMu.Unlock()
}

func metered() bool {
	meteredMu.Lock()
	f := meteredFn
	meteredMu.Unlock()
	return f()
}

// Status — состояние копии для страницы.
type Status struct {
	Running  bool      `json:"running"`
	Phase    string    `json:"phase,omitempty"` // scan | hash | upload | commit
	Done     int64     `json:"done,omitempty"`
	Total    int64     `json:"total,omitempty"`
	LastRun  int64     `json:"lastRun,omitempty"` // последняя попытка (unix)
	LastOK   int64     `json:"lastOK,omitempty"`  // последняя удачная
	LastErr  string    `json:"lastErr,omitempty"`
	Waiting  string    `json:"waiting,omitempty"` // почему не идёт (лимитная сеть, нет сервера)
	Files    int       `json:"files,omitempty"`
	Bytes    int64     `json:"bytes,omitempty"`
	Skipped  []Skipped `json:"skipped,omitempty"`
	Server   string    `json:"server,omitempty"`
	Sig      string    `json:"sig,omitempty"`     // «отпечаток» папки на момент удачной копии (change)
	Changed  int64     `json:"changed,omitempty"` // когда заметили правку (change)
	NextHint int64     `json:"next,omitempty"`    // когда примерно следующая (unix)
}

type jobRun struct {
	Job
	st     Status
	cancel context.CancelFunc
	kick   bool
}

type module struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	jobs map[string]*jobRun
}

var (
	curMu sync.Mutex
	cur   *module
)

func current() *module {
	curMu.Lock()
	defer curMu.Unlock()
	return cur
}

func dataDir() string { return filepath.Join(addons.DataDir(), "backup") }

type saved struct {
	Jobs   []Job             `json:"jobs"`
	Status map[string]Status `json:"status"`
}

func (m *module) Start() error {
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.jobs = map[string]*jobRun{}
	var s saved
	_ = readJSON(filepath.Join(dataDir(), "jobs.json"), &s)
	for _, j := range s.Jobs {
		st := s.Status[j.ID]
		st.Running, st.Phase, st.Done, st.Total = false, "", 0, 0
		m.jobs[j.ID] = &jobRun{Job: j, st: st}
	}
	m.wg.Add(1)
	go m.loop()
	curMu.Lock()
	cur = m
	curMu.Unlock()
	return nil
}

func (m *module) Stop() {
	curMu.Lock()
	if cur == m {
		cur = nil
	}
	curMu.Unlock()
	m.cancel()
	m.mu.Lock()
	for _, j := range m.jobs {
		if j.cancel != nil {
			j.cancel()
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
	m.mu.Lock()
	m.saveLocked()
	m.mu.Unlock()
}

func (m *module) saveLocked() {
	s := saved{Status: map[string]Status{}}
	for _, j := range m.jobs {
		s.Jobs = append(s.Jobs, j.Job)
		st := j.st
		st.Running, st.Phase, st.Done, st.Total = false, "", 0, 0
		s.Status[j.ID] = st
	}
	sort.Slice(s.Jobs, func(a, b int) bool { return s.Jobs[a].Name < s.Jobs[b].Name })
	_, _ = writeJSON(filepath.Join(dataDir(), "jobs.json"), s)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------- расписание ----------

const retryAfter = 10 * time.Minute

func (m *module) loop() {
	defer m.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		m.tick(time.Now())
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// signature — отпечаток папки (имена, размеры, время) без чтения файлов.
func signature(root string) string {
	h := sha256.New()
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil {
			fmt.Fprintf(h, "%s\x00%d\x00%d\n", p, info.Size(), info.ModTime().UnixNano())
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// due — пора ли делать копию j (под m.mu); заодно обновляет «когда».
func due(j *jobRun, now time.Time) bool {
	if j.kick {
		return true
	}
	if j.st.LastErr != "" && now.Sub(time.Unix(j.st.LastRun, 0)) < retryAfter {
		return false
	}
	last := time.Unix(j.st.LastOK, 0)
	switch j.Schedule {
	case Daily:
		j.st.NextHint = last.Add(24 * time.Hour).Unix()
		return j.st.LastOK == 0 || now.Sub(last) >= 24*time.Hour
	case Weekly:
		j.st.NextHint = last.Add(7 * 24 * time.Hour).Unix()
		return j.st.LastOK == 0 || now.Sub(last) >= 7*24*time.Hour
	case Change:
		if j.st.Changed == 0 {
			return j.st.LastOK == 0
		}
		j.st.NextHint = j.st.Changed + int64(j.DelayMin)*60
		return now.Unix() >= j.st.NextHint
	}
	return false
}

func (m *module) tick(now time.Time) {
	m.mu.Lock()
	var list []*jobRun
	for _, j := range m.jobs {
		list = append(list, j)
	}
	m.mu.Unlock()
	for _, j := range list {
		if j.Schedule == Change {
			sig := signature(j.Path)
			m.mu.Lock()
			if j.st.LastOK != 0 && sig != j.st.Sig {
				// Правка: ждём тишины DelayMin — каждая новая правка отодвигает.
				if j.st.Changed == 0 || sig != j.lastSeenSig() {
					j.st.Changed = now.Unix()
				}
			}
			j.setSeenSig(sig)
			m.mu.Unlock()
		}
		m.mu.Lock()
		run := !j.st.Running && due(j, now)
		m.mu.Unlock()
		if run {
			m.start(j)
		}
	}
}

// seen — последний увиденный отпечаток (для «каждая правка отодвигает»).
var seenSigs sync.Map

func (j *jobRun) lastSeenSig() string {
	v, _ := seenSigs.Load(j.ID)
	s, _ := v.(string)
	return s
}

func (j *jobRun) setSeenSig(s string) { seenSigs.Store(j.ID, s) }

// start — запустить копию j в фоне.
func (m *module) start(j *jobRun) {
	m.mu.Lock()
	if j.st.Running {
		m.mu.Unlock()
		return
	}
	kicked := j.kick
	j.kick = false
	if metered() && !j.Metered {
		j.st.Waiting = "лимитная (мобильная) сеть — копия ждёт Wi-Fi; разреши «можно по мобильной сети», если нужно"
		if kicked {
			j.st.LastErr = j.st.Waiting
		}
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	j.cancel = cancel
	j.st.Running, j.st.Waiting, j.st.Phase, j.st.Done, j.st.Total = true, "", "scan", 0, 0
	job := j.Job
	sigBefore := ""
	if job.Schedule == Change {
		sigBefore = signature(job.Path)
	}
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		started := time.Now()
		res, srv, err := m.runOnce(ctx, job, func(phase string, done, total int64) {
			m.mu.Lock()
			j.st.Phase, j.st.Done, j.st.Total = phase, done, total
			m.mu.Unlock()
		})
		m.mu.Lock()
		defer m.mu.Unlock()
		j.st.Running, j.st.Phase, j.cancel = false, "", nil
		j.st.LastRun = started.Unix()
		j.st.Server = srv
		if err != nil {
			j.st.LastErr = err.Error()
			if errors.Is(err, context.Canceled) {
				j.st.LastErr = "остановлено"
			}
		} else {
			j.st.LastErr, j.st.LastOK = "", time.Now().Unix()
			j.st.Files, j.st.Bytes, j.st.Skipped = res.Files, res.Version.Bytes, res.Skipped
			if len(j.st.Skipped) > 50 {
				j.st.Skipped = j.st.Skipped[:50]
			}
			j.st.Sig, j.st.Changed = sigBefore, 0
			if len(res.Skipped) > 0 {
				devnet.Warn(fmt.Sprintf("Резервная копия «%s»: %d файлов пропущено (менялись при чтении или не читаются) — см. приложение.", job.Name, len(res.Skipped)))
			}
		}
		if _, ok := m.jobs[j.ID]; ok {
			m.saveLocked()
		}
	}()
}

func (m *module) runOnce(ctx context.Context, job Job, prog Progress) (Result, string, error) {
	rm, name, err := pickServer(ctx, job.Server)
	if err != nil {
		return Result{}, "", err
	}
	cache := LoadCache(filepath.Join(dataDir(), "cache-"+job.ID+".json"))
	res, err := RunBackup(ctx, rm, job, cache, prog)
	return res, name, err
}

// ---------- серверы ----------

// servers — серверы сети, которые сейчас на связи.
func servers() []mesh.Peer {
	var out []mesh.Peer
	for _, p := range devnet.Status().Peers {
		if !p.Self && p.Online && p.Platform == "server" && p.IP.IsValid() {
			out = append(out, p)
		}
	}
	return out
}

func remoteFor(ip string) *Remote {
	return &Remote{Base: "http://" + net.JoinHostPort(ip, strconv.Itoa(Port)),
		HC: &http.Client{Transport: &http.Transport{
			DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
				return devnet.DialPeer(addr)
			},
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     60 * time.Second,
		}}}
}

// pickServer — сервер с копиями: названный (имя или адрес) или первый,
// кто ответил.
func pickServer(ctx context.Context, want string) (*Remote, string, error) {
	st := devnet.Status()
	if st.State == "off" || st.State == "stopped" {
		return nil, "", devnet.ErrOff
	}
	list := servers()
	if len(list) == 0 {
		return nil, "", errors.New("ни одного сервера сети нет на связи — копия будет, когда он появится")
	}
	var lastErr error
	for _, p := range list {
		if want != "" && p.Host != want && p.IP.String() != want {
			continue
		}
		rm := remoteFor(p.IP.String())
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := rm.Sets(c)
		cancel()
		if err == nil {
			return rm, p.Host, nil
		}
		lastErr = err
	}
	if want != "" && lastErr == nil {
		return nil, "", fmt.Errorf("сервера «%s» сейчас нет на связи", want)
	}
	return nil, "", fmt.Errorf("на сервере нет резервных копий (включи их в панели сервера, «Плагины»): %v", lastErr)
}

// ---------- API ----------

func writeJSON2(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	writeJSON2(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

type jobView struct {
	Job
	Status Status `json:"status"`
}

func (m *module) view() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	jobs := []jobView{}
	for _, j := range m.jobs {
		jobs = append(jobs, jobView{j.Job, j.st})
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].Name < jobs[b].Name })
	srvs := []map[string]string{}
	for _, p := range servers() {
		srvs = append(srvs, map[string]string{"host": p.Host, "name": p.Name, "ip": p.IP.String()})
	}
	return map[string]any{"jobs": jobs, "servers": srvs, "metered": metered(), "mesh": devnet.Status().State}
}

func handleAPI(w http.ResponseWriter, r *http.Request) {
	m := current()
	if m == nil {
		writeJSON2(w, http.StatusServiceUnavailable, map[string]string{"error": "приложение не запущено"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON2(w, 200, m.view())
		return
	}
	var req struct {
		Op  string `json:"op"`
		ID  string `json:"id"`
		Job Job    `json:"job"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, errors.New("нужен JSON {\"op\"}"))
		return
	}
	var err error
	switch req.Op {
	case "add":
		j := req.Job
		j.ID = newID()
		if err = j.Check(); err == nil {
			m.mu.Lock()
			if len(m.jobs) >= 32 {
				err = errors.New("не больше 32 копий")
			} else {
				m.jobs[j.ID] = &jobRun{Job: j, kick: true}
				m.saveLocked()
			}
			m.mu.Unlock()
			if err == nil {
				go m.tick(time.Now())
			}
		}
	case "update":
		j := req.Job
		if err = j.Check(); err == nil {
			m.mu.Lock()
			if old := m.jobs[j.ID]; old == nil {
				err = errors.New("нет такой копии")
			} else {
				old.Job = j
				m.saveLocked()
			}
			m.mu.Unlock()
		}
	case "remove":
		m.mu.Lock()
		if j := m.jobs[req.ID]; j != nil {
			if j.cancel != nil {
				j.cancel()
			}
			delete(m.jobs, req.ID)
			os.Remove(filepath.Join(dataDir(), "cache-"+req.ID+".json"))
			m.saveLocked()
		}
		m.mu.Unlock()
	case "run":
		m.mu.Lock()
		j := m.jobs[req.ID]
		if j == nil {
			err = errors.New("нет такой копии")
		} else {
			j.kick = true
		}
		m.mu.Unlock()
		if j != nil {
			m.start(j)
		}
	case "cancel":
		m.mu.Lock()
		if j := m.jobs[req.ID]; j != nil && j.cancel != nil {
			j.cancel()
		}
		m.mu.Unlock()
	default:
		err = errors.New("op: add, update, remove, run или cancel")
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON2(w, 200, m.view())
}

func remoteOf(r *http.Request) (*Remote, error) {
	rm, _, err := pickServer(r.Context(), r.URL.Query().Get("server"))
	return rm, err
}

func handleRemote(w http.ResponseWriter, r *http.Request) {
	rm, err := remoteOf(r)
	if err != nil {
		fail(w, err)
		return
	}
	sets, err := rm.Sets(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON2(w, 200, map[string]any{"sets": sets})
}

func handleVersions(w http.ResponseWriter, r *http.Request) {
	rm, err := remoteOf(r)
	if err != nil {
		fail(w, err)
		return
	}
	q := r.URL.Query()
	vs, err := rm.Versions(r.Context(), q.Get("device"), q.Get("set"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON2(w, 200, map[string]any{"versions": vs})
}

func handleFiles(w http.ResponseWriter, r *http.Request) {
	rm, err := remoteOf(r)
	if err != nil {
		fail(w, err)
		return
	}
	q := r.URL.Query()
	man, err := rm.Manifest(r.Context(), q.Get("device"), q.Get("set"), q.Get("version"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON2(w, 200, man)
}

func handleRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RestoreReq
		Server string `json:"server"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, err)
		return
	}
	rm, _, err := pickServer(r.Context(), req.Server)
	if err != nil {
		fail(w, err)
		return
	}
	res, err := Restore(r.Context(), rm, req.RestoreReq, time.Now())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON2(w, 200, res)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Device  string `json:"device"`
		Set     string `json:"set"`
		Version string `json:"version"`
		Server  string `json:"server"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, err)
		return
	}
	rm, _, err := pickServer(r.Context(), req.Server)
	if err != nil {
		fail(w, err)
		return
	}
	if err := rm.Delete(r.Context(), req.Device, req.Set, req.Version); err != nil {
		fail(w, err)
		return
	}
	writeJSON2(w, 200, map[string]bool{"ok": true})
}
