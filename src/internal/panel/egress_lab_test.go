//go:build netnslab

package panel

// Стенд на настоящем ядре (test/netns/egress.sh, сборка GitHub): выход
// сервера A через сервер B — настоящие скрипты панели, настоящие службы
// systemd (с NetworkNamespacePath=/run/netns/a), настоящий
// ssh_tunnel_vpn_linux и настоящий sshd в пространстве b. Запускается внутри
// пространства a под root:
//
//	ip netns exec a env LAB=… LAB_LINK=… LAB_VPN_BIN=… panel.test -test.run TestEgressLab -test.v
//
// Без переменной LAB тест пропускается: на машине разработчика он не нужен
// (и опасен — меняет маршруты).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type labEgress struct {
	systemEgress
	bin string
}

// Download — клиент из сборки этого же PR, а не из релиза GitHub.
func (l labEgress) Download(_ context.Context, _ string, dst string) error {
	b, err := os.ReadFile(l.bin)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o755)
}

func labSh(cmd string) (string, error) {
	out, err := exec.Command("bash", "-c", cmd).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// labIn — команда в другом пространстве имён.
func labIn(ns, cmd string) (string, error) {
	return labSh("ip netns exec " + ns + " bash -c " + shQuote(cmd))
}

// labSnapshot — всё, что выход сервера может поменять в ядре и системе.
func labSnapshot(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, c := range []string{
		"ip -4 rule", "ip -6 rule",
		"ip -4 route show table all",
		// Адреса fe80:: появляются у карт сами через секунды после подъёма
		// (DAD) — к выходу сервера отношения не имеют.
		"ip -6 route show table all | grep -v 'fe80::\\|^ff00::/8\\|^multicast'",
		"nft list ruleset",
		"sysctl net.ipv4.conf.all.src_valid_mark net.ipv4.conf.all.rp_filter net.ipv4.conf.default.rp_filter net.ipv4.ip_forward",
		"ls /etc/systemd/system | grep ssh_tunnel_egress || true",
		"ls " + egressVarDir + " " + egressBinDir + " 2>&1 || true",
		"ip link show sshtun0 2>&1 | head -1 || true",
	} {
		out, _ := labSh(c)
		fmt.Fprintf(&b, "$ %s\n%s\n", c, out)
	}
	return b.String()
}

func labDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	in := func(l []string, s string) bool {
		for _, x := range l {
			if x == s {
				return true
			}
		}
		return false
	}
	var d []string
	for _, x := range al {
		if !in(bl, x) {
			d = append(d, "- "+x)
		}
	}
	for _, x := range bl {
		if !in(al, x) {
			d = append(d, "+ "+x)
		}
	}
	return strings.Join(d, "\n")
}

func labWait(d time.Duration, f func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return true
		}
		time.Sleep(time.Second)
	}
	return f()
}

// labEcho — каким адресом A виден в «интернете» ("" — не дошёл).
func labEcho() string {
	out, err := labSh("curl -fsS -m 4 http://192.0.2.80/")
	if err != nil {
		return ""
	}
	return out
}

// labInbound — входящие к A снаружи: своя служба и порт «контейнера» за DNAT.
func labInbound(t *testing.T, what string) {
	t.Helper()
	for _, c := range []struct{ name, url string }{
		{"служба самого A", "http://203.0.113.10:8080/"},
		{"опубликованный порт (DNAT)", "http://203.0.113.10:8081/"},
	} {
		out, err := labIn("inet", "curl -fsS -m 5 --interface 192.0.2.99 "+c.url)
		if err != nil || out != "192.0.2.99" {
			t.Errorf("%s: входящее «%s» не получило ответ: %q %v", what, c.name, out, err)
		} else {
			t.Logf("%s: входящее «%s» — ок", what, c.name)
		}
	}
	// SSH к A снаружи — тот же путь, что у панели и пользователей туннеля.
	if out, err := labIn("inet", "exec 3<>/dev/tcp/203.0.113.10/22 && timeout 5 head -c 4 <&3"); err != nil || out != "SSH-" {
		t.Errorf("%s: SSH к A снаружи не ответил: %q %v", what, out, err)
	}
}

func labWaitJob(t *testing.T, m *EgressManager) *egressJob {
	t.Helper()
	var j *egressJob
	labWait(4*time.Minute, func() bool {
		st := m.Status()
		j = st.Job
		return j == nil || !j.Running
	})
	if j != nil {
		t.Logf("включение: этап %s, ошибка %q\n%s", j.Stage, j.Error, strings.Join(j.Lines, "\n"))
	}
	return j
}

func labLogs(t *testing.T) {
	out, _ := labSh("journalctl -u " + egressUnit + " -u " + egressRulesUnit + " -u " + egressRollbackUnit + " --no-pager -n 80 --since '-3min'")
	t.Logf("журнал служб:\n%s", out)
}

func TestEgressLab(t *testing.T) {
	lab := os.Getenv("LAB")
	if lab == "" {
		t.Skip("только на стенде test/netns")
	}
	link, bin := os.Getenv("LAB_LINK"), os.Getenv("LAB_VPN_BIN")
	ipEchoURLs = []string{"http://192.0.2.80/"}
	dir := filepath.Join(lab, "panel-egress")
	os.RemoveAll(dir)
	m := NewEgressManager(labEgress{systemEgress: NewSystemEgress(dir).(systemEgress), bin: bin}, dir, "v0.0.0-lab", nil)

	base := labSnapshot(t)
	if got := labEcho(); got != "203.0.113.10" {
		t.Fatalf("до всего A виден как %q, ждали свой адрес", got)
	}
	labInbound(t, "до включения")
	same := func(what string) {
		t.Helper()
		var d string
		ok := labWait(20*time.Second, func() bool { d = labDiff(base, labSnapshot(t)); return d == "" })
		if !ok {
			t.Errorf("%s: система не вернулась как была:\n%s", what, d)
			labLogs(t)
		} else {
			t.Logf("%s: ip rule, все таблицы, nft, sysctl, службы — как до включения", what)
		}
	}

	// 1. Петля A→B→A: B виден снаружи адресом A — отказ, ничего не тронуто.
	loopNFT := filepath.Join(lab, "loop.nft")
	os.WriteFile(loopNFT, []byte(`table ip lab_loop {
	chain in {
		type nat hook input priority 100; policy accept;
		ip saddr 203.0.113.20 ip daddr 192.0.2.80 snat to 203.0.113.10
	}
}
`), 0o644)
	if out, err := labIn("inet", "nft -f "+loopNFT); err != nil {
		t.Fatalf("подделать петлю: %v %s", err, out)
	}
	if err := m.Apply(link, EgressModeBlock); err != nil {
		t.Fatalf("Apply (петля): %v", err)
	}
	if j := labWaitJob(t, m); j == nil || !strings.Contains(j.Error, "петля") {
		t.Errorf("петля не распознана: %+v", j)
	}
	labIn("inet", "nft delete table ip lab_loop")
	same("петля")

	// 2. Без подтверждения: через минуту всё как было.
	if err := m.Apply(link, EgressModeBlock); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if j := labWaitJob(t, m); j == nil || j.Error != "" {
		labLogs(t)
		t.Fatalf("включение не прошло: %+v", j)
	}
	if !labWait(30*time.Second, func() bool { return labEcho() == "203.0.113.20" }) {
		t.Errorf("после включения A виден как %q, ждали адрес B", labEcho())
		labLogs(t)
	} else {
		t.Log("после включения A виден адресом B")
	}
	labInbound(t, "окно подтверждения")
	if st := m.Status(); st.Phase != EgressApplying {
		t.Errorf("фаза %s, ждали applying", st.Phase)
	}
	t.Log("жду откат без подтверждения…")
	if !labWait(2*time.Minute, func() bool { return m.Status().Phase == EgressOff }) {
		t.Errorf("откат не случился: фаза %s", m.Status().Phase)
		labLogs(t)
	}
	same("откат без подтверждения")
	if m.Status().RolledBackAt == 0 {
		t.Error("страница не узнает об откате: нет отметки rolled-back")
	}
	if got := labEcho(); got != "203.0.113.10" {
		t.Errorf("после отката A виден как %q", got)
	}

	// 3. С подтверждением, переживает перезапуск служб.
	if err := m.Apply("", EgressModeBlock); err != nil {
		t.Fatalf("повторный Apply (прежний B): %v", err)
	}
	if j := labWaitJob(t, m); j == nil || j.Error != "" {
		labLogs(t)
		t.Fatalf("повторное включение не прошло: %+v", j)
	}
	time.Sleep(egressConfirmMin + time.Second)
	if err := m.Confirm(m.Status().Token); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !labWait(30*time.Second, func() bool { return m.Status().Phase == EgressOn && labEcho() == "203.0.113.20" }) {
		t.Errorf("после подтверждения: фаза %s, адрес %q", m.Status().Phase, labEcho())
		labLogs(t)
	}
	if out, _ := labSh("systemctl is-enabled " + egressUnit + " " + egressRulesUnit); strings.Count(out, "enabled") != 2 {
		t.Errorf("после подтверждения службы не в автозапуске: %s", out)
	}
	labInbound(t, "подтверждено")
	if out, err := labSh("systemctl restart " + egressRulesUnit + " " + egressUnit); err != nil {
		t.Errorf("перезапуск служб: %v %s", err, out)
	}
	if !labWait(40*time.Second, func() bool { return labEcho() == "203.0.113.20" }) {
		t.Errorf("после перезапуска служб A виден как %q", labEcho())
		labLogs(t)
	} else {
		t.Log("перезапуск служб пережит")
	}
	labInbound(t, "после перезапуска служб")

	// 4. B упал: «сервер без интернета» — блок; «выход напрямую» — напрямую.
	labSh("bash -c 'source " + os.Getenv("LAB_SH") + " && kill_b'")
	time.Sleep(3 * time.Second)
	if got := labEcho(); got != "" {
		t.Errorf("B упал, режим «без интернета»: A всё равно вышел как %q", got)
	} else {
		t.Log("B упал, «без интернета»: наружу не выходит")
	}
	labInbound(t, "B упал, без интернета")
	if !labWait(60*time.Second, func() bool { return m.Status().Phase == EgressDown }) {
		t.Errorf("фаза при упавшем B: %s, ждали down", m.Status().Phase)
	}
	if err := m.SetMode(EgressModeDirect); err != nil {
		t.Errorf("SetMode(direct): %v", err)
	}
	if !labWait(90*time.Second, func() bool { return labEcho() == "203.0.113.10" }) {
		out, _ := labSh("ip -4 rule; ip -4 route show table 7098")
		t.Errorf("B упал, режим «напрямую»: A виден как %q, ждали свой адрес\n%s", labEcho(), out)
		labLogs(t)
	} else {
		t.Log("B упал, «напрямую»: выходит своим адресом")
	}
	labInbound(t, "B упал, напрямую")
	if err := m.SetMode(EgressModeBlock); err != nil {
		t.Errorf("SetMode(block): %v", err)
	}

	// B вернулся — клиент поднимается (служба перезапускается сама или по
	// надзору панели).
	if _, err := labSh("bash -c 'source " + os.Getenv("LAB_SH") + " && start_b'"); err != nil {
		t.Errorf("поднять B: %v", err)
	}
	labSh("systemctl restart " + egressUnit)
	if !labWait(60*time.Second, func() bool { return labEcho() == "203.0.113.20" }) {
		t.Errorf("B вернулся, а A виден как %q", labEcho())
		labLogs(t)
	}

	// 5. Выключить — точно как было.
	if err := m.Off(); err != nil {
		t.Errorf("Off: %v", err)
	}
	same("выключение")

	// 6. Скрипт включения умер посередине — откат всё равно случается.
	for _, after := range []string{
		"systemctl enable --now " + egressRollbackTmr,
		"systemctl start " + egressRulesUnit,
	} {
		labKilledApply(t, m, link, after)
		t.Logf("жду откат после смерти скрипта после «%s»…", after)
		if !labWait(2*time.Minute, func() bool {
			_, err := os.Stat(egressVarDir)
			return errors.Is(err, os.ErrNotExist)
		}) {
			t.Errorf("скрипт убит после «%s»: откат не случился", after)
			labLogs(t)
		}
		same("скрипт убит после «" + after + "»")
	}
}

// labKilledApply — скрипт включения, убитый (kill -9 самого себя) сразу
// после строки after: так же, как если бы процесс умер посередине.
func labKilledApply(t *testing.T, m *EgressManager, link, after string) {
	t.Helper()
	facts, err := m.sys.Facts()
	if err != nil {
		t.Fatal(err)
	}
	facts.Direct = nil
	plan, err := planEgress(facts, true)
	if err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(m.dir, "bin.new")
	if err := m.sys.Download(context.Background(), "", newBin); err != nil {
		t.Fatal(err)
	}
	script := egressApplyScript(egressApplyInput{
		NewBinary: newBin, Version: "lab", Host: "203.0.113.20", Port: 22, User: "tunlab",
		KeyFile: m.keyPath(), MarkDir: m.dir, Plan: plan,
	})
	if !strings.Contains(script, after+"\n") {
		t.Fatalf("в скрипте нет строки %q", after)
	}
	script = strings.Replace(script, after+"\n", after+"\nkill -9 $$\n", 1)
	err = m.sys.RunScript(script, func(string) {})
	t.Logf("скрипт убит после «%s»: %v", after, err)
	if _, err := os.Stat(egressVarDir + "/pending"); err != nil {
		t.Errorf("после убитого скрипта нет pending — откату нечего делать: %v", err)
	}
	if out, _ := labSh("systemctl is-active " + egressRollbackTmr); out != "active" {
		t.Errorf("после убитого скрипта таймер отката не взведён: %s", out)
	}
	_ = link
}
