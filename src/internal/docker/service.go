package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/config"
)

// AppID — плагин в каталоге (addons/catalog_docker.go).
const AppID = "docker"

// Kernel — слой VPN глазами плагина (vpn/vpnlayer/docker_linux.go). Его нет
// в обычной версии без VPN и не на Linux — тогда выбор сохраняется, но не
// действует.
type Kernel interface {
	// SetTargets — какие адреса контейнеров куда вести. Слой ставит правила,
	// пока поднят адаптер, и снимает все, когда он выключается.
	SetTargets([]Target)
	// Status — что сейчас со слоем и сколько правил стоит.
	Status() KernelStatus
}

// KernelStatus — состояние слоя для страницы.
type KernelStatus struct {
	Up    bool   `json:"up"`    // адаптер VPN поднят
	Full  bool   `json:"full"`  // туннель (круглая кнопка) включён
	Rules int    `json:"rules"` // сколько наших правил стоит в ядре
	Error string `json:"error,omitempty"`
}

var (
	kernelMu sync.Mutex
	kernel   Kernel
)

// SetKernel — слой VPN сообщает о себе (зовётся при его создании).
func SetKernel(k Kernel) {
	kernelMu.Lock()
	kernel = k
	kernelMu.Unlock()
	if s := current(); s != nil {
		s.kick()
	}
}

func getKernel() Kernel {
	kernelMu.Lock()
	defer kernelMu.Unlock()
	return kernel
}

// Service — работающий плагин: следит за Docker и держит правила в ядре.
type Service struct {
	st  *Store
	cli *Client

	mu      sync.Mutex
	rows    []Row
	listErr error
	updated time.Time

	wake   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

var (
	curMu sync.Mutex
	cur   *Service
)

func current() *Service {
	curMu.Lock()
	defer curMu.Unlock()
	return cur
}

func init() {
	addons.Register(AppID, func() addons.Module { return &Service{} })
	addons.RegisterRoutes(AppID,
		addons.Route{Path: "/api/docker", Methods: "GET", Desc: "Docker (Linux, VPN-версия): контейнеры (имя, проект compose, образ, сети, состояние), выбор для каждого (host — как компьютер, tunnel — через сервер, direct — напрямую), что действует и почему, проекты, состояние слоя VPN.", H: handleList},
		addons.Route{Path: "/api/docker/set", Methods: "POST", Desc: "Выбор Docker: {\"container\":\"<имя или ID>\",\"route\":\"host|tunnel|direct\"} или {\"project\":\"<проект compose>\",\"route\":…} — весь проект разом. Метка ssh_tunnel.route у контейнера сильнее.", H: handleSet},
	)
}

// Start — плагин включён: следить за Docker и вести правила.
func (s *Service) Start() error {
	if s.st == nil {
		s.st = OpenStore(config.Dir())
	}
	if s.cli == nil {
		s.cli = NewClient(DefaultSocket())
	}
	s.wake = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	curMu.Lock()
	cur = s
	curMu.Unlock()
	go s.loop(ctx)
	go s.watch(ctx)
	return nil
}

// Stop — плагин выключен: правил для контейнеров больше нет, все идут как
// компьютер.
func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
	curMu.Lock()
	if cur == s {
		cur = nil
	}
	curMu.Unlock()
	if k := getKernel(); k != nil {
		k.SetTargets(nil)
	}
}

func (s *Service) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// loop пересчитывает список и правила: по событию Docker (с паузой 300 мс —
// события идут пачками), после выбора человека и раз в минуту на всякий
// случай (событие могло потеряться при обрыве потока).
func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		s.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.wake:
			select {
			case <-time.After(300 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
	}
}

// watch держит поток событий Docker; оборвался (Docker перезапустили) —
// переподключается с паузой.
func (s *Service) watch(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		_ = s.cli.Watch(ctx, s.kick)
		s.kick() // за время обрыва что-то могло поменяться
		if time.Since(start) > time.Minute {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < 30*time.Second {
			wait *= 2
		}
	}
}

func (s *Service) refresh(ctx context.Context) {
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	cs, err := s.cli.List(lctx)
	cancel()
	if ctx.Err() != nil {
		return
	}
	var rows []Row
	if err == nil {
		rows = Resolve(cs, s.st.Get())
	}
	s.mu.Lock()
	if err == nil {
		s.rows = rows
	} else {
		// Docker не ответил — правила по последнему списку остаются: адреса
		// без Docker не меняются, а снять их значило бы открыть обход.
		rows = s.rows
	}
	s.listErr, s.updated = err, time.Now()
	s.mu.Unlock()
	if k := getKernel(); k != nil {
		k.SetTargets(Targets(rows))
	}
}

// Ответ GET /api/docker.
type listResp struct {
	Available  bool          `json:"available"`
	Error      string        `json:"error,omitempty"`
	VPN        bool          `json:"vpn"` // слой VPN этой версии умеет правила
	Kernel     *KernelStatus `json:"kernel,omitempty"`
	Containers []Row         `json:"containers"`
	Projects   []Project     `json:"projects"`
	Updated    int64         `json:"updated,omitempty"`
}

func handleList(w http.ResponseWriter, r *http.Request) {
	s := current()
	if s == nil {
		writeErr(w, http.StatusNotFound, "плагин Docker не включён")
		return
	}
	s.mu.Lock()
	if s.updated.IsZero() {
		s.mu.Unlock()
		s.refresh(r.Context())
		s.mu.Lock()
	}
	resp := listResp{Available: s.listErr == nil, Containers: s.rows, Updated: s.updated.Unix()}
	if s.listErr != nil {
		resp.Error = s.listErr.Error()
	}
	rows := s.rows
	s.mu.Unlock()
	if resp.Containers == nil {
		resp.Containers = []Row{}
	}
	resp.Projects = Projects(rows, s.st.Get())
	if resp.Projects == nil {
		resp.Projects = []Project{}
	}
	if k := getKernel(); k != nil {
		st := k.Status()
		resp.VPN, resp.Kernel = true, &st
	}
	writeJSON(w, resp)
}

func handleSet(w http.ResponseWriter, r *http.Request) {
	s := current()
	if s == nil {
		writeErr(w, http.StatusNotFound, "плагин Docker не включён")
		return
	}
	var req struct {
		Container string `json:"container"`
		Project   string `json:"project"`
		Route     string `json:"route"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "плохой запрос: "+err.Error())
		return
	}
	v, ok := ParseChoice(req.Route)
	if !ok || req.Route == "" {
		writeErr(w, http.StatusBadRequest, "route: host, tunnel или direct")
		return
	}
	var err error
	switch {
	case req.Project != "":
		err = s.st.SetProject(req.Project, v)
	case req.Container != "":
		var row *Row
		row, err = s.find(r.Context(), req.Container)
		if err == nil && !row.Switchable {
			err = errors.New("контейнер «" + row.Name + "» не отделить: " + row.Why)
		}
		if err == nil {
			err = s.st.SetContainer(row.Container, v)
		}
	default:
		err = errors.New("нужно container или project")
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.refresh(r.Context())
	writeJSON(w, map[string]bool{"ok": true})
}

// find — контейнер по имени, ID или его началу.
func (s *Service) find(ctx context.Context, q string) (*Row, error) {
	s.refresh(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var hit *Row
	for i := range s.rows {
		c := &s.rows[i]
		if c.Name == q || c.ID == q {
			return c, nil
		}
		if len(q) >= 4 && len(c.ID) >= len(q) && c.ID[:len(q)] == q {
			if hit != nil {
				return nil, errors.New("под «" + q + "» подходит несколько контейнеров")
			}
			hit = c
		}
	}
	if hit == nil {
		return nil, errors.New("нет контейнера «" + q + "»")
	}
	return hit, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
