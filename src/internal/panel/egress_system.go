package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// systemEgress — egressSystem поверх ip, nft, systemd и сети. Нужен root.
type systemEgress struct {
	markDir string // папка панели: там файл rolled-back
}

// NewSystemEgress — настоящая egressSystem. dataDir — папка данных панели
// (там же, где у EgressManager, — отчёт об откате кладёт туда же скрипт).
func NewSystemEgress(dataDir string) egressSystem {
	return systemEgress{markDir: dataDir + "/egress"}
}

func (systemEgress) RunScript(script string, onLine func(string)) error {
	return systemMesh{}.RunScript(script, onLine)
}

func (s systemEgress) State() egressSysState {
	var st egressSysState
	if b, err := os.ReadFile(egressVarDir + "/pending"); err == nil {
		st.Pending, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	rules := systemMesh{}.UnitState(egressRulesUnit)
	client := systemMesh{}.UnitState(egressUnit)
	st.RulesActive, st.RulesEnabled = rules.Active, rules.Enabled
	st.ClientActive = client.Active
	if out, err := exec.Command("ip", "-4", "route", "show", "table", strconv.Itoa(egressTable)).Output(); err == nil {
		st.RoutesUp = strings.TrimSpace(string(out)) != ""
	}
	if b, err := os.ReadFile(egressBinDir + "/VERSION"); err == nil {
		st.Version = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(s.markDir, "rolled-back")); err == nil {
		st.RolledBackAt, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	_, tunErr := net.InterfaceByName("sshtun0")
	st.Conflict = egressConflict(systemMesh{}.UnitState("ssh_tunnel_vpn.service").Active, tunErr == nil, client.Active)
	return st
}

// egressConflict — есть ли на сервере чужой клиент VPN ssh_tunnel. Оба
// создают адаптер sshtun0, вместе они не уживутся.
func egressConflict(userUnitActive, tunExists, ourClientActive bool) string {
	switch {
	case userUnitActive:
		return "на сервере уже работает свой клиент VPN ssh_tunnel (служба ssh_tunnel_vpn) — два клиента рядом не уживутся: оба создают адаптер sshtun0. Выключи его, если хочешь выход сервера отсюда"
	case tunExists && !ourClientActive:
		return "адаптер sshtun0 уже занят: на сервере запущен ssh_tunnel_vpn_linux вручную — выключи его, если хочешь выход сервера отсюда"
	}
	return ""
}

func (systemEgress) Restart(unit string) error {
	out, err := runSystemctl("restart", unit)
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %s", unit, strings.TrimSpace(out))
	}
	return nil
}

func (systemEgress) Facts() (egressFacts, error) {
	var f egressFacts
	for _, fam := range []string{"-4", "-6"} {
		out, err := exec.Command("ip", "-j", fam, "route", "show", "default", "table", "main").Output()
		if err != nil {
			continue
		}
		f.Uplinks = append(f.Uplinks, parseIPRouteDevs(out)...)
	}
	seen := map[string]bool{}
	for _, dev := range f.Uplinks {
		if seen[dev] {
			continue
		}
		seen[dev] = true
		out, err := exec.Command("ip", "-j", "addr", "show", "dev", dev, "scope", "global").Output()
		if err == nil {
			f.Local = append(f.Local, parseIPAddrJSON(out)...)
		}
	}
	if len(f.Uplinks) == 0 {
		return f, errors.New("не нашёл маршрут по умолчанию (ip route) — через какую карту сервер ходит в интернет?")
	}
	return f, nil
}

// parseIPRouteDevs — карты из `ip -j route show default` (кроме sshtun0).
func parseIPRouteDevs(data []byte) []string {
	var rs []struct {
		Dev string `json:"dev"`
	}
	if json.Unmarshal(data, &rs) != nil {
		return nil
	}
	var out []string
	for _, r := range rs {
		if r.Dev != "" && r.Dev != "sshtun0" {
			out = append(out, r.Dev)
		}
	}
	return out
}

// parseIPAddrJSON — адреса из `ip -j addr show`.
func parseIPAddrJSON(data []byte) []netip.Addr {
	var ifs []struct {
		AddrInfo []struct {
			Local string `json:"local"`
		} `json:"addr_info"`
	}
	if json.Unmarshal(data, &ifs) != nil {
		return nil
	}
	var out []netip.Addr
	for _, i := range ifs {
		for _, a := range i.AddrInfo {
			if p, err := netip.ParseAddr(a.Local); err == nil {
				out = append(out, p)
			}
		}
	}
	return out
}

// ipEchoURLs — кто говорит «твой адрес снаружи». Несколько: любой может
// лежать.
var ipEchoURLs = []string{"https://api.ipify.org", "https://ifconfig.me/ip", "https://icanhazip.com"}

func fetchIP(ctx context.Context, c *http.Client) (string, error) {
	var last error
	for _, u := range ipEchoURLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "curl/8")
		resp, err := c.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		if a, err := netip.ParseAddr(strings.TrimSpace(string(b))); err == nil && resp.StatusCode == http.StatusOK {
			return a.Unmap().String(), nil
		}
		last = fmt.Errorf("%s ответил %s", u, resp.Status)
	}
	return "", last
}

func (systemEgress) PublicIP(ctx context.Context) (string, error) {
	return fetchIP(ctx, &http.Client{Timeout: 8 * time.Second})
}

func (systemEgress) PublicIPVia(ctx context.Context, t EgressTarget, key []byte) (string, string, error) {
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("испорчен ключ: %w", err)
	}
	addr := net.JoinHostPort(t.IP, strconv.Itoa(t.Port))
	var known string
	cfg := &ssh.ClientConfig{
		User: t.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// Ключа сервера в ссылке нет: запоминаем увиденный сейчас и кладём
		// клиенту VPN в known_hosts — дальше он сверяет его сам.
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			known = knownhosts.Line([]string{knownhosts.Normalize(addr)}, k)
			return nil
		},
		Timeout: 10 * time.Second,
	}
	cl, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return "", "", err
	}
	defer cl.Close()
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(_ context.Context, network, a string) (net.Conn, error) { return cl.Dial(network, a) },
	}}
	ip, err := fetchIP(ctx, hc)
	return ip, known, err
}

var releaseTagRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// egressAssetName — файл клиента в релизе.
func egressAssetName(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "ssh_tunnel_vpn_linux", nil
	case "arm64":
		return "ssh_tunnel_vpn_linux_arm64", nil
	}
	return "", fmt.Errorf("для архитектуры %s готового клиента VPN нет", goarch)
}

func egressReleaseURL(version, file string) string {
	return "https://github.com/VITAZGIO/ssh_tunnel/releases/download/" + version + "/" + file
}

func (systemEgress) Download(ctx context.Context, version, dst string) error {
	if !releaseTagRe.MatchString(version) {
		return fmt.Errorf("панель собрана не из релиза (версия %q) — клиент той же версии взять негде", version)
	}
	name, err := egressAssetName(runtime.GOARCH)
	if err != nil {
		return err
	}
	got, err := downloadFile(ctx, egressReleaseURL(version, name), dst)
	if err != nil {
		return fmt.Errorf("клиент не скачался: %w", err)
	}
	want, err := expectedSum(ctx, egressReleaseURL(version, "SHA256SUMS.txt"), name)
	if err != nil {
		return err
	}
	if !strings.EqualFold(want, got) {
		return errors.New("контрольная сумма клиента не совпала — файл повреждён или подменён")
	}
	return checkELF(dst)
}

func (systemEgress) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}
