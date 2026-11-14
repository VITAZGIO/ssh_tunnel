package panel

// Выход самого сервера через другой сервер (ТЗ 10): кнопка в «Настройках».
//
// Как включается. В панели сервера B заводится обычный клиент (как для
// устройства), его ссылку sshtunnel://… вставляют в карточку панели A.
// Панель A:
//  1. проверяет ссылку и сам B: заходит на него по SSH с этим ключом и
//     спрашивает «какой у меня адрес снаружи» через B. Совпал с адресом
//     самого A — B выходит в интернет через A (петля), отказ;
//  2. скачивает Linux-клиент VPN той же версии, что панель, и сверяет
//     контрольную сумму (свой файл в /usr/local/lib/ssh_tunnel_egress —
//     клиент, поставленный человеком, не трогается);
//  3. ставит правила (egress_plan.go), клиент и таймер отката
//     (egress_units.go). С этого момента идёт окно в 60 секунд;
//  4. страница, увидев «применяется», шлёт подтверждение новым запросом —
//     раз он дошёл, панель снаружи доступна. Нет подтверждения — таймер
//     systemd возвращает всё как было.
//
// Состояние на экране берётся с самой системы (файлы, службы, таблица
// маршрутов), а не из памяти панели: откат делает не она.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sshtunnel/internal/share"
)

// Фазы на экране.
const (
	EgressOff        = "off"        // выключено
	EgressInstalling = "installing" // проверка, скачивание, установка
	EgressApplying   = "applying"   // применено, идёт окно подтверждения
	EgressRollback   = "rollback"   // окно кончилось, таймер откатывает
	EgressOn         = "on"         // работает через B
	EgressDown       = "down"       // включено, но B недоступен
)

// Поведение, когда B недоступен.
const (
	EgressModeBlock  = "block"  // «сервер без интернета» (по умолчанию)
	EgressModeDirect = "direct" // «выход напрямую»
)

// egressConfirmMin — подтверждение раньше этого не принимается: оно должно
// прийти уже по новым маршрутам, а не «на лету» тем же запросом.
const egressConfirmMin = 3 * time.Second

// egressSysState — то, что видно на системе.
type egressSysState struct {
	// Pending — когда началось окно отката (unix); 0 — окна нет.
	Pending      int64
	RulesActive  bool
	RulesEnabled bool
	ClientActive bool
	// RoutesUp — клиент поставил маршруты в свою таблицу: связь с B есть.
	RoutesUp bool
	// Conflict — на сервере уже есть другой клиент VPN ssh_tunnel.
	Conflict string
	// Version — версия своего клиента ("" — не стоит).
	Version string
	// RolledBackAt — когда был последний откат по таймеру (unix).
	RolledBackAt int64
}

// egressPhase — фаза по состоянию системы.
func egressPhase(st egressSysState, now time.Time) string {
	switch {
	case st.Pending > 0:
		if now.Unix() < st.Pending+egressWindowSec {
			return EgressApplying
		}
		return EgressRollback
	case !st.RulesActive && !st.RulesEnabled:
		return EgressOff
	case st.ClientActive && st.RoutesUp:
		return EgressOn
	default:
		return EgressDown
	}
}

// egressSystem — всё, что выход сервера делает с системой и сетью.
type egressSystem interface {
	RunScript(script string, onLine func(string)) error
	State() egressSysState
	Facts() (egressFacts, error)
	Restart(unit string) error
	// PublicIP — адрес, с которым этот сервер виден снаружи (по текущим
	// маршрутам: без выхода — свой, с выходом — адрес B).
	PublicIP(ctx context.Context) (string, error)
	// PublicIPVia — то же, но через B по SSH (ещё до включения).
	PublicIPVia(ctx context.Context, t EgressTarget, key []byte) (ip, knownHost string, err error)
	// Download качает клиент версии version для этой архитектуры в dst и
	// сверяет контрольную сумму.
	Download(ctx context.Context, version, dst string) error
	Resolve(ctx context.Context, host string) ([]netip.Addr, error)
	// Reachable — отвечает ли B на порт SSH с этого сервера (мимо туннеля:
	// правило «to B lookup main»).
	Reachable(ctx context.Context, ip string, port int) bool
}

// EgressTarget — сервер B.
type EgressTarget struct {
	Name string `json:"name"`
	Host string `json:"host"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
	User string `json:"user"`
}

type egressConfig struct {
	Target *EgressTarget `json:"target,omitempty"`
	Mode   string        `json:"mode"`
}

// EgressStatus — ответ GET /api/egress.
type EgressStatus struct {
	Phase     string        `json:"phase"`
	Mode      string        `json:"mode"`
	Target    *EgressTarget `json:"target,omitempty"`
	Remaining int           `json:"remaining,omitempty"`
	// Token — подтверждение этого окна (только в фазе applying).
	Token        string     `json:"token,omitempty"`
	ExitIP       string     `json:"exitIp,omitempty"`
	SelfIP       string     `json:"selfIp,omitempty"`
	Conflict     string     `json:"conflict,omitempty"`
	RolledBackAt int64      `json:"rolledBackAt,omitempty"`
	Version      string     `json:"clientVersion,omitempty"`
	Job          *egressJob `json:"job,omitempty"`
}

type egressJob struct {
	Running bool     `json:"running"`
	Stage   string   `json:"stage"`
	Error   string   `json:"error,omitempty"`
	Lines   []string `json:"lines,omitempty"`
}

// EgressManager — выход сервера в панели.
type EgressManager struct {
	sys     egressSystem
	dir     string // папка панели для ключа и настроек выхода
	version string
	// direct — куда всегда напрямую сверх B (главный сервер сети устройств).
	direct func() []string
	now    func() time.Time

	mu      sync.Mutex
	cfg     egressConfig
	job     *egressJob
	token   string
	exitIP  string
	exitAt  time.Time
	selfIP  string
	downAt  time.Time
	restart time.Time
	backoff time.Duration

	// Доступность B по надзору (tick). Клиент VPN, потеряв B, держит свои
	// маршруты, пока переподключается (так на ПК нет утечки мимо туннеля), —
	// по маршрутам падение B не видно. bFails — проверок подряд без ответа.
	bFails int
	bDown  bool
	// catchAllOff — снято правило «всё остальное — в туннель» (режим
	// «выход напрямую» при недоступном B).
	catchAllOff bool
}

// egressDownAfter — сколько проверок подряд без ответа B считать его
// недоступным (проверка раз в 15 с).
const egressDownAfter = 2

// NewEgressManager — dir: папка данных панели/egress.
func NewEgressManager(sys egressSystem, dir, version string, direct func() []string) *EgressManager {
	m := &EgressManager{sys: sys, dir: dir, version: version, direct: direct, now: time.Now}
	m.cfg.Mode = EgressModeBlock
	if data, err := os.ReadFile(m.cfgPath()); err == nil {
		json.Unmarshal(data, &m.cfg)
	}
	if m.cfg.Mode != EgressModeDirect {
		m.cfg.Mode = EgressModeBlock
	}
	return m
}

func (m *EgressManager) cfgPath() string { return filepath.Join(m.dir, "egress.json") }
func (m *EgressManager) keyPath() string { return filepath.Join(m.dir, "key") }

func (m *EgressManager) saveLocked() error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(m.cfg, "", "  ")
	return os.WriteFile(m.cfgPath(), data, 0o600)
}

// Status — что показать.
func (m *EgressManager) Status() EgressStatus {
	st := m.sys.State()
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	out := EgressStatus{Phase: egressPhase(st, now), Mode: m.cfg.Mode, Target: m.cfg.Target,
		Conflict: st.Conflict, RolledBackAt: st.RolledBackAt, Version: st.Version, SelfIP: m.selfIP}
	if m.job != nil {
		j := *m.job
		out.Job = &j
		if j.Running {
			out.Phase = EgressInstalling
		}
	}
	if out.Phase == EgressOn && m.bDown {
		out.Phase = EgressDown
	}
	switch out.Phase {
	case EgressApplying:
		out.Remaining = int(st.Pending + egressWindowSec - now.Unix())
		out.Token = m.token
	case EgressOn:
		out.ExitIP = m.exitIP
	}
	return out
}

func (m *EgressManager) jobLine(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil {
		return
	}
	m.job.Lines = append(m.job.Lines, s)
	if len(m.job.Lines) > 60 {
		m.job.Lines = m.job.Lines[len(m.job.Lines)-60:]
	}
}

func (m *EgressManager) jobStage(s string) {
	m.mu.Lock()
	if m.job != nil {
		m.job.Stage = s
	}
	m.mu.Unlock()
}

// Apply начинает включение через сервер из ссылки (пустая — прежний B).
// Работа идёт фоном; ошибки — в Status().Job.Error.
func (m *EgressManager) Apply(link, mode string) error {
	if mode != "" && mode != EgressModeBlock && mode != EgressModeDirect {
		return errors.New("неизвестное поведение без сервера")
	}
	st := m.sys.State()
	if st.Conflict != "" {
		return errors.New(st.Conflict)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil && m.job.Running {
		return errors.New("уже идёт включение")
	}
	if ph := egressPhase(st, m.now()); ph != EgressOff {
		return errors.New("выход через другой сервер уже включён — сначала выключи его")
	}
	var doc *share.Doc
	if strings.TrimSpace(link) != "" {
		d, err := share.Parse([]byte(strings.TrimSpace(link)))
		if err != nil {
			return err
		}
		if !d.KeyIncluded || d.KeyContents == "" {
			return errors.New("в ссылке нет ключа — нужна ссылка нового клиента из панели того сервера (её показывают сразу после создания)")
		}
		if err := validHost(d.Host); err != nil {
			return err
		}
		if d.User == "" {
			return errors.New("в ссылке нет пользователя")
		}
		doc = &d
	} else if m.cfg.Target == nil {
		return errors.New("вставь ссылку клиента из панели сервера, через который выходить")
	} else if _, err := os.Stat(m.keyPath()); err != nil {
		return errors.New("ключ прежнего сервера потерян — вставь ссылку заново")
	}
	if mode == "" {
		mode = m.cfg.Mode
	}
	m.job = &egressJob{Running: true, Stage: "check"}
	m.token = ""
	go m.runApply(doc, mode)
	return nil
}

func (m *EgressManager) fail(err error) {
	m.mu.Lock()
	if m.job != nil {
		m.job.Running, m.job.Error = false, err.Error()
	}
	m.mu.Unlock()
}

func (m *EgressManager) runApply(doc *share.Doc, mode string) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	m.mu.Lock()
	var target EgressTarget
	if m.cfg.Target != nil {
		target = *m.cfg.Target
	}
	m.mu.Unlock()
	var key []byte
	if doc != nil {
		target = EgressTarget{Name: cleanName(doc.Name), Host: doc.Host, Port: doc.SSHPort, User: doc.User}
		key = []byte(doc.KeyContents)
	} else {
		var err error
		if key, err = os.ReadFile(m.keyPath()); err != nil {
			m.fail(errors.New("ключ прежнего сервера потерян — вставь ссылку заново"))
			return
		}
	}
	if target.Port == 0 {
		target.Port = 22
	}

	// Адрес B — числом: профиль клиента не должен зависеть от DNS, который
	// при «сервере без интернета» без B не работает.
	ip, err := m.resolveOne(ctx, target.Host)
	if err != nil {
		m.fail(err)
		return
	}
	target.IP = ip.String()
	facts, err := m.sys.Facts()
	if err != nil {
		m.fail(err)
		return
	}
	for _, a := range facts.Local {
		if a.Unmap() == ip {
			m.fail(errors.New("это адрес самого этого сервера — выходить через себя нельзя"))
			return
		}
	}

	m.jobStage("probe")
	self, err := m.sys.PublicIP(ctx)
	if err != nil {
		m.fail(fmt.Errorf("не узнал адрес этого сервера снаружи: %w", err))
		return
	}
	via, known, err := m.sys.PublicIPVia(ctx, target, key)
	if err != nil {
		m.fail(fmt.Errorf("сервер %s не пустил или не выпустил в интернет: %w", target.Host, err))
		return
	}
	if err := checkEgressLoop(self, via, facts.Local); err != nil {
		m.fail(err)
		return
	}
	m.mu.Lock()
	m.selfIP = self
	m.mu.Unlock()
	m.jobLine("снаружи этот сервер виден как " + self + ", через " + target.Host + " — как " + via)

	newBin := ""
	if m.sys.State().Version != m.version {
		m.jobStage("download")
		if err := os.MkdirAll(m.dir, 0o700); err != nil {
			m.fail(err)
			return
		}
		newBin = filepath.Join(m.dir, "ssh_tunnel_vpn_linux.new")
		if err := m.sys.Download(ctx, m.version, newBin); err != nil {
			os.Remove(newBin)
			m.fail(err)
			return
		}
		defer os.Remove(newBin)
	}
	if doc != nil {
		if err := os.MkdirAll(m.dir, 0o700); err == nil {
			err = os.WriteFile(m.keyPath(), key, 0o600)
		}
		if err != nil {
			m.fail(fmt.Errorf("не сохранил ключ: %w", err))
			return
		}
	}

	direct := []netip.Addr{ip}
	if m.direct != nil {
		for _, h := range m.direct() {
			if a, err := m.resolveOne(ctx, h); err == nil {
				direct = append(direct, a)
			}
		}
	}
	facts.Direct = direct
	plan, err := planEgress(facts, mode != EgressModeDirect)
	if err != nil {
		m.fail(err)
		return
	}

	m.jobStage("apply")
	script := egressApplyScript(egressApplyInput{
		NewBinary: newBin, Version: m.version, Host: target.IP, Port: target.Port, User: target.User,
		KeyFile: m.keyPath(), KnownHost: known, MarkDir: m.dir, Plan: plan,
	})
	os.Remove(filepath.Join(m.dir, "rolled-back"))
	if err := m.sys.RunScript(script, m.jobLine); err != nil {
		// Что успело встать — снимет таймер; ждать его минуту незачем.
		m.sys.RunScript(egressOffNowScript(), m.jobLine)
		m.fail(err)
		return
	}
	m.mu.Lock()
	m.cfg.Target, m.cfg.Mode = &target, mode
	m.saveLocked()
	m.token = randomHex(12)
	m.exitIP, m.exitAt = "", time.Time{}
	m.job.Running, m.job.Stage = false, "done"
	m.mu.Unlock()
}

func (m *EgressManager) resolveOne(ctx context.Context, host string) (netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap(), nil
	}
	addrs, err := m.sys.Resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("не нашёл адрес сервера %s", host)
	}
	for _, a := range addrs {
		if a.Unmap().Is4() {
			return a.Unmap(), nil
		}
	}
	return addrs[0], nil
}

// checkEgressLoop — B виден снаружи под адресом этого сервера: значит, B
// (или кто-то дальше по его цепочке) выходит в интернет через нас.
func checkEgressLoop(self, via string, local []netip.Addr) error {
	v, err := netip.ParseAddr(strings.TrimSpace(via))
	if err != nil {
		return fmt.Errorf("через тот сервер пришёл странный адрес %q", via)
	}
	v = v.Unmap()
	if s, err := netip.ParseAddr(strings.TrimSpace(self)); err == nil && s.Unmap() == v {
		return errors.New("петля: тот сервер сам выходит в интернет через этот (снаружи у обоих один адрес " + v.String() + "). Выключи выход на том сервере или выбери другой")
	}
	for _, a := range local {
		if a.Unmap() == v {
			return errors.New("петля: тот сервер выходит в интернет через этот (" + v.String() + ")")
		}
	}
	return nil
}

// Confirm — подтверждение из браузера: панель доступна по новым маршрутам.
func (m *EgressManager) Confirm(token string) error {
	st := m.sys.State()
	m.mu.Lock()
	now := m.now()
	if egressPhase(st, now) != EgressApplying {
		m.mu.Unlock()
		return errors.New("подтверждать нечего — окно уже закрылось")
	}
	if token == "" || token != m.token {
		m.mu.Unlock()
		return errors.New("это подтверждение не от текущего включения — обнови страницу")
	}
	if now.Sub(time.Unix(st.Pending, 0)) < egressConfirmMin {
		m.mu.Unlock()
		return errors.New("рано: подтверждение должно прийти уже по новым маршрутам")
	}
	m.token = ""
	m.mu.Unlock()
	return m.sys.RunScript(egressConfirmScript(), func(string) {})
}

// Off — выключить и убрать всё.
func (m *EgressManager) Off() error {
	m.mu.Lock()
	if m.job != nil && m.job.Running {
		m.mu.Unlock()
		return errors.New("идёт включение — дождись конца")
	}
	m.token, m.exitIP = "", ""
	m.bFails, m.bDown, m.catchAllOff = 0, false, false
	m.mu.Unlock()
	return m.sys.RunScript(egressOffNowScript(), func(string) {})
}

// SetMode — поведение, когда B недоступен. Включённый выход переставляет
// правила сразу; входящие соединения это не затрагивает.
func (m *EgressManager) SetMode(mode string) error {
	if mode != EgressModeBlock && mode != EgressModeDirect {
		return errors.New("неизвестное поведение без сервера")
	}
	st := m.sys.State()
	m.mu.Lock()
	if m.job != nil && m.job.Running {
		m.mu.Unlock()
		return errors.New("идёт включение — дождись конца")
	}
	m.cfg.Mode = mode
	err := m.saveLocked()
	target := m.cfg.Target
	// rules-up.sh ставит правила целиком, с «всё остальное — в туннель»; при
	// недоступном B в режиме «напрямую» надзор снимет его снова.
	m.catchAllOff = false
	m.mu.Unlock()
	if err != nil || egressPhase(st, m.now()) == EgressOff || target == nil {
		return err
	}
	facts, err := m.sys.Facts()
	if err != nil {
		return err
	}
	direct := []netip.Addr{}
	if a, err := netip.ParseAddr(target.IP); err == nil {
		direct = append(direct, a)
	}
	if m.direct != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		for _, h := range m.direct() {
			if a, err := m.resolveOne(ctx, h); err == nil {
				direct = append(direct, a)
			}
		}
		cancel()
	}
	facts.Direct = direct
	plan, err := planEgress(facts, mode == EgressModeBlock)
	if err != nil {
		return err
	}
	return m.sys.RunScript(egressModeScript(plan), func(string) {})
}

// watchB — проверить, жив ли B, и в режиме «выход напрямую» снять или
// вернуть правило «всё остальное — в туннель». Клиент VPN при упавшем B
// маршруты в свою таблицу не снимает (переподключается), поэтому без этого
// «напрямую» не наступило бы вовсе (нашёл стенд test/netns, ТЗ 14).
func (m *EgressManager) watchB(ctx context.Context, t EgressTarget, mode string) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	up := m.sys.Reachable(c, t.IP, t.Port)
	cancel()
	m.mu.Lock()
	if up {
		m.bFails = 0
	} else {
		m.bFails++
	}
	m.bDown = m.bFails >= egressDownAfter
	var script string
	switch {
	case mode == EgressModeDirect && m.bDown && !m.catchAllOff:
		script, m.catchAllOff = egressCatchAllScript(false), true
	case m.catchAllOff && (!m.bDown || mode != EgressModeDirect):
		script, m.catchAllOff = egressCatchAllScript(true), false
	}
	m.mu.Unlock()
	if script != "" {
		m.sys.RunScript(script, func(string) {})
	}
}

// Start — надзор: адрес снаружи и перезапуск клиента, если B пропал.
func (m *EgressManager) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			m.tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// tick — один шаг надзора. Клиент VPN, потеряв сервер насовсем, гасит
// туннель и сам не поднимает (так он ведёт себя и на ПК); на сервере
// человека рядом нет — перезапускаем службу с растущей паузой.
func (m *EgressManager) tick(ctx context.Context) {
	st := m.sys.State()
	m.mu.Lock()
	now := m.now()
	phase := egressPhase(st, now)
	if m.job != nil && m.job.Running {
		m.mu.Unlock()
		return
	}
	wantIP := false
	doRestart := false
	switch phase {
	case EgressOn:
		m.downAt, m.backoff = time.Time{}, 0
		wantIP = now.Sub(m.exitAt) > time.Minute
	case EgressDown:
		m.exitIP = ""
		if m.downAt.IsZero() {
			m.downAt = now
		}
		if now.Sub(m.downAt) > 40*time.Second && !now.Before(m.restart) {
			if m.backoff == 0 {
				m.backoff = time.Minute
			} else if m.backoff < 5*time.Minute {
				m.backoff *= 2
			}
			m.restart = now.Add(m.backoff)
			doRestart = true
		}
	case EgressOff:
		m.downAt, m.backoff, m.exitIP = time.Time{}, 0, ""
		wantIP = m.selfIP == "" || now.Sub(m.exitAt) > 30*time.Minute
	}
	target, mode := m.cfg.Target, m.cfg.Mode
	m.mu.Unlock()
	if doRestart {
		m.sys.Restart(egressUnit)
	}
	if (phase == EgressOn || phase == EgressDown) && target != nil {
		m.watchB(ctx, *target, mode)
	} else {
		m.mu.Lock()
		m.bFails, m.bDown, m.catchAllOff = 0, false, false
		m.mu.Unlock()
	}
	if wantIP {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		ip, err := m.sys.PublicIP(c)
		cancel()
		m.mu.Lock()
		m.exitAt = m.now()
		if err == nil {
			if phase == EgressOn {
				m.exitIP = ip
			} else {
				m.selfIP = ip
			}
		}
		m.mu.Unlock()
	}
}
