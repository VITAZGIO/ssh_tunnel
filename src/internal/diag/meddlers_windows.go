//go:build windows

package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// dpiProcs — обходы DPI, которые работают процессом (zapret на Windows —
// winws.exe, GoodbyeDPI, ByeDPI, SpoofDPI).
var dpiProcs = []string{"winws.exe", "goodbyedpi.exe", "ciadpi.exe", "byedpi.exe", "spoofdpi.exe"}

// dpiServices — службы и драйверы обходов DPI.
var dpiServices = []string{"zapret", "winws1", "winws2", "GoodbyeDPI", "WinDivert", "WinDivert14", "WinDivert1.4"}

// vpnServices — службы чужих VPN: имя службы → что это.
var vpnServices = map[string]string{
	"WireGuardManager":   "WireGuard",
	"netbird":            "NetBird",
	"OpenVPNService":     "OpenVPN",
	"agent_ovpnconnect":  "OpenVPN Connect",
	"RvControlSvc":       "Radmin VPN",
	"Tailscale":          "Tailscale",
	"ZeroTierOneService": "ZeroTier",
	"Hamachi2Svc":        "Hamachi",
}

// vpnAdapters — по описанию адаптера: чей он.
var vpnAdapters = []struct{ match, name string }{
	{"netbird", "NetBird"}, {"wireguard", "WireGuard"}, {"tap-windows", "OpenVPN (TAP)"}, {"openvpn", "OpenVPN"},
	{"radmin", "Radmin VPN"}, {"tailscale", "Tailscale"}, {"zerotier", "ZeroTier"},
	{"hamachi", "Hamachi"}, {"wintun", "Wintun (чужой VPN)"},
}

const ourAdapter = "ssh_tunnel"

func hideWindow(c *exec.Cmd) *exec.Cmd {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return c
}

func scanPlatform(ctx context.Context) []Finding {
	var fs []Finding
	seen := map[string]bool{}
	add := func(f Finding) {
		if !seen[f.ID] {
			seen[f.ID] = true
			fs = append(fs, f)
		}
	}
	// Процессы обходов DPI (с командной строкой — чтобы вернуть как было и
	// помирить).
	for _, p := range processes(ctx, dpiProcs) {
		add(Finding{ID: "proc:" + strings.ToLower(p.Name), Kind: "dpi", Severity: "warn",
			Name: dpiName(p.Name), Detail: p.Exe, CanStop: true,
			PID: p.PID, Exe: p.Exe, Args: p.Args, Dir: filepath.Dir(p.Exe)})
	}
	// Службы и драйверы обходов DPI.
	for _, s := range dpiServices {
		if running(s) {
			add(Finding{ID: "svc:" + strings.ToLower(s), Kind: "dpi", Severity: "warn",
				Name: s, Detail: "служба " + s, CanStop: true, Service: s})
		}
	}
	// Службы чужих VPN, в том числе туннели WireGuard (WireGuardTunnel$имя).
	for s, name := range vpnServices {
		if running(s) {
			add(Finding{ID: "svc:" + strings.ToLower(s), Kind: "vpn", Severity: "warn",
				Name: name, Detail: "служба " + s, CanStop: true, Service: s})
		}
	}
	for _, s := range servicesWithPrefix("WireGuardTunnel$") {
		if running(s) {
			add(Finding{ID: "svc:" + strings.ToLower(s), Kind: "vpn", Severity: "warn",
				Name: "WireGuard: " + strings.TrimPrefix(s, "WireGuardTunnel$"), CanStop: true, Service: s})
		}
	}
	// Адаптеры чужих VPN без найденной службы — только сообщить.
	for _, a := range adapters() {
		if !a.up || strings.EqualFold(a.name, ourAdapter) {
			continue
		}
		d := strings.ToLower(a.desc + " " + a.name)
		// NetBird поднимает WireGuard-адаптер «wt0» — это он, а не WireGuard.
		if strings.HasPrefix(strings.ToLower(a.name), "wt") {
			d = "netbird " + d
		}
		for _, v := range vpnAdapters {
			if strings.Contains(d, v.match) {
				if svcFor(fs, v.name) {
					break
				}
				add(Finding{ID: "if:" + a.name, Kind: "vpn", Severity: "warn",
					Name: v.name + " (" + a.name + ")", Detail: a.desc})
				break
			}
		}
		if len(a.staticDNS) > 0 {
			add(Finding{ID: "dns:" + a.name, Kind: "dns", Severity: "info",
				Name: a.name + ": " + strings.Join(a.staticDNS, ", ")})
		}
	}
	// Системный прокси и PAC (свой — 127.0.0.1 — не в счёт).
	if p := readProxy(); p.PAC != "" || (p.Enable == 1 && !ourProxy(p.Server)) {
		name := p.Server
		if p.PAC != "" {
			name = "PAC " + p.PAC
		}
		add(Finding{ID: "proxy", Kind: "proxy", Severity: "warn", Name: name, CanStop: true})
	}
	// hosts.
	if ex := hostsEntries(); len(ex) > 0 {
		add(Finding{ID: "hosts", Kind: "hosts", Severity: "info", Name: strings.Join(ex, "; ")})
	}
	// Чужие маршруты «на весь интернет».
	for _, r := range catchAllRoutes(ctx) {
		add(Finding{ID: "route:" + r, Kind: "routes", Severity: "warn", Name: r})
	}
	return fs
}

func dpiName(exe string) string {
	switch strings.ToLower(exe) {
	case "winws.exe":
		return "zapret (winws.exe)"
	case "goodbyedpi.exe":
		return "GoodbyeDPI"
	}
	return exe
}

func svcFor(fs []Finding, name string) bool {
	for _, f := range fs {
		if f.Kind == "vpn" && f.Service != "" && strings.HasPrefix(f.Name, name) {
			return true
		}
	}
	return false
}

type proc struct {
	PID  int    `json:"ProcessId"`
	Name string `json:"Name"`
	Exe  string `json:"ExecutablePath"`
	Args string `json:"CommandLine"`
}

// processes — запущенные процессы с этими именами и их командные строки.
func processes(ctx context.Context, names []string) []proc {
	var q []string
	for _, n := range names {
		q = append(q, "Name='"+n+"'")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := hideWindow(exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		`@(Get-CimInstance Win32_Process -Filter "`+strings.Join(q, " OR ")+`" | Select-Object ProcessId,Name,ExecutablePath,CommandLine) | ConvertTo-Json -Compress`)).Output()
	if err != nil {
		return nil
	}
	var ps []proc
	if json.Unmarshal(out, &ps) != nil {
		var one proc
		if json.Unmarshal(out, &one) == nil && one.PID != 0 {
			ps = []proc{one}
		}
	}
	for i := range ps {
		// В CommandLine первым идёт сам exe — оставляем только аргументы.
		a := strings.TrimSpace(ps[i].Args)
		if strings.HasPrefix(a, `"`) {
			if j := strings.Index(a[1:], `"`); j >= 0 {
				a = a[j+2:]
			}
		} else if j := strings.IndexByte(a, ' '); j >= 0 {
			a = a[j:]
		} else {
			a = ""
		}
		ps[i].Args = strings.TrimSpace(a)
	}
	return ps
}

// running — служба (или драйвер) существует и работает.
func running(name string) bool {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false
	}
	defer windows.CloseServiceHandle(m)
	n, _ := windows.UTF16PtrFromString(name)
	h, err := windows.OpenService(m, n, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return false
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	if windows.QueryServiceStatus(h, &st) != nil {
		return false
	}
	return st.CurrentState == windows.SERVICE_RUNNING
}

func servicesWithPrefix(prefix string) []string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, _ := k.ReadSubKeyNames(-1)
	var out []string
	for _, n := range names {
		if strings.HasPrefix(strings.ToLower(n), strings.ToLower(prefix)) {
			out = append(out, n)
		}
	}
	return out
}

type adapter struct {
	name, desc string
	up         bool
	staticDNS  []string
}

func adapters() []adapter {
	size := uint32(16 << 10)
	var buf []byte
	for i := 0; i < 3; i++ {
		buf = make([]byte, size)
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, 0, 0, (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])), &size)
		if err == nil {
			break
		}
		if err != windows.ERROR_BUFFER_OVERFLOW {
			return nil
		}
	}
	var out []adapter
	for a := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); a != nil; a = a.Next {
		ad := adapter{
			name: windows.UTF16PtrToString(a.FriendlyName),
			desc: windows.UTF16PtrToString(a.Description),
			up:   a.OperStatus == windows.IfOperStatusUp,
		}
		guid := windows.BytePtrToString(a.AdapterName)
		ad.staticDNS = staticDNS(guid)
		out = append(out, ad)
	}
	return out
}

// staticDNS — DNS, вписанный в адаптер руками (не от роутера).
func staticDNS(guid string) []string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Services\Tcpip\Parameters\Interfaces\`+guid, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	v, _, err := k.GetStringValue("NameServer")
	if err != nil || strings.TrimSpace(v) == "" {
		return nil
	}
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
}

const inetSettings = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

func readProxy() ProxySnap {
	var p ProxySnap
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettings, registry.QUERY_VALUE)
	if err != nil {
		return p
	}
	defer k.Close()
	if v, _, err := k.GetIntegerValue("ProxyEnable"); err == nil {
		p.Enable = uint32(v)
	}
	p.Server, _, _ = k.GetStringValue("ProxyServer")
	p.PAC, _, _ = k.GetStringValue("AutoConfigURL")
	return p
}

func writeProxy(p ProxySnap) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettings, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetDWordValue("ProxyEnable", p.Enable); err != nil {
		return err
	}
	if p.Server != "" {
		_ = k.SetStringValue("ProxyServer", p.Server)
	}
	if p.PAC != "" {
		_ = k.SetStringValue("AutoConfigURL", p.PAC)
	} else {
		_ = k.DeleteValue("AutoConfigURL")
	}
	notifyProxy()
	return nil
}

// notifyProxy — сказать программам, что прокси сменился (как sysproxy).
func notifyProxy() {
	wininet := windows.NewLazySystemDLL("wininet.dll")
	p := wininet.NewProc("InternetSetOptionW")
	const settingsChanged, refresh = 39, 37
	p.Call(0, settingsChanged, 0, 0)
	p.Call(0, refresh, 0, 0)
}

// ourProxy — прокси свой (ssh_tunnel ставит 127.0.0.1:порт).
func ourProxy(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "127.0.0.1") || strings.Contains(s, "localhost")
}

func hostsEntries() []string {
	b, err := os.ReadFile(filepath.Join(os.Getenv("SystemRoot"), `System32\drivers\etc\hosts`))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if i := strings.IndexByte(l, '#'); i >= 0 {
			l = strings.TrimSpace(l[:i])
		}
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		name := strings.ToLower(f[1])
		if name == "localhost" || strings.HasSuffix(name, ".localhost") || name == "localhost.localdomain" {
			continue
		}
		out = append(out, f[1]+" → "+f[0])
		if len(out) == 5 {
			out = append(out, "…")
			break
		}
	}
	return out
}

// catchAllRoutes — адаптеры (не наш), через которые стоят маршруты 0.0.0.0/1
// и 128.0.0.0/1 — так ставят «весь интернет» чужие VPN.
func catchAllRoutes(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := hideWindow(exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		`@(Get-NetRoute -DestinationPrefix '0.0.0.0/1','128.0.0.0/1' -ErrorAction SilentlyContinue | Select-Object -ExpandProperty InterfaceAlias -Unique) | ConvertTo-Json -Compress`)).Output()
	if err != nil {
		return nil
	}
	var names []string
	if json.Unmarshal(out, &names) != nil {
		var one string
		if json.Unmarshal(out, &one) == nil && one != "" {
			names = []string{one}
		}
	}
	var res []string
	for _, n := range names {
		if !strings.EqualFold(n, ourAdapter) {
			res = append(res, n)
		}
	}
	return res
}

// psQuote — строка для PowerShell в одинарных кавычках.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// elevated запускает программу с правами администратора (окно UAC) и ждёт.
func elevated(exe string, args ...string) error {
	var qa []string
	for _, a := range args {
		qa = append(qa, psQuote(a))
	}
	al := ""
	if len(qa) > 0 {
		al = " -ArgumentList " + strings.Join(qa, ",")
	}
	cmd := hideWindow(exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"$p = Start-Process -FilePath "+psQuote(exe)+al+" -Verb RunAs -WindowStyle Hidden -Wait -PassThru; exit $p.ExitCode"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "canceled") || strings.Contains(string(out), "отмен") {
			return errors.New("в окне администратора нажали «Нет»")
		}
		return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sc — sc.exe; без прав — то же самое с окном UAC.
func sc(args ...string) error {
	err := hideWindow(exec.Command("sc.exe", args...)).Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 5 { // ERROR_ACCESS_DENIED
		return elevated("sc.exe", args...)
	}
	return err
}

func waitService(name string, want bool) {
	for i := 0; i < 40 && running(name) != want; i++ {
		time.Sleep(250 * time.Millisecond)
	}
}

func startType(name string) uint32 {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+name, registry.QUERY_VALUE)
	if err != nil {
		return 3
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("Start")
	if err != nil {
		return 3
	}
	return uint32(v)
}

var startNames = map[uint32]string{0: "boot", 1: "system", 2: "auto", 3: "demand", 4: "disabled"}

func stopFinding(f Finding) (Undo, error) {
	var u Undo
	switch {
	case f.Service != "":
		u.Service, u.StartType = f.Service, startType(f.Service)
		if err := sc("stop", f.Service); err != nil && running(f.Service) {
			return u, err
		}
		waitService(f.Service, false)
		if running(f.Service) {
			return u, errors.New("служба не остановилась")
		}
	case f.PID != 0:
		u.Exe, u.Args, u.Dir = f.Exe, f.Args, f.Dir
		pid := strconv.Itoa(f.PID)
		if err := hideWindow(exec.Command("taskkill.exe", "/PID", pid, "/F")).Run(); err != nil {
			if err := elevated("taskkill.exe", "/PID", pid, "/F"); err != nil {
				return u, err
			}
		}
	case f.ID == "proxy":
		p := readProxy()
		u.Proxy = &p
		if err := writeProxy(ProxySnap{Enable: 0, Server: p.Server}); err != nil {
			return u, err
		}
	default:
		return u, errors.New("это не остановить из программы")
	}
	return u, nil
}

func restoreUndo(u Undo) error {
	switch {
	case u.Service != "":
		if u.State == "disabled" {
			if err := sc("config", u.Service, "start=", startNames[u.StartType]); err != nil {
				return err
			}
		}
		if err := sc("start", u.Service); err != nil && !running(u.Service) {
			return err
		}
		waitService(u.Service, true)
	case u.Exe != "":
		for _, r := range u.RunKeys {
			if err := putRun(r); err != nil {
				return err
			}
		}
		return startProcess(u.Exe, u.Args, u.Dir)
	case u.Proxy != nil:
		return writeProxy(*u.Proxy)
	}
	return nil
}

// startProcess запускает программу как была (обходам DPI нужны права
// администратора — через окно UAC).
func startProcess(exe, args, dir string) error {
	ps := "Start-Process -FilePath " + psQuote(exe) + " -WorkingDirectory " + psQuote(dir) + " -Verb RunAs -WindowStyle Hidden"
	if strings.TrimSpace(args) != "" {
		ps += " -ArgumentList " + psQuote(args)
	}
	out, err := hideWindow(exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// disableAutostart — «Отключить насовсем»: служба — запуск вручную; процесс
// — убрать его записи из автозапуска (Run). Ничего не удаляем с диска.
func disableAutostart(u *Undo) error {
	switch {
	case u.Service != "":
		return sc("config", u.Service, "start=", "demand")
	case u.Exe != "":
		base := strings.ToLower(filepath.Base(u.Exe))
		for _, hive := range []string{"HKCU", "HKLM"} {
			for _, r := range runValues(hive) {
				if strings.Contains(strings.ToLower(r.Value), base) {
					if err := delRun(r); err != nil {
						return err
					}
					u.RunKeys = append(u.RunKeys, r)
				}
			}
		}
		if len(u.RunKeys) == 0 {
			return errors.New("в автозапуске (Run) не нашёл — он запускается иначе (служба, планировщик); остановлен до перезагрузки")
		}
		return nil
	case u.Proxy != nil:
		return nil // прокси и так выключен и сам не вернётся
	}
	return errors.New("нечего отключать")
}

const runPath = `Software\Microsoft\Windows\CurrentVersion\Run`

func runValues(hive string) []RunValue {
	root := registry.CURRENT_USER
	if hive == "HKLM" {
		root = registry.LOCAL_MACHINE
	}
	k, err := registry.OpenKey(root, runPath, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, _ := k.ReadValueNames(-1)
	var out []RunValue
	for _, n := range names {
		v, _, err := k.GetStringValue(n)
		if err == nil {
			out = append(out, RunValue{Hive: hive, Name: n, Value: v})
		}
	}
	return out
}

func delRun(r RunValue) error {
	key := r.Hive + `\` + runPath
	if err := hideWindow(exec.Command("reg.exe", "delete", key, "/v", r.Name, "/f")).Run(); err != nil {
		return elevated("reg.exe", "delete", key, "/v", r.Name, "/f")
	}
	return nil
}

func putRun(r RunValue) error {
	key := r.Hive + `\` + runPath
	args := []string{"add", key, "/v", r.Name, "/t", "REG_SZ", "/d", r.Value, "/f"}
	if err := hideWindow(exec.Command("reg.exe", args...)).Run(); err != nil {
		return elevated("reg.exe", args...)
	}
	return nil
}

// appendLines дописывает строки в файл; нет прав (Program Files) — через
// окно UAC.
func appendLines(file string, lines []string) error {
	text := "\r\n" + strings.Join(lines, "\r\n") + "\r\n"
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err == nil {
		_, err = f.WriteString(text)
		f.Close()
		return err
	}
	var q []string
	for _, l := range lines {
		q = append(q, psQuote(l))
	}
	return elevated("powershell.exe", "-NoProfile", "-Command",
		"Add-Content -LiteralPath "+psQuote(file)+" -Value @("+strings.Join(q, ",")+")")
}
