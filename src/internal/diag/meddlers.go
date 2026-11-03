package diag

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"
)

// «Кто трогает трафик»: ищет программы и настройки, которые мешают VPN
// (обходы DPI, чужие VPN, прокси, DNS, hosts, антивирус с проверкой HTTPS,
// чужие маршруты), и по сценарию «спросить → попробовать → спросить»
// (решение владельца, п. 6) проверяет, в них ли дело:
//
//  1. экран спрашивает «Остановить на время проверки?» — останавливаем ТОЛЬКО
//     по POST …/try (это и есть [Разрешаю]);
//  2. сами останавливаем и перепроверяем (переподключение + сайт);
//  3. не помогло — сами возвращаем как было;
//  4. помогло — экран спрашивает: «Отключить насовсем» (только автозапуск,
//     ничего не удаляем) / «Вернуть как было» / «Помирить» (где можно).
//
// Всё остановленное и отключённое — в «Что я отключал» (Store.Disabled) с
// кнопкой «Вернуть»; список хранит ядро в diag.json.

// Finding — найденное.
type Finding struct {
	ID       string `json:"id"`   // стабильный: «svc:WinDivert», «proc:winws.exe», «if:wg0», «proxy»…
	Kind     string `json:"kind"` // dpi, vpn, proxy, dns, hosts, antivirus, routes, firewall
	Name     string `json:"name"` // что показать: «zapret (winws.exe)»
	Detail   string `json:"detail,omitempty"`
	Severity string `json:"severity"` // warn — скорее всего мешает, info — может мешать
	What     string `json:"what"`
	Harm     string `json:"harm"`
	Todo     string `json:"todo"`
	CanStop  bool   `json:"canStop"`
	// Платформенные подробности: что останавливать.
	Service string `json:"service,omitempty"`
	Unit    string `json:"unit,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Exe     string `json:"exe,omitempty"`
	Args    string `json:"args,omitempty"`
	Dir     string `json:"dir,omitempty"`
}

// MeddlerScan — результат поиска.
type MeddlerScan struct {
	Time     time.Time `json:"time"`
	Findings []Finding `json:"findings"`
	Summary  string    `json:"summary"`
}

// Undo — что программа остановила или отключила и как вернуть.
type Undo struct {
	ID    string    `json:"id"`
	Kind  string    `json:"kind"`
	Name  string    `json:"name"`
	State string    `json:"state"` // stopped — на время проверки или оставлено; disabled — и автозапуск выключен; reconciled — помирили
	Time  time.Time `json:"time"`

	Service   string     `json:"service,omitempty"`
	StartType uint32     `json:"startType,omitempty"` // Windows: прежний тип запуска службы
	Unit      string     `json:"unit,omitempty"`      // Linux: юнит systemd
	WasEnable bool       `json:"wasEnabled,omitempty"`
	Exe       string     `json:"exe,omitempty"`
	Args      string     `json:"args,omitempty"`
	Dir       string     `json:"dir,omitempty"`
	RunKeys   []RunValue `json:"runKeys,omitempty"` // Windows: убранные записи автозапуска
	Proxy     *ProxySnap `json:"proxy,omitempty"`   // Windows: прежний системный прокси
	Excluded  []string   `json:"excluded,omitempty"`
	ExclFile  string     `json:"exclFile,omitempty"`
}

// RunValue — запись автозапуска в реестре (HKCU/HKLM …\Run).
type RunValue struct {
	Hive  string `json:"hive"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ProxySnap — системный прокси Windows до остановки.
type ProxySnap struct {
	Enable uint32 `json:"enable"`
	Server string `json:"server,omitempty"`
	PAC    string `json:"pac,omitempty"`
}

var errNoReconcile = errors.New("помирить не получится: эту программу не настроить так, чтобы она пропускала VPN")

// kindText — что это, чем мешает, что делать; %s — имя.
var kindText = map[string][3][2]string{
	"dpi": {
		{"%s — обход блокировок через перехват пакетов (WinDivert, nfqueue).", "%s — a DPI bypass that intercepts packets (WinDivert, nfqueue)."},
		{"Переписывает пакеты всех программ, в том числе SSH до сервера: соединение рвётся или виснет.", "Rewrites packets of every program, including SSH to the server: the connection drops or hangs."},
		{"Остановить на время проверки; помогло — исключить адрес сервера в его списках (кнопка «Помирить»).", "Stop it for the check; if it helps, exclude the server address in its lists (\"Reconcile\")."},
	},
	"vpn": {
		{"%s — другой VPN.", "%s — another VPN."},
		{"Два VPN спорят за маршруты и DNS: часть трафика уходит не туда.", "Two VPNs fight over routes and DNS: some traffic goes the wrong way."},
		{"Остановить на время проверки. Если нужен оба — добавь его сети в «всегда напрямую».", "Stop it for the check. If you need both, add its networks to \"always direct\"."},
	},
	"proxy": {
		{"Системный прокси или PAC-файл: %s.", "System proxy or PAC file: %s."},
		{"Браузеры идут через чужой прокси, а не через ssh_tunnel.", "Browsers go through someone else's proxy, not ssh_tunnel."},
		{"Отключить на время проверки (вернуть можно одной кнопкой).", "Turn it off for the check (one button brings it back)."},
	},
	"dns": {
		{"DNS задан вручную: %s.", "DNS set manually: %s."},
		{"Чужой DNS может отвечать заглушками или мимо туннеля.", "A foreign DNS may answer with block pages or bypass the tunnel."},
		{"Если сайты не открываются — поставь «автоматически» в свойствах адаптера.", "If sites do not open, set DNS to automatic in the adapter properties."},
	},
	"hosts": {
		{"В файле hosts есть записи: %s.", "The hosts file has entries: %s."},
		{"Записи в hosts сильнее DNS: имя ведёт туда, куда записано, даже через VPN.", "hosts entries beat DNS: the name goes where written, even via VPN."},
		{"Проверь записи; лишние закомментируй (# в начале строки), нужны права администратора.", "Check the entries; comment out extra ones (# at the line start), admin rights needed."},
	},
	"antivirus": {
		{"Проверку HTTPS делает %s: сертификат сайта подменён.", "%s inspects HTTPS: the site certificate is replaced."},
		{"Антивирус расшифровывает защищённые соединения — часть сайтов и программ не доверяет такому сертификату.", "The antivirus decrypts secure connections — some sites and programs do not trust its certificate."},
		{"Выключи в антивирусе «проверку защищённых соединений» (HTTPS/SSL-сканирование).", "Turn off \"secure connection scanning\" (HTTPS/SSL scanning) in the antivirus."},
	},
	"routes": {
		{"Чужие маршруты в обход: %s.", "Foreign catch-all routes: %s."},
		{"Маршруты «на весь интернет» через другой адаптер перехватывают трафик раньше VPN.", "\"Whole internet\" routes via another adapter catch traffic before the VPN."},
		{"Выключи программу, которая их ставит, или удали маршрут (route delete).", "Turn off the program that sets them, or delete the route (route delete)."},
	},
	"firewall": {
		{"Правила файрвола перехватывают трафик: %s.", "Firewall rules intercept traffic: %s."},
		{"Перенаправление (REDIRECT/TPROXY/NFQUEUE) уводит соединения в другую программу.", "Redirection (REDIRECT/TPROXY/NFQUEUE) diverts connections to another program."},
		{"Найди программу, которая ставит правила (zapret, прокси), и выключи её.", "Find the program that sets the rules (zapret, a proxy) and turn it off."},
	},
	"battery": {
		{"Экономия батареи для ssh_tunnel включена.", "Battery optimization for ssh_tunnel is on."},
		{"Система может усыпить VPN в фоне.", "The system may put the VPN to sleep in the background."},
		{"Отключи оптимизацию батареи для ssh_tunnel.", "Turn off battery optimization for ssh_tunnel."},
	},
}

// describe заполняет тексты находки на языке lang.
func describe(f *Finding, lang string) {
	t, ok := kindText[f.Kind]
	if !ok {
		return
	}
	i := 0
	if lang == "en" {
		i = 1
	}
	f.What = fmt.Sprintf(t[0][i], f.Name)
	f.Harm = t[1][i]
	f.Todo = t[2][i]
}

// Meddlers — поиск, проба, возврат.
type Meddlers struct {
	h  Host
	st *Store
}

// Scan ищет, кто мешает.
func (m *Meddlers) Scan(ctx context.Context, lang string) MeddlerScan {
	fs := scanPlatform(ctx)
	if f, ok := checkHTTPSInspection(ctx, m.h); ok {
		fs = append(fs, f)
	}
	for i := range fs {
		describe(&fs[i], lang)
	}
	sc := MeddlerScan{Time: time.Now(), Findings: fs}
	warn := 0
	for _, f := range fs {
		if f.Severity == "warn" {
			warn++
		}
	}
	switch {
	case len(fs) == 0:
		sc.Summary = tr(lang, "Ничего мешающего не нашёл.", "Found nothing that gets in the way.")
	case warn == 0:
		sc.Summary = tr(lang, fmt.Sprintf("Явных помех нет, но есть что проверить (%d).", len(fs)),
			fmt.Sprintf("No obvious interference, but a few things to check (%d).", len(fs)))
	default:
		sc.Summary = tr(lang, fmt.Sprintf("Нашёл, кто может мешать: %d.", warn),
			fmt.Sprintf("Found what may interfere: %d.", warn))
	}
	m.st.update(func(d *storeData) { d.Meddlers = &sc })
	return sc
}

func (m *Meddlers) find(ctx context.Context, id string) (Finding, error) {
	for _, f := range scanPlatform(ctx) {
		if f.ID == id {
			return f, nil
		}
	}
	return Finding{}, errors.New("уже не нашёл — возможно, оно само остановилось")
}

// TryResult — чем кончилась проба.
type TryResult struct {
	Helped       bool   `json:"helped"`
	Text         string `json:"text"`
	CanReconcile bool   `json:"canReconcile"`
	Undo         *Undo  `json:"undo,omitempty"`
}

// healthy — работает ли сейчас то, что должно: при включённой круглой
// кнопке — туннель и сайт через VPN, иначе — сайт напрямую.
func (m *Meddlers) healthy(ctx context.Context, site string) bool {
	r, err := CheckSite(ctx, m.h, site, "ru")
	if err != nil {
		return false
	}
	if m.h.Bypass() {
		return m.h.TunnelUp() && r.VPN.OK
	}
	return r.Direct.OK
}

// Try — человек нажал [Разрешаю]: остановить, перепроверить, не помогло —
// вернуть.
func (m *Meddlers) Try(ctx context.Context, id, site, lang string) (TryResult, error) {
	if site == "" {
		site = "youtube.com"
	}
	f, err := m.find(ctx, id)
	if err != nil {
		return TryResult{}, err
	}
	if !f.CanStop {
		return TryResult{}, errors.New("это не остановить из программы — см. «что делать»")
	}
	before := m.healthy(ctx, site)
	u, err := stopFinding(f)
	if err != nil {
		return TryResult{}, fmt.Errorf("не остановилось: %w", err)
	}
	u.ID, u.Kind, u.Name, u.State, u.Time = f.ID, f.Kind, f.Name, "stopped", time.Now()
	m.st.putUndo(u)
	m.h.Bus().Infof("Диагностика: остановил %s на время проверки", f.Name)

	if m.h.Bypass() {
		_ = m.h.Reconnect()
	}
	after := m.healthy(ctx, site)
	if before || !after {
		if err := restoreUndo(u); err != nil {
			return TryResult{Undo: &u, Text: tr(lang,
				"Дело не в нём, но вернуть само не вышло ("+err.Error()+") — нажми «Вернуть» в списке.",
				"It is not the cause, but restoring failed ("+err.Error()+") — press \"Restore\" in the list.")}, nil
		}
		m.st.dropUndo(u.ID)
		m.h.Bus().Infof("Диагностика: %s не мешал — вернул как было", f.Name)
		if m.h.Bypass() {
			_ = m.h.Reconnect()
		}
		return TryResult{Text: tr(lang, "Дело не в нём, вернул как было.", "It is not the cause, restored as it was.")}, nil
	}
	return TryResult{
		Helped:       true,
		Undo:         &u,
		CanReconcile: canReconcile(u),
		Text:         tr(lang, "Без «"+f.Name+"» работает. Что дальше?", "Works without \""+f.Name+"\". What next?"),
	}, nil
}

// Keep — «Отключить насовсем»: выключить только автозапуск, ничего не
// удалять.
func (m *Meddlers) Keep(id string) (Undo, error) {
	u, ok := m.st.getUndo(id)
	if !ok {
		return Undo{}, errors.New("нет в списке «Что я отключал»")
	}
	if err := disableAutostart(&u); err != nil {
		return u, err
	}
	u.State, u.Time = "disabled", time.Now()
	m.st.putUndo(u)
	m.h.Bus().Infof("Диагностика: %s отключён насовсем (только автозапуск)", u.Name)
	return u, nil
}

// Restore — «Вернуть»: как было до программы.
func (m *Meddlers) Restore(id string) error {
	u, ok := m.st.getUndo(id)
	if !ok {
		return errors.New("нет в списке «Что я отключал»")
	}
	if err := restoreUndo(u); err != nil {
		return err
	}
	m.st.dropUndo(id)
	m.h.Bus().Infof("Диагностика: вернул %s как было", u.Name)
	return nil
}

// Reconcile — «Помирить»: настроить программу так, чтобы она не трогала
// VPN (исключить адрес сервера), и запустить её снова.
func (m *Meddlers) Reconcile(ctx context.Context, id, site, lang string) (TryResult, error) {
	u, ok := m.st.getUndo(id)
	if !ok {
		return TryResult{}, errors.New("нет в списке «Что я отключал»")
	}
	if !canReconcile(u) {
		return TryResult{}, errNoReconcile
	}
	if err := reconcile(&u, m.h.Config().Active().Host, m.serverIPs(ctx)); err != nil {
		return TryResult{}, err
	}
	if err := restoreUndo(u); err != nil {
		return TryResult{}, fmt.Errorf("исключение добавил, но запустить снова не вышло: %w", err)
	}
	u.State, u.Time = "reconciled", time.Now()
	m.st.putUndo(u)
	if m.h.Bypass() {
		_ = m.h.Reconnect()
	}
	if site == "" {
		site = "youtube.com"
	}
	if m.healthy(ctx, site) {
		return TryResult{Helped: true, Undo: &u, Text: tr(lang,
			"Помирил: «"+u.Name+"» работает и не мешает VPN.",
			"Reconciled: \""+u.Name+"\" runs and does not disturb the VPN.")}, nil
	}
	return TryResult{Undo: &u, Text: tr(lang,
		"Исключение добавил, но с ним всё равно не работает. Можно отключить насовсем.",
		"Added the exclusion, but it still does not work. You can turn it off for good.")}, nil
}

func (m *Meddlers) serverIPs(ctx context.Context) []string {
	host := m.h.Config().Active().Host
	if host == "" {
		return nil
	}
	ips, err := lookup(ctx, m.h.DirectResolver(), host)
	if err != nil {
		return nil
	}
	return ips
}

// avNames — по имени издателя сертификата узнаём, кто проверяет HTTPS.
var avNames = []string{
	"kaspersky", "eset", "avast", "avg", "bitdefender", "dr.web", "drweb", "norton",
	"avira", "sophos", "fortinet", "fortigate", "zscaler", "adguard", "netfilter",
	"cisco umbrella", "mcafee", "trend micro", "comodo", "fiddler", "charles", "mitmproxy",
	"kerio", "check point", "palo alto", "360",
}

// checkHTTPSInspection — подменён ли сертификат известного сайта на пути
// «напрямую» (антивирус или корпоративный фильтр расшифровывает HTTPS).
func checkHTTPSInspection(ctx context.Context, h Host) (Finding, bool) {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	c, err := h.DialDirect(ctx, "tcp", "www.google.com:443")
	if err != nil {
		return Finding{}, false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(stepTimeout))
	tc := tls.Client(c, &tls.Config{ServerName: "www.google.com", InsecureSkipVerify: true})
	if tc.HandshakeContext(ctx) != nil {
		return Finding{}, false
	}
	for _, cert := range tc.ConnectionState().PeerCertificates {
		names := strings.ToLower(strings.Join(append(append([]string{cert.Issuer.CommonName},
			cert.Issuer.Organization...), cert.Subject.Organization...), " "))
		for _, av := range avNames {
			if strings.Contains(names, av) {
				name := cert.Issuer.CommonName
				if len(cert.Issuer.Organization) > 0 {
					name = cert.Issuer.Organization[0]
				}
				return Finding{ID: "av", Kind: "antivirus", Name: name, Severity: "warn"}, true
			}
		}
	}
	return Finding{}, false
}

// splitArgs — простая разбивка командной строки (кавычки "…").
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	q := false
	for _, r := range s {
		switch {
		case r == '"':
			q = !q
		case (r == ' ' || r == '\t') && !q:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
