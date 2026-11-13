package panel

// Выход сервера через другой сервер: скрипты и службы systemd.
//
// Откат не зависит ни от процесса панели, ни от сети: это таймер systemd
// (OnActiveSec — 60 с) и простой скрипт sh. Пока лежит файл
// pending, таймер через минуту снимает всё (off.sh). Подтверждение (новый
// запрос к панели из браузера) убирает pending, гасит таймер и только тогда
// включает службы в автозапуск. До подтверждения службы запущены, но не
// включены: перезагрузка посреди окна поднимет сервер без них, а таймер
// после загрузки уберёт остатки.

import (
	"fmt"
	"strings"
)

// shQuote — строка в одинарных кавычках для sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// egressUnitPaths — все наши юниты (off.sh убирает их целиком).
var egressUnitNames = []string{egressUnit, egressRulesUnit, egressRollbackUnit, egressRollbackTmr}

func egressRulesUnitText() string {
	return `# ssh_tunnel: правила выхода сервера через другой сервер. Создано панелью.
[Unit]
Description=ssh_tunnel - правила выхода сервера через другой сервер
After=network-online.target
Wants=network-online.target
Before=` + egressUnit + `

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh ` + egressVarDir + `/rules-up.sh
ExecStop=/bin/sh ` + egressVarDir + `/rules-down.sh

[Install]
WantedBy=multi-user.target
`
}

func egressClientUnitText() string {
	return fmt.Sprintf(`# ssh_tunnel: выход сервера через другой сервер (клиент VPN). Создано панелью.
[Unit]
Description=ssh_tunnel - выход этого сервера через другой сервер
Requires=%[1]s
After=%[1]s network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=XDG_CONFIG_HOME=%[2]s/config
Environment=SSH_TUNNEL_ROUTE_TABLE=%[3]d
ExecStart=%[4]s
Restart=always
RestartSec=10
KillSignal=SIGTERM
TimeoutStopSec=15

[Install]
WantedBy=multi-user.target
`, egressRulesUnit, egressVarDir, egressTable, egressBinPath)
}

func egressRollbackUnitText() string {
	return `# ssh_tunnel: откат выхода сервера, если его не подтвердили. Создано панелью.
[Unit]
Description=ssh_tunnel - откат выхода сервера без подтверждения

[Service]
Type=oneshot
ExecStart=/bin/sh ` + egressVarDir + `/rollback.sh
`
}

// egressRollbackTimerText — только OnActiveSec. OnBootSec здесь нельзя
// (нашёл стенд test/netns, ТЗ 14): на сервере, загруженном больше минуты
// назад, срок OnBootSec уже в прошлом, и systemd запускает откат сразу при
// включении таймера — выход откатывался бы в ту же секунду. Перезагрузку
// посреди окна покрывает и так: таймер включён (timers.target), при загрузке
// он активируется заново и срабатывает через OnActiveSec после неё.
func egressRollbackTimerText() string {
	return fmt.Sprintf(`# ssh_tunnel: окно подтверждения выхода сервера. Создано панелью.
[Unit]
Description=ssh_tunnel - окно подтверждения выхода сервера

[Timer]
OnActiveSec=%[1]d
AccuracySec=1s

[Install]
WantedBy=timers.target
`, egressWindowSec)
}

// egressOffScript — выключить и убрать всё: службы, правила, таблицу,
// sysctl, файлы. Годится в любом состоянии, в том числе после падения.
func egressOffScript() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# ssh_tunnel: выключить выход сервера и убрать всё. Пишет панель.\nset -u\n")
	fmt.Fprintf(&b, "systemctl disable --now %s %s %s 2>/dev/null || true\n", egressUnit, egressRulesUnit, egressRollbackTmr)
	b.WriteString(rulesDownScript())
	fmt.Fprintf(&b, `ip -4 route flush table %[1]d 2>/dev/null || true
ip -6 route flush table %[1]d 2>/dev/null || true
C=%[2]s/config/ssh_tunnel
if [ -f "$C/resolv.conf.backup" ]; then
  rm -f /etc/resolv.conf
  if [ -f "$C/resolv.conf.link" ]; then ln -s "$(cat "$C/resolv.conf.link")" /etc/resolv.conf
  else cp "$C/resolv.conf.backup" /etc/resolv.conf; fi
  echo "/etc/resolv.conf возвращён"
fi
resolvectl revert sshtun0 2>/dev/null || true
`, egressTable, egressVarDir)
	for _, u := range egressUnitNames {
		fmt.Fprintf(&b, "rm -f /etc/systemd/system/%s\n", u)
	}
	b.WriteString("systemctl daemon-reload\n")
	fmt.Fprintf(&b, "systemctl reset-failed %s %s %s 2>/dev/null || true\n", egressUnit, egressRulesUnit, egressRollbackUnit)
	fmt.Fprintf(&b, "rm -rf %s %s\necho \"выход сервера выключен, всё убрано\"\n", egressVarDir, egressBinDir)
	return b.String()
}

// egressRollbackScript — запускает таймер. markDir — папка панели, куда
// записать, что был откат (страница это покажет).
func egressRollbackScript(markDir string) string {
	return fmt.Sprintf(`#!/bin/sh
# ssh_tunnel: откат выхода сервера, если его не подтвердили. Пишет панель.
[ -f %[1]s/pending ] || exit 0
echo "подтверждения не было — возвращаю как было"
mkdir -p %[2]s && date +%%s > %[2]s/rolled-back
exec /bin/sh %[1]s/off.sh
`, egressVarDir, shQuote(markDir))
}

// egressApplyInput — что нужно скрипту включения.
type egressApplyInput struct {
	NewBinary string // проверенный файл клиента; пусто — уже стоит нужной версии
	Version   string
	Host      string // адрес B (IP: DNS при выключенном интернете не нужен)
	Port      int
	User      string
	KeyFile   string // закрытый ключ в папке панели
	KnownHost string // строка known_hosts для B (ключ, увиденный при проверке)
	MarkDir   string
	Plan      egressPlan
}

func heredoc(path, body, marker string) string {
	return "cat > " + path + " <<'" + marker + "'\n" + body + marker + "\n"
}

// egressApplyScript — включение (bash от root, под systemd-run). Таймер
// отката включается раньше всего, что меняет маршруты: прервётся скрипт
// после этого — откат всё равно случится.
func egressApplyScript(in egressApplyInput) string {
	var b strings.Builder
	b.WriteString("set -eu\ntrap '' PIPE\n")
	fmt.Fprintf(&b, "D=%s\nmkdir -p \"$D/config\"\nchmod 700 \"$D\"\n", egressVarDir)
	if in.NewBinary != "" {
		fmt.Fprintf(&b, "echo \"ставлю клиент %s\"\ninstall -D -m 755 %s %s\necho %s > %s/VERSION\n",
			in.Version, shQuote(in.NewBinary), egressBinPath, shQuote(in.Version), egressBinDir)
	}
	fmt.Fprintf(&b, "install -m 600 %s \"$D/key\"\n", shQuote(in.KeyFile))
	fmt.Fprintf(&b, "echo \"записываю профиль сервера %s\"\n", in.Host)
	fmt.Fprintf(&b, "XDG_CONFIG_HOME=\"$D/config\" %s -host %s -sshport %d -user %s -key \"$D/key\" -filter all -save\n",
		egressBinPath, shQuote(in.Host), in.Port, shQuote(in.User))
	if in.KnownHost != "" {
		b.WriteString("mkdir -p \"$D/config/ssh_tunnel\"\n")
		b.WriteString(heredoc("\"$D/config/ssh_tunnel/known_hosts\"", strings.TrimSpace(in.KnownHost)+"\n", "EOF_KH_7097"))
	}
	b.WriteString(heredoc("\"$D/off.sh\"", egressOffScript(), "EOF_OFF_7097"))
	b.WriteString(heredoc("\"$D/rollback.sh\"", egressRollbackScript(in.MarkDir), "EOF_RB_7097"))
	b.WriteString(heredoc("\"$D/rules-up.sh\"", in.Plan.upScript(), "EOF_UP_7097"))
	b.WriteString(heredoc("\"$D/rules-down.sh\"", "#!/bin/sh\nset -u\n"+rulesDownScript(), "EOF_DOWN_7097"))
	for name, text := range map[string]string{
		egressRulesUnit: egressRulesUnitText(), egressUnit: egressClientUnitText(),
		egressRollbackUnit: egressRollbackUnitText(), egressRollbackTmr: egressRollbackTimerText(),
	} {
		b.WriteString(heredoc("/etc/systemd/system/"+name, text, "EOF_UNIT_7097"))
	}
	fmt.Fprintf(&b, `systemctl daemon-reload
date +%%s > "$D/pending"
systemctl enable --now %s
echo "окно отката: %d с"
systemctl start %s
systemctl start %s
echo "применено — жду подтверждения из панели"
`, egressRollbackTmr, egressWindowSec, egressRulesUnit, egressUnit)
	return b.String()
}

// egressConfirmScript — человек подтвердил, что сервер доступен.
func egressConfirmScript() string {
	return fmt.Sprintf(`set -eu
D=%s
[ -f "$D/pending" ] || { echo "окно подтверждения уже закрылось — откат прошёл или идёт" >&2; exit 3; }
rm -f "$D/pending"
systemctl disable --now %s
systemctl enable %s %s
echo "подтверждено: выход сервера включён и переживёт перезагрузку"
`, egressVarDir, egressRollbackTmr, egressRulesUnit, egressUnit)
}

// egressModeScript — сменить поведение без B: переписать правила и, если
// они стоят, поставить заново.
func egressModeScript(p egressPlan) string {
	return "set -eu\nD=" + egressVarDir + "\n[ -d \"$D\" ] || exit 0\n" +
		heredoc("\"$D/rules-up.sh\"", p.upScript(), "EOF_UP_7097") +
		"if systemctl is-active --quiet " + egressRulesUnit + "; then /bin/sh \"$D/rules-up.sh\"; fi\n"
}

// egressCatchAllScript — снять (on=false) или вернуть правило «всё
// остальное — в туннель» (pref 9501). Вернуть — заново поставить правила
// целиком (rules-up.sh), если служба правил работает.
func egressCatchAllScript(on bool) string {
	if on {
		return "D=" + egressVarDir + "\n" +
			"if systemctl is-active --quiet " + egressRulesUnit + " && [ -f \"$D/rules-up.sh\" ]; then /bin/sh \"$D/rules-up.sh\"; fi\n" +
			"echo \"сервер Б снова отвечает — выход опять через него\"\n"
	}
	return fmt.Sprintf(`for fam in -4 -6; do
  while ip $fam rule del pref %d 2>/dev/null; do :; done
done
echo "сервер Б не отвечает — выход напрямую, пока он не вернётся"
`, prefEgressTunnel)
}

// egressOffNowScript — кнопка «Выключить»: тот же off.sh, а если его нет
// (сломанная установка) — его текст прямо здесь.
func egressOffNowScript() string {
	return "if [ -f " + egressVarDir + "/off.sh ]; then exec /bin/sh " + egressVarDir + "/off.sh; fi\n" + egressOffScript()
}
