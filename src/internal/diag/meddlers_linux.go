//go:build linux && !android

package diag

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Linux: чужие туннели (wg, tun, tailscale…), обходы DPI (nfqws, tpws,
// byedpi), перехват в iptables/nft, DNS в resolv.conf. Остановить и вернуть
// — через systemd (нужны права root, их есть у VPN-версии).

const ourIf = "sshtun0"

var linuxVPNIf = []struct{ prefix, name string }{
	{"wg", "WireGuard"}, {"tailscale", "Tailscale"}, {"wt", "NetBird"}, {"nb-", "NetBird"},
	{"zt", "ZeroTier"}, {"ham", "Hamachi"}, {"tun", "VPN (tun)"}, {"tap", "VPN (tap)"},
}

// vpnUnits — служба systemd чужого VPN.
var vpnUnits = map[string]string{
	"Tailscale": "tailscaled", "NetBird": "netbird", "ZeroTier": "zerotier-one", "Hamachi": "logmein-hamachi",
}

var linuxDPI = map[string]string{
	"nfqws": "zapret (nfqws)", "tpws": "zapret (tpws)", "ciadpi": "ByeDPI", "byedpi": "ByeDPI",
	"spoofdpi": "SpoofDPI", "goodbyedpi": "GoodbyeDPI",
}

func scanPlatform(ctx context.Context) []Finding {
	var fs []Finding
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Name == ourIf || i.Flags&net.FlagUp == 0 {
			continue
		}
		for _, v := range linuxVPNIf {
			if strings.HasPrefix(i.Name, v.prefix) {
				f := Finding{ID: "if:" + i.Name, Kind: "vpn", Severity: "warn", Name: v.name + " (" + i.Name + ")"}
				if strings.HasPrefix(i.Name, "wg") && unitActive("wg-quick@"+i.Name) {
					f.Unit = "wg-quick@" + i.Name
				}
				if u, ok := vpnUnits[v.name]; ok && f.Unit == "" && unitActive(u) {
					f.Unit = u
				}
				f.CanStop = f.Unit != ""
				fs = append(fs, f)
				break
			}
		}
	}
	for _, p := range procsByName() {
		name, ok := linuxDPI[p.comm]
		if !ok {
			continue
		}
		f := Finding{ID: "proc:" + p.comm, Kind: "dpi", Severity: "warn", Name: name,
			PID: p.pid, Exe: p.exe, Args: p.args, Dir: filepath.Dir(p.exe), Unit: p.unit}
		f.CanStop = f.Unit != ""
		fs = append(fs, f)
	}
	if r := redirectRules(ctx); r != "" {
		fs = append(fs, Finding{ID: "firewall", Kind: "firewall", Severity: "warn", Name: r})
	}
	if ns := foreignNameservers(); len(ns) > 0 {
		fs = append(fs, Finding{ID: "dns:resolv.conf", Kind: "dns", Severity: "info",
			Name: "/etc/resolv.conf: " + strings.Join(ns, ", ")})
	}
	return fs
}

func unitActive(u string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", u).Run() == nil
}

func unitEnabled(u string) bool {
	return exec.Command("systemctl", "is-enabled", "--quiet", u).Run() == nil
}

type lproc struct {
	pid             int
	comm, exe, args string
	unit            string
}

func procsByName() []lproc {
	ents, _ := os.ReadDir("/proc")
	var out []lproc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil {
			continue
		}
		c := strings.TrimSpace(string(comm))
		if _, ok := linuxDPI[c]; !ok {
			continue
		}
		p := lproc{pid: pid, comm: c}
		p.exe, _ = os.Readlink("/proc/" + e.Name() + "/exe")
		if b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline"); err == nil {
			parts := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
			if len(parts) > 1 {
				p.args = strings.Join(parts[1:], " ")
			}
		}
		if b, err := os.ReadFile("/proc/" + e.Name() + "/cgroup"); err == nil {
			for _, seg := range strings.Split(string(b), "/") {
				if strings.HasSuffix(strings.TrimSpace(seg), ".service") {
					p.unit = strings.TrimSpace(seg)
				}
			}
		}
		out = append(out, p)
	}
	return out
}

// redirectRules — правила, которые уводят трафик в другую программу.
func redirectRules(ctx context.Context) string {
	if os.Geteuid() != 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var hits []string
	if out, err := exec.CommandContext(ctx, "nft", "list", "ruleset").Output(); err == nil {
		s := string(out)
		for _, w := range []string{"queue", "tproxy", "redirect"} {
			if strings.Contains(s, " "+w) {
				hits = append(hits, "nft "+w)
			}
		}
	}
	if out, err := exec.CommandContext(ctx, "iptables-save").Output(); err == nil {
		s := string(out)
		for _, w := range []string{"NFQUEUE", "TPROXY", "REDIRECT"} {
			if strings.Contains(s, "-j "+w) {
				hits = append(hits, "iptables "+w)
			}
		}
	}
	return strings.Join(hits, ", ")
}

// foreignNameservers — DNS из resolv.conf, если это не systemd-resolved и не
// мы.
func foreignNameservers() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[0] == "nameserver" {
			ip := net.ParseIP(f[1])
			if ip == nil || ip.IsLoopback() || f[1] == "198.18.0.53" {
				continue
			}
			out = append(out, f[1])
		}
	}
	return out
}

func needRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("нужны права root (запусти VPN-версию через sudo)")
	}
	return nil
}

func stopFinding(f Finding) (Undo, error) {
	if f.Unit == "" {
		return Undo{}, errors.New("это не остановить из программы")
	}
	if err := needRoot(); err != nil {
		return Undo{}, err
	}
	u := Undo{Unit: f.Unit, WasEnable: unitEnabled(f.Unit), Exe: f.Exe, Args: f.Args, Dir: f.Dir}
	if out, err := exec.Command("systemctl", "stop", f.Unit).CombinedOutput(); err != nil {
		return u, fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
	}
	return u, nil
}

func restoreUndo(u Undo) error {
	if u.Unit == "" {
		return nil
	}
	if err := needRoot(); err != nil {
		return err
	}
	if u.State == "disabled" && u.WasEnable {
		_ = exec.Command("systemctl", "enable", u.Unit).Run()
	}
	if out, err := exec.Command("systemctl", "start", u.Unit).CombinedOutput(); err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func disableAutostart(u *Undo) error {
	if u.Unit == "" {
		return errors.New("нечего отключать")
	}
	if err := needRoot(); err != nil {
		return err
	}
	if out, err := exec.Command("systemctl", "disable", u.Unit).CombinedOutput(); err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func appendLines(file string, lines []string) error {
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n" + strings.Join(lines, "\n") + "\n")
	return err
}
