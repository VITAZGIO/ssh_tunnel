// Package machines — плагин «Машины»: показания своих серверов и компьютеров
// (процессор, память, диск, температура, время работы) и, по желанию,
// включение (Wake-on-LAN) и выключение (wol.go).
//
// Как читать — у каждой машины свой способ:
//   - ssh — программа заходит по SSH (ключом программы из плагина SSH или
//     паролем) и читает /proc (Linux: Ubuntu, Proxmox, Orange Pi);
//   - netdata — забирает показания у netdata (http://адрес:19999);
//   - ha — датчики Home Assistant по токену (любые: температура, нагрузка…);
//   - self — само это устройство (Linux).
//
// Читает устройство, где машины заведены, раз в 30 с. «Показывать всем
// устройствам сети» — и другие свои устройства (телефон на работе) видят
// эти показания по сети устройств (порт SharePort): настроил один раз на
// домашнем сервере — видно отовсюду.
package machines

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"sshtunnel/internal/addons"
	"sshtunnel/internal/devnet"
	"sshtunnel/internal/mesh"
	"sshtunnel/internal/sshapp"
)

// ID — плагин в каталоге.
const ID = "machines"

// SharePort — порт в сети устройств: отдать свои показания.
const SharePort = 47843

func init() {
	addons.Register(ID, func() addons.Module { return &module{} })
	addons.RegisterRoutes(ID, addons.Route{
		Path: "/api/machines", Methods: "GET, POST",
		Desc: "Машины: GET — свои машины с показаниями и показания с других устройств сети (те, что показывают их всем); POST {\"op\":\"add|remove|wake|shutdown|settings|refresh\",…}.",
		H:    handleAPI,
	})
}

// Machine — машина и как её читать.
type Machine struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Src  string `json:"src"` // ssh, netdata, ha, self, "" — без показаний

	Host    string   `json:"host,omitempty"` // ssh
	Port    int      `json:"port,omitempty"`
	User    string   `json:"user,omitempty"`
	Pass    string   `json:"pass,omitempty"` // пусто — ключ программы
	HostKey string   `json:"hostKey,omitempty"`
	URL     string   `json:"url,omitempty"`     // netdata, ha
	Token   string   `json:"token,omitempty"`   // ha
	Sensors []string `json:"sensors,omitempty"` // ha: entity_id

	MAC  string `json:"mac,omitempty"`  // включить по сети
	Peer string `json:"peer,omitempty"` // выключить: машина в сети устройств
}

// Reading — показания.
type Reading struct {
	Time     int64     `json:"time"`
	Error    string    `json:"error,omitempty"`
	CPU      float64   `json:"cpu"` // %
	Cores    int       `json:"cores,omitempty"`
	Load     float64   `json:"load,omitempty"`
	MemUsed  int64     `json:"memUsed,omitempty"`
	MemTotal int64     `json:"memTotal,omitempty"`
	DiskUsed int64     `json:"diskUsed,omitempty"`
	DiskTot  int64     `json:"diskTotal,omitempty"`
	Temp     float64   `json:"temp,omitempty"` // °C
	Uptime   int64     `json:"uptime,omitempty"`
	Extra    []Extra   `json:"extra,omitempty"` // ha: датчики
	Hist     []float64 `json:"hist,omitempty"`  // процессор за последние ~30 мин
	Online   bool      `json:"online"`          // отвечает (пинг или показания)
	Ping     float64   `json:"ping,omitempty"`  // мс
}

// Extra — любой датчик (Home Assistant).
type Extra struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Unit  string `json:"unit,omitempty"`
}

// Settings — настройки этого устройства.
type Settings struct {
	Share         bool `json:"share"`         // показывать свои машины всем устройствам сети
	AllowShutdown bool `json:"allowShutdown"` // своим устройствам можно выключать это
}

type conf struct {
	Machines []Machine `json:"machines"`
	Settings Settings  `json:"settings"`
}

var (
	mu       sync.Mutex
	readings = map[string]*Reading{}
)

func confPath() string { return filepath.Join(addons.DataDir(), "machines.json") }

func loadConf() conf {
	var c conf
	if b, err := os.ReadFile(confPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if c.Machines == nil {
		c.Machines = []Machine{}
	}
	return c
}

func saveConf(c conf) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := confPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, confPath())
}

func loadSettings() Settings { return loadConf().Settings }

// ---------------------------------------------------------------------------
// Чтение.

// script — показания Linux одним заходом. Только то, что ядро держит в
// памяти (/proc, /sys, statfs корня): спящие диски не будит; других
// разделов, smartctl и hdparm не трогаем.
// Что внутри: нагрузка, ядра, два снимка
// /proc/stat через секунду (процессор), память, корень диска, самая горячая
// зона, время работы.
const script = `cat /proc/loadavg; echo ---; nproc; echo ---; head -1 /proc/stat; sleep 1; head -1 /proc/stat; echo ---; ` +
	`grep -E '^(MemTotal|MemAvailable):' /proc/meminfo; echo ---; df -P -B1 / | tail -1; echo ---; ` +
	`cat /sys/class/thermal/thermal_zone*/temp 2>/dev/null | sort -n | tail -1; echo ---; cat /proc/uptime`

func parseScript(out string) (r Reading) {
	part := strings.Split(out, "---")
	get := func(i int) string {
		if i < len(part) {
			return strings.TrimSpace(part[i])
		}
		return ""
	}
	if f := strings.Fields(get(0)); len(f) > 0 {
		r.Load, _ = strconv.ParseFloat(f[0], 64)
	}
	r.Cores, _ = strconv.Atoi(get(1))
	if l := strings.Split(get(2), "\n"); len(l) == 2 {
		a, b := cpuTimes(l[0]), cpuTimes(l[1])
		if len(a) >= 4 && len(b) >= 4 {
			var ta, tb int64
			for i := range a {
				ta += a[i]
				tb += b[i]
			}
			idle := (b[3] - a[3])
			if len(a) > 4 {
				idle += b[4] - a[4] // iowait
			}
			if d := tb - ta; d > 0 {
				r.CPU = float64(d-idle) * 100 / float64(d)
			}
		}
	}
	for _, l := range strings.Split(get(3), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 {
			v, _ := strconv.ParseInt(f[1], 10, 64)
			if f[0] == "MemTotal:" {
				r.MemTotal = v * 1024
			} else {
				r.MemUsed = v * 1024 // пока «доступно», ниже — занято
			}
		}
	}
	if r.MemTotal > 0 {
		r.MemUsed = r.MemTotal - r.MemUsed
	}
	if f := strings.Fields(get(4)); len(f) >= 4 {
		r.DiskTot, _ = strconv.ParseInt(f[1], 10, 64)
		r.DiskUsed, _ = strconv.ParseInt(f[2], 10, 64)
	}
	if t, err := strconv.ParseFloat(get(5), 64); err == nil {
		r.Temp = t / 1000
	}
	if f := strings.Fields(get(6)); len(f) > 0 {
		u, _ := strconv.ParseFloat(f[0], 64)
		r.Uptime = int64(u)
	}
	return r
}

func cpuTimes(line string) []int64 {
	f := strings.Fields(line)
	var out []int64
	for _, x := range f[min(1, len(f)):] {
		v, _ := strconv.ParseInt(x, 10, 64)
		out = append(out, v)
	}
	return out
}

// Соединения SSH держим открытыми: каждый новый вход пишет в журналы
// машины (auth.log, wtmp), и раз в 30 с это будило бы диск, если система на
// HDD. Показания читаются новым каналом в том же соединении — без входа.
var (
	sshMu    sync.Mutex
	sshConns = map[string]*ssh.Client{}
)

func sshKeyOf(m Machine) string {
	return m.ID + "|" + m.Host + "|" + strconv.Itoa(m.Port) + "|" + m.User
}

func runSSH(m Machine) (string, string, error) {
	k := sshKeyOf(m)
	sshMu.Lock()
	cl := sshConns[k]
	sshMu.Unlock()
	newKey := ""
	if cl == nil {
		var err error
		if cl, newKey, err = dialSSH(m); err != nil {
			return "", "", err
		}
		sshMu.Lock()
		sshConns[k] = cl
		sshMu.Unlock()
	}
	s, err := cl.NewSession()
	if err != nil {
		// Соединение умерло (машину перезагрузили) — один раз заново.
		cl.Close()
		sshMu.Lock()
		delete(sshConns, k)
		sshMu.Unlock()
		if cl, newKey, err = dialSSH(m); err != nil {
			return "", "", err
		}
		sshMu.Lock()
		sshConns[k] = cl
		sshMu.Unlock()
		if s, err = cl.NewSession(); err != nil {
			return "", newKey, err
		}
	}
	defer s.Close()
	out, err := s.Output(script)
	if err != nil && len(out) == 0 {
		return "", newKey, errors.New("машина не ответила показаниями (это Linux?)")
	}
	return string(out), newKey, nil
}

// dropSSH закрывает соединения машины (изменили или убрали).
func dropSSH(id string) {
	sshMu.Lock()
	defer sshMu.Unlock()
	for k, c := range sshConns {
		if strings.HasPrefix(k, id+"|") {
			c.Close()
			delete(sshConns, k)
		}
	}
}

func readSSH(m Machine) (Reading, string, error) {
	out, key, err := runSSH(m)
	if err != nil {
		return Reading{}, key, err
	}
	return parseScript(out), key, nil
}

func dialSSH(m Machine) (*ssh.Client, string, error) {
	var auth []ssh.AuthMethod
	if m.Pass != "" {
		auth = append(auth, ssh.Password(m.Pass))
	}
	if sg, err := sshapp.Signer(); err == nil {
		auth = append(auth, ssh.PublicKeys(sg))
	}
	port := m.Port
	if port == 0 {
		port = 22
	}
	newKey := ""
	cfg := &ssh.ClientConfig{User: m.User, Auth: auth, Timeout: 15 * time.Second,
		// Ключ машины: первый раз запоминаем, потом сверяем (как ssh accept-new).
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
			if m.HostKey == "" {
				newKey = line
				return nil
			}
			if line != m.HostKey {
				return errors.New("ключ машины сменился — если её переустанавливали, удали и добавь заново")
			}
			return nil
		}}
	addr := net.JoinHostPort(m.Host, strconv.Itoa(port))
	conn, err := devnet.Dial("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("нет связи с %s", addr)
	}
	_ = conn.SetDeadline(time.Now().Add(25 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, "", errors.New("машина не пустила: неверный пароль или ключ программы не установлен (плагин SSH → «Установить ключ»)")
		}
		return nil, "", err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), newKey, nil
}

func readSelf() (Reading, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "android" {
		return Reading{}, errors.New("«это устройство» — только Linux; Windows читай через netdata или Home Assistant")
	}
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil && len(out) == 0 {
		return Reading{}, err
	}
	return parseScript(string(out)), nil
}

var hc = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
	DialContext: func(_ context.Context, network, addr string) (net.Conn, error) { return devnet.Dial(network, addr) },
}}

func getJSON(url string, hdr map[string]string, v any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, x := range hdr {
		req.Header.Set(k, x)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return errors.New("нет связи с " + req.URL.Host)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s ответил %s", req.URL.Host, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

// netdata: последние значения графиков (system.cpu — проценты по видам,
// system.ram — МиБ, disk_space._ — ГиБ, system.uptime — секунды).
func readNetdata(m Machine) (Reading, error) {
	base := strings.TrimRight(m.URL, "/")
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	type data struct {
		Labels []string    `json:"labels"`
		Data   [][]float64 `json:"data"`
	}
	chart := func(name string) (map[string]float64, error) {
		var d data
		if err := getJSON(base+"/api/v1/data?chart="+name+"&after=-2&points=1&format=json", nil, &d); err != nil {
			return nil, err
		}
		out := map[string]float64{}
		if len(d.Data) > 0 {
			for i, l := range d.Labels {
				if i < len(d.Data[0]) {
					out[l] = d.Data[0][i]
				}
			}
		}
		return out, nil
	}
	var r Reading
	cpu, err := chart("system.cpu")
	if err != nil {
		return r, err
	}
	for k, v := range cpu {
		if k != "time" && k != "idle" {
			r.CPU += v
		}
	}
	if ram, err := chart("system.ram"); err == nil {
		var tot float64
		for k, v := range ram {
			if k != "time" {
				tot += v
			}
		}
		r.MemTotal, r.MemUsed = int64(tot*(1<<20)), int64(ram["used"]*(1<<20))
	}
	// Корень диска: в новых netdata — «disk_space./», в старых — «disk_space._».
	d, err := chart("disk_space./")
	if err != nil || len(d) == 0 {
		d, err = chart("disk_space._")
	}
	if err == nil {
		r.DiskUsed = int64(d["used"] * (1 << 30))
		r.DiskTot = int64((d["used"] + d["avail"] + d["reserved for root"]) * (1 << 30))
	}
	if u, err := chart("system.uptime"); err == nil {
		r.Uptime = int64(u["uptime"])
	}
	if l, err := chart("system.load"); err == nil {
		r.Load = l["load1"]
	}
	return r, nil
}

// ha: состояния выбранных датчиков. Похожие на нагрузку/память/температуру
// раскладываются по своим полям, остальное — списком.
func readHA(m Machine) (Reading, error) {
	base := strings.TrimRight(m.URL, "/")
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	var r Reading
	for _, id := range m.Sensors {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		var st struct {
			State string `json:"state"`
			Attr  struct {
				Name string `json:"friendly_name"`
				Unit string `json:"unit_of_measurement"`
			} `json:"attributes"`
		}
		if err := getJSON(base+"/api/states/"+id, map[string]string{"Authorization": "Bearer " + m.Token}, &st); err != nil {
			return r, err
		}
		name := st.Attr.Name
		if name == "" {
			name = id
		}
		v, _ := strconv.ParseFloat(st.State, 64)
		low := strings.ToLower(id + " " + name)
		switch {
		case st.Attr.Unit == "%" && (strings.Contains(low, "cpu") || strings.Contains(low, "processor") || strings.Contains(low, "процессор")):
			r.CPU = v
		case st.Attr.Unit == "°C" && r.Temp == 0:
			r.Temp = v
		}
		r.Extra = append(r.Extra, Extra{name, st.State, st.Attr.Unit})
	}
	return r, nil
}

// ping — жива ли машина: в сети устройств — её пинг, иначе — соединение с
// обычными портами (Windows не отвечает на ICMP без настройки, а 445/3389
// у неё открыты). Задержка в мс; -1 — не ответила.
func ping(m Machine) float64 {
	if m.Peer != "" {
		if a, err := netip.ParseAddr(m.Peer); err == nil {
			if d, err := devnet.Ping(a); err == nil {
				return float64(d.Microseconds()) / 1000
			}
		}
	}
	host := m.Host
	if host == "" {
		host = m.Peer
	}
	if host == "" {
		return -1
	}
	ports := []string{"445", "3389", "22", "80", "8006"}
	ch := make(chan float64, len(ports))
	for _, port := range ports {
		go func(port string) {
			t := time.Now()
			c, err := devnet.Dial("tcp", net.JoinHostPort(host, port))
			if err != nil {
				ch <- -1
				return
			}
			c.Close()
			ch <- float64(time.Since(t).Microseconds()) / 1000
		}(port)
	}
	timeout := time.After(3 * time.Second)
	for range ports {
		select {
		case v := <-ch:
			if v >= 0 {
				return v
			}
		case <-timeout:
			return -1
		}
	}
	return -1
}

func readOne(m Machine) Reading {
	var r Reading
	var err error
	switch m.Src {
	case "ssh":
		var key string
		r, key, err = readSSH(m)
		if key != "" {
			mu.Lock()
			c := loadConf()
			for i := range c.Machines {
				if c.Machines[i].ID == m.ID && c.Machines[i].HostKey == "" {
					c.Machines[i].HostKey = key
				}
			}
			_ = saveConf(c)
			mu.Unlock()
		}
	case "netdata":
		r, err = readNetdata(m)
	case "ha":
		r, err = readHA(m)
	case "self":
		r, err = readSelf()
	default:
		r = Reading{}
	}
	r.Time = time.Now().Unix()
	if err != nil {
		r = Reading{Time: r.Time, Error: err.Error()}
	}
	if p := ping(m); p >= 0 {
		r.Online, r.Ping = true, p
	} else if err == nil && m.Src != "" {
		r.Online = true // показания пришли — значит жива
	}
	return r
}

// refresh перечитывает все машины разом.
func refresh() {
	c := loadConf()
	var wg sync.WaitGroup
	for _, m := range c.Machines {
		wg.Add(1)
		go func(m Machine) {
			defer wg.Done()
			r := readOne(m)
			mu.Lock()
			if old := readings[m.ID]; old != nil {
				r.Hist = old.Hist
			}
			if r.Error == "" && (m.Src == "ssh" || m.Src == "netdata" || m.Src == "self") {
				r.Hist = append(r.Hist, r.CPU)
				if len(r.Hist) > 60 {
					r.Hist = r.Hist[len(r.Hist)-60:]
				}
			}
			readings[m.ID] = &r
			mu.Unlock()
		}(m)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Модуль: опрос, общий доступ, просьбы о включении/выключении.

type module struct{ stop chan struct{} }

// Shared — машина для других устройств: без паролей и адресов входа.
type Shared struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Src     string   `json:"src"`
	MAC     string   `json:"mac,omitempty"`
	Reading *Reading `json:"reading,omitempty"`
}

func shared() []Shared {
	c := loadConf()
	mu.Lock()
	defer mu.Unlock()
	out := []Shared{}
	for _, m := range c.Machines {
		out = append(out, Shared{ID: m.ID, Name: m.Name, Src: m.Src, MAC: m.MAC, Reading: readings[m.ID]})
	}
	return out
}

func (m *module) Start() error {
	m.stop = make(chan struct{})
	mesh.Serve(WolPort, serveWol)
	mesh.Serve(SharePort, func(c net.Conn, _ mesh.Caller) {
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if !loadSettings().Share {
			_, _ = c.Write([]byte("[]"))
			return
		}
		b, _ := json.Marshal(shared())
		_, _ = c.Write(b)
	})
	stop := m.stop
	go func() {
		refresh()
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				refresh()
			}
		}
	}()
	return nil
}

func (m *module) Stop() {
	sshMu.Lock()
	for k, c := range sshConns {
		c.Close()
		delete(sshConns, k)
	}
	sshMu.Unlock()
	mesh.Serve(WolPort, nil)
	mesh.Serve(SharePort, nil)
	if m.stop != nil {
		close(m.stop)
	}
}

// fromNetwork — показания с других своих устройств, которые их показывают.
func fromNetwork() []map[string]any {
	var out []map[string]any
	var omu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range devnet.Status().Peers {
		if p.Self || !p.Online || !p.IP.IsValid() {
			continue
		}
		name := p.Name
		if name == "" {
			name = p.Host
		}
		wg.Add(1)
		go func(ip, name string) {
			defer wg.Done()
			c, err := devnet.DialPeer(net.JoinHostPort(ip, strconv.Itoa(SharePort)))
			if err != nil {
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			b, _ := io.ReadAll(io.LimitReader(c, 2<<20))
			var ms []Shared
			if json.Unmarshal(b, &ms) != nil || len(ms) == 0 {
				return
			}
			omu.Lock()
			out = append(out, map[string]any{"device": name, "ip": ip, "machines": ms})
			omu.Unlock()
		}(p.IP.String(), name)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]["device"]) < fmt.Sprint(out[j]["device"]) })
	return out
}

func randID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func handleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		c := loadConf()
		mu.Lock()
		type item struct {
			Machine
			Saved   bool     `json:"saved"`
			Reading *Reading `json:"reading,omitempty"`
		}
		var ms []item
		for _, m := range c.Machines {
			it := item{Machine: m, Saved: m.Pass != "" || m.Token != "", Reading: readings[m.ID]}
			it.Pass, it.Token, it.HostKey = "", "", ""
			ms = append(ms, it)
		}
		mu.Unlock()
		online := map[string]bool{}
		type peer struct {
			IP     string `json:"ip"`
			Name   string `json:"name"`
			Online bool   `json:"online"`
		}
		var ps []peer
		for _, p := range devnet.Status().Peers {
			if p.Self || !p.IP.IsValid() {
				continue
			}
			n := p.Name
			if n == "" {
				n = p.Host
			}
			ps = append(ps, peer{p.IP.String(), n, p.Online})
			online[p.IP.String()] = p.Online
		}
		out := map[string]any{"machines": ms, "settings": c.Settings, "peers": ps, "online": online,
			"os": runtime.GOOS, "canShutdown": runtime.GOOS == "windows" || runtime.GOOS == "linux"}
		if r.URL.Query().Get("net") != "0" {
			out["network"] = fromNetwork()
		}
		writeJSON(w, out)
		return
	}
	var req struct {
		Op string `json:"op"`
		Machine
		Settings *Settings `json:"settings"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "нужен JSON {\"op\"}"})
		return
	}
	mu.Lock()
	c := loadConf()
	mu.Unlock()
	var err error
	var out any = map[string]bool{"ok": true}
	find := func(id string) *Machine {
		for i := range c.Machines {
			if c.Machines[i].ID == id {
				return &c.Machines[i]
			}
		}
		return nil
	}
	switch req.Op {
	case "add":
		m := req.Machine
		m.Name = strings.TrimSpace(m.Name)
		if m.Name == "" {
			err = errors.New("нужно имя")
			break
		}
		if m.MAC != "" {
			if m.MAC, _, err = normMAC(m.MAC); err != nil {
				break
			}
		}
		if old := find(m.ID); old != nil {
			if m.Pass == "" {
				m.Pass = old.Pass
			}
			if m.Token == "" {
				m.Token = old.Token
			}
			if m.Host == old.Host {
				m.HostKey = old.HostKey
			}
			*old = m
		} else {
			if m.ID == "" {
				m.ID = randID()
			}
			c.Machines = append(c.Machines, m)
		}
		mu.Lock()
		err = saveConf(c)
		mu.Unlock()
		dropSSH(m.ID)
		go refresh()
	case "remove":
		out := c.Machines[:0]
		for _, m := range c.Machines {
			if m.ID != req.ID {
				out = append(out, m)
			}
		}
		c.Machines = out
		mu.Lock()
		err = saveConf(c)
		delete(readings, req.ID)
		mu.Unlock()
		dropSSH(req.ID)
	case "settings":
		if req.Settings != nil {
			c.Settings = *req.Settings
			mu.Lock()
			err = saveConf(c)
			mu.Unlock()
		}
	case "refresh":
		refresh()
	case "wake":
		mac := req.MAC
		if m := find(req.ID); m != nil && mac == "" {
			mac = m.MAC
		}
		if mac == "" {
			err = errors.New("у машины не указан MAC — «Изменить» → «Включение по сети»")
			break
		}
		sent, failed := wakeAll(mac)
		if len(sent) == 0 {
			err = errors.New("никто не отправил пакет")
			break
		}
		out = map[string]any{"sent": sent, "failed": failed}
	case "shutdown":
		m := find(req.ID)
		if m == nil || m.Peer == "" {
			err = errors.New("выключение не настроено — «Изменить» → «Выключать как»")
			break
		}
		err = askPeer(m.Peer, "shutdown", "")
	default:
		err = errors.New("op: add, remove, wake, shutdown, settings, refresh")
	}
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, out)
}
