package panel

import (
	"context"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"sshtunnel/internal/share"
)

// fakeEgressSystem — egressSystem в памяти для тестов EgressManager: без
// root, без сети, без systemd.
type fakeEgressSystem struct {
	mu sync.Mutex

	st      egressSysState
	facts   egressFacts
	factsOK bool
	selfIP  string
	viaIP   string
	viaErr  error
	scripts []string
	restart int

	// confirmEffect — после скрипта подтверждения.
	onScript func(script string)
}

func newFakeEgressSystem() *fakeEgressSystem {
	return &fakeEgressSystem{
		facts:   egressFacts{Uplinks: []string{"eth0"}, Local: []netip.Addr{mustParseAddr("203.0.113.1")}},
		factsOK: true,
		selfIP:  "203.0.113.1",
		viaIP:   "198.51.100.9",
	}
}

func mustParseAddr(s string) netip.Addr {
	a, err := netip.ParseAddr(s)
	if err != nil {
		panic(err)
	}
	return a
}

func (f *fakeEgressSystem) RunScript(script string, onLine func(string)) error {
	f.mu.Lock()
	f.scripts = append(f.scripts, script)
	cb := f.onScript
	switch {
	case strings.Contains(script, "жду подтверждения из панели"):
		// egressApplyScript: правила встали, таймер отката пошёл.
		f.st = egressSysState{Pending: time.Now().Unix(), RulesActive: true, ClientActive: true, RoutesUp: true}
	case strings.Contains(script, "подтверждено: выход сервера включён"):
		// egressConfirmScript: окно закрыто, всё остаётся включённым.
		f.st.Pending, f.st.RulesEnabled = 0, true
	case strings.Contains(script, "выключить выход сервера"):
		// egressOffScript (через egressOffNowScript).
		f.st = egressSysState{}
	}
	f.mu.Unlock()
	if cb != nil {
		cb(script)
	}
	return nil
}

func (f *fakeEgressSystem) State() egressSysState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeEgressSystem) setState(st egressSysState) {
	f.mu.Lock()
	f.st = st
	f.mu.Unlock()
}

func (f *fakeEgressSystem) Facts() (egressFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.factsOK {
		return egressFacts{}, errExpected
	}
	return f.facts, nil
}

func (f *fakeEgressSystem) Restart(unit string) error {
	f.mu.Lock()
	f.restart++
	f.mu.Unlock()
	return nil
}

func (f *fakeEgressSystem) PublicIP(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.selfIP, nil
}

func (f *fakeEgressSystem) PublicIPVia(ctx context.Context, t EgressTarget, key []byte) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.viaErr != nil {
		return "", "", f.viaErr
	}
	return f.viaIP, "known-hosts-line", nil
}

func (f *fakeEgressSystem) Download(ctx context.Context, version, dst string) error {
	return os.WriteFile(dst, []byte("fake"), 0o700)
}

func (f *fakeEgressSystem) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	return []netip.Addr{mustParseAddr("198.51.100.9")}, nil
}

var errExpected = &testErr{"нет карт"}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }

func testLink(t *testing.T, host, user, key string) string {
	t.Helper()
	data, err := share.Build(share.Doc{
		Name: "B", Host: host, SSHPort: 22, User: user, SocksPort: 1080, HTTPPort: 1081,
		PoolSize: 4, FilterMode: "all", KeyIncluded: true, KeyContents: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func newTestEgressManager(t *testing.T, sys *fakeEgressSystem) *EgressManager {
	t.Helper()
	dir := t.TempDir()
	return NewEgressManager(sys, dir, "v1.9.0", nil)
}

func waitJobDone(t *testing.T, m *EgressManager) EgressStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Status()
		if st.Job == nil || !st.Job.Running {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("включение не завершилось за отведённое время")
	return EgressStatus{}
}

func TestEgressApplyУспехИПодтверждение(t *testing.T) {
	sys := newFakeEgressSystem()
	m := newTestEgressManager(t, sys)

	link := testLink(t, "198.51.100.9", "tun_0000000000000001", "демо-ключ")
	if err := m.Apply(link, EgressModeBlock); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	st := waitJobDone(t, m)
	if st.Job.Error != "" {
		t.Fatalf("включение упало: %s", st.Job.Error)
	}
	if st.Phase != EgressApplying {
		t.Fatalf("фаза = %q, ожидалось applying", st.Phase)
	}
	if st.Token == "" {
		t.Fatal("нет токена подтверждения")
	}

	// Подтверждать слишком рано (меньше egressConfirmMin) нельзя.
	sys.setState(egressSysState{Pending: time.Now().Unix()})
	if err := m.Confirm(st.Token); err == nil {
		t.Fatal("подтверждение «прямо сейчас» должно быть отказом")
	}

	sys.setState(egressSysState{Pending: time.Now().Add(-egressConfirmMin - time.Second).Unix()})
	if err := m.Confirm(st.Token); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	sys.mu.Lock()
	n := len(sys.scripts)
	sys.mu.Unlock()
	if n < 2 {
		t.Fatalf("ожидалось минимум 2 скрипта (apply + confirm), выполнено %d", n)
	}
}

func TestEgressApplyОтказываетНаПетлю(t *testing.T) {
	sys := newFakeEgressSystem()
	sys.selfIP = "198.51.100.9"
	sys.viaIP = "198.51.100.9" // B видит нас тем же адресом — петля
	m := newTestEgressManager(t, sys)

	link := testLink(t, "203.0.113.50", "tun_0000000000000002", "демо-ключ")
	if err := m.Apply(link, ""); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	st := waitJobDone(t, m)
	if st.Job.Error == "" {
		t.Fatal("петля должна быть отловлена как ошибка")
	}
}

func TestEgressApplyБезКлючаВСсылке(t *testing.T) {
	sys := newFakeEgressSystem()
	m := newTestEgressManager(t, sys)
	data, _ := share.Build(share.Doc{Name: "B", Host: "203.0.113.50", SSHPort: 22, User: "tun_x"})
	if err := m.Apply(string(data), ""); err == nil {
		t.Fatal("ссылка без ключа должна быть отказом сразу, без фоновой работы")
	}
}

func TestEgressApplyОтказываетПриКонфликте(t *testing.T) {
	sys := newFakeEgressSystem()
	sys.setState(egressSysState{Conflict: "занято другим клиентом"})
	m := newTestEgressManager(t, sys)
	link := testLink(t, "203.0.113.50", "tun_0000000000000003", "демо-ключ")
	if err := m.Apply(link, ""); err == nil {
		t.Fatal("конфликт с другим клиентом VPN должен остановить включение сразу")
	}
}

func TestEgressOffИSetMode(t *testing.T) {
	sys := newFakeEgressSystem()
	m := newTestEgressManager(t, sys)
	if err := m.Off(); err != nil {
		t.Fatalf("Off на выключенном не должен быть ошибкой: %v", err)
	}
	if err := m.SetMode(EgressModeDirect); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if err := m.SetMode("что-то не то"); err == nil {
		t.Fatal("неизвестный режим должен быть отказом")
	}
	// Сохранился на диске.
	m2 := NewEgressManager(sys, m.dir, "v1.9.0", nil)
	if m2.cfg.Mode != EgressModeDirect {
		t.Fatalf("режим не сохранился: %q", m2.cfg.Mode)
	}
}

func TestEgressTickПерезапускаетПослеТаймаута(t *testing.T) {
	sys := newFakeEgressSystem()
	m := newTestEgressManager(t, sys)
	m.cfg.Target = &EgressTarget{Host: "203.0.113.50"}
	frozen := time.Now()
	m.now = func() time.Time { return frozen }

	sys.setState(egressSysState{RulesActive: true, RulesEnabled: true, ClientActive: false, RoutesUp: false})
	m.tick(context.Background())
	if sys.restart != 0 {
		t.Fatal("перезапуск раньше 40 секунд простоя быть не должен")
	}
	frozen = frozen.Add(41 * time.Second)
	m.now = func() time.Time { return frozen }
	m.tick(context.Background())
	if sys.restart != 1 {
		t.Fatalf("ожидался 1 перезапуск, случилось %d", sys.restart)
	}
	// Повторный tick раньше backoff не должен перезапускать снова.
	frozen = frozen.Add(5 * time.Second)
	m.now = func() time.Time { return frozen }
	m.tick(context.Background())
	if sys.restart != 1 {
		t.Fatalf("повторный перезапуск раньше backoff — ожидался 1, получено %d", sys.restart)
	}
}

func TestEgressVarDirНачинаетсяОтКорня(t *testing.T) {
	// Пишут и читают его только скрипты на самом Linux-сервере (не этот
	// процесс) — проверяем только форму строки, не семантику ОС, на
	// которой собираются тесты.
	if !strings.HasPrefix(egressVarDir, "/") {
		t.Fatalf("egressVarDir должен быть путём Linux от корня: %q", egressVarDir)
	}
}
