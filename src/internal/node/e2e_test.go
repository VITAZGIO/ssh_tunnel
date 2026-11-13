package node

// Сквозная проверка без подделок опознания (ТЗ 13): настоящий бинарник
// meshd, собранный в тесте из src/cmd/meshd, два настоящих клиента сети
// устройств (internal/mesh) и хост плагинов на настоящих портах 47835–47838
// (meshd ведёт соединения устройств именно на них). Кто пришёл, хост узнаёт
// у meshd (op who), как в бою. Устройства ни разу не в сети одновременно:
// всё сводит сервер.
//
// В жизни устройства ходят к meshd через SSH-туннель; здесь — напрямую по
// TCP, для протокола это одно и то же. Порты плагинов заняты (на машине
// работает программа) — тест пропускается.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sshtunnel/internal/backup"
	"sshtunnel/internal/mesh"
	"sshtunnel/internal/mirror"
	"sshtunnel/internal/nodeapi"
)

var buildMeshd = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "meshd-node")
	if err != nil {
		return "", err
	}
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	bin := filepath.Join(dir, "meshd"+exe)
	out, err := exec.Command("go", "build", "-o", bin, "../../cmd/meshd").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("сборка meshd: %v: %s", err, out)
	}
	return bin, nil
})

func runMeshd(t *testing.T) string {
	t.Helper()
	bin, err := buildMeshd()
	if err != nil {
		t.Skipf("не удалось собрать meshd: %v", err)
	}
	cmd := exec.Command(bin, "-listen", "127.0.0.1:0", "-state", filepath.Join(t.TempDir(), "state.json"), "-stun", "")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if i := strings.Index(sc.Text(), "слушает "); i >= 0 {
				select {
				case found <- strings.TrimSpace(sc.Text()[i+len("слушает "):]):
				default:
				}
			}
		}
	}()
	select {
	case addr := <-found:
		return addr
	case <-time.After(15 * time.Second):
		t.Fatal("meshd не поднялся")
		return ""
	}
}

// device — устройство сети: настоящий клиент mesh и HTTP поверх него.
type device struct {
	c      *mesh.Client
	cancel context.CancelFunc
	hc     *http.Client
}

func startDevice(t *testing.T, addr, key, name string) *device {
	t.Helper()
	cfg := mesh.Config{Key: key, DeviceID: mesh.NewDeviceID(), Name: name, Addr: addr,
		NoDirect: true, NoNATProbe: true, Platform: "windows"}
	c := mesh.New(cfg, func(network, a string) (net.Conn, error) { return net.Dial(network, a) }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	d := &device{c: c, cancel: cancel, hc: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(_ context.Context, _, a string) (net.Conn, error) {
			return c.DialPeer(a)
		}}}}
	t.Cleanup(d.stop)
	deadline := time.Now().Add(10 * time.Second)
	for c.Status().State != "online" {
		if time.Now().After(deadline) {
			t.Fatalf("%s не подключился: %+v", name, c.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Адрес сервера в сети у каждого сервера свой (ТЗ 12) — берём его из
	// списка устройств, как это делают сами приложения.
	for serverIP == "" {
		for _, p := range c.Status().Peers {
			if p.Platform == "server" && p.IP.IsValid() {
				serverIP = p.IP.String()
			}
		}
		if serverIP == "" {
			if time.Now().After(deadline) {
				t.Fatalf("%s не видит сервер в списке устройств: %+v", name, c.Status())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return d
}

func (d *device) stop() {
	d.cancel()
	deadline := time.Now().Add(5 * time.Second)
	for d.c.Status().State == "online" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}

// serverIP — адрес сервера в сети устройств; узнаётся при первом подключении.
var serverIP string

func base(port int) string { return "http://" + net.JoinHostPort(serverIP, strconv.Itoa(port)) }

func portsFree(t *testing.T) {
	t.Helper()
	for _, p := range nodeapi.Ports {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			t.Skipf("порт %d занят (работает программа?) — сквозной тест пропущен", p)
		}
		ln.Close()
	}
}

func TestСквозьНастоящийMeshd(t *testing.T) {
	if testing.Short() {
		t.Skip("долгий")
	}
	portsFree(t)
	serverIP = "" // у каждого запущенного meshd адрес свой
	addr := runMeshd(t)

	var cmu sync.Mutex
	callers := map[string]nodeapi.Caller{} // device → кто пришёл, по словам meshd
	h, err := New(Options{Dir: t.TempDir(), Meshd: addr, Version: "test", Who: func(c net.Conn) (nodeapi.Caller, error) {
		who, err := AskMeshd(addr, c)
		if err == nil {
			cmu.Lock()
			callers[who.Device] = who
			cmu.Unlock()
		}
		return who, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := h.SetConfig(nodeapi.ConfigPatch{Mirror: &on, Files: &on, FilesWrite: &on, Clipboard: &on, Backup: &on}); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(); err != nil {
		t.Skipf("порты плагинов: %v", err)
	}
	t.Cleanup(h.Close)

	key := mesh.NewKey()
	ctx := context.Background()

	// ---------- ПК в сети, телефона нет ----------
	pc := startDevice(t, addr, key, "ПК")

	// Соединение мимо meshd (тот же адрес, но не через сеть устройств) —
	// закрывается без ответа.
	raw, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(nodeapi.PortFiles))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(raw, "GET /shares HTTP/1.0\r\n\r\n")
	raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, _ := raw.Read(make([]byte, 16)); n > 0 {
		t.Fatal("соединение мимо meshd получило ответ")
	}
	raw.Close()

	// Файлы: ПК кладёт файл в общую папку сети на сервере.
	req, _ := http.NewRequest(http.MethodPut, base(nodeapi.PortFiles)+"/put?s=server&p=/отчёт.txt&off=0&done=1", strings.NewReader("с ПК"))
	resp, err := pc.hc.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("загрузка файла: %v %v", err, resp)
	}
	resp.Body.Close()

	// Сеть, которую назвал meshd, — та, под которой лежат данные.
	st := h.State()
	if len(st.Networks) != 1 || st.Meshd != "ok" {
		t.Fatalf("после первого обращения: %+v", st)
	}
	netID := st.Networks[0].ID
	if st.Networks[0].LastActive[nodeapi.PluginFiles] == 0 {
		t.Fatal("нет последней активности файлов")
	}
	pcID := ""
	cmu.Lock()
	for id, c := range callers {
		if c.Name == "ПК" && c.Net == netID {
			pcID = id
		}
	}
	cmu.Unlock()
	if pcID == "" {
		t.Fatalf("meshd не назвал ПК: %+v", callers)
	}

	// Зеркальная папка сети: ПК кладёт файл.
	f, err := h.AddFolder(nodeapi.FolderReq{Net: netID, Name: "Общая"})
	if err != nil {
		t.Fatal(err)
	}
	pcDir := t.TempDir()
	os.WriteFile(filepath.Join(pcDir, "заметка.md"), []byte("написано на ПК"), 0o644)
	repPC := &mirror.Replica{Folder: f.ID, Path: pcDir, Mode: mirror.ModeBoth, Device: "pc",
		State: filepath.Join(t.TempDir(), "pc.json"), R: &mirror.Remote{Base: base(nodeapi.PortMirror), HC: pc.hc}}
	// ТЗ 18: новая папка ни для кого — пока не отметили, ПК её не видит.
	if fs, err := repPC.R.Folders(ctx); err != nil || len(fs) != 0 {
		t.Fatalf("неотмеченный ПК видит папки: %v %v", fs, err)
	}
	if _, err := repPC.Sync(ctx, 0, false); !mirror.Closed(err) || repPC.Status().State != "closed" {
		t.Fatalf("неотмеченный ПК: %v %+v", err, repPC.Status())
	}
	if b, _ := os.ReadFile(filepath.Join(pcDir, "заметка.md")); string(b) != "написано на ПК" {
		t.Fatal("закрытая папка тронула файл на устройстве")
	}
	// Галочка — по id, который назвал meshd (op who), тот же, что видят
	// устройства в списке.
	if err := h.SetFolderAccess(nodeapi.AccessReq{Net: netID, Folder: f.ID, Device: nodeapi.PeerID(pcID), On: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repPC.Sync(ctx, 0, false); err != nil {
		t.Fatalf("зеркало ПК: %v", err)
	}

	// Буфер: ПК копирует текст.
	resp, err = pc.hc.Post(base(nodeapi.PortClipboard)+"/clip?id=e2e-1&kind=text", "text/plain", strings.NewReader("скопировано на ПК"))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("буфер ПК: %v %v", err, resp)
	}
	resp.Body.Close()

	// Резервная копия ПК.
	bkDir := t.TempDir()
	os.WriteFile(filepath.Join(bkDir, "важное.txt"), []byte("резервная копия"), 0o644)
	bkPC := &backup.Remote{Base: base(nodeapi.PortBackup), HC: pc.hc}
	res, err := backup.RunBackup(ctx, bkPC, backup.Job{ID: "docs", Name: "Документы", Path: bkDir, Schedule: backup.Daily, Keep: 3}, nil, nil)
	if err != nil {
		t.Fatalf("резервная копия ПК: %v", err)
	}
	pc.stop()

	// ---------- телефон в сети, ПК уже нет ----------
	phone := startDevice(t, addr, key, "Телефон")

	phDir := t.TempDir()
	repPh := &mirror.Replica{Folder: f.ID, Path: phDir, Mode: mirror.ModeBoth, Device: "phone",
		State: filepath.Join(t.TempDir(), "ph.json"), R: &mirror.Remote{Base: base(nodeapi.PortMirror), HC: phone.hc}}
	// Телефон появился позже — не видит ничего, пока не отметили.
	if _, err := repPh.Sync(ctx, 0, false); !mirror.Closed(err) {
		t.Fatalf("неотмеченный телефон: %v", err)
	}
	phID := ""
	cmu.Lock()
	for id, c := range callers {
		if c.Name == "Телефон" && c.Net == netID {
			phID = id
		}
	}
	cmu.Unlock()
	// Устройства видят друг друга с тем же id, что у сервера (новый meshd).
	sawPC := false
	for _, p := range phone.c.Status().Peers {
		if p.Name == "ПК" {
			sawPC = p.ID == nodeapi.PeerID(pcID) && p.ID != ""
		}
	}
	if phID == "" || !sawPC || phone.c.Status().Self.ID != nodeapi.PeerID(phID) {
		t.Fatalf("id устройств: телефон %q, ПК в списке %v: %+v", phID, sawPC, phone.c.Status())
	}
	if err := h.SetFolderAccess(nodeapi.AccessReq{Net: netID, Folder: f.ID, Device: nodeapi.PeerID(phID), On: true}); err != nil {
		t.Fatal(err)
	}
	if st := h.State(); len(st.Networks[0].Folders[0].Devices) != 2 {
		t.Fatalf("кому видна папка: %+v", st.Networks[0].Folders)
	}
	if _, err := repPh.Sync(ctx, 0, false); err != nil {
		t.Fatalf("зеркало телефона: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(phDir, "заметка.md")); err != nil || string(b) != "написано на ПК" {
		t.Fatalf("файл зеркала не дошёл до телефона: %v %q", err, b)
	}
	// Кто правил — по опознанию meshd, а не по словам устройства.
	cmu.Lock()
	pcHost := callers[pcID].Host
	cmu.Unlock()
	if rep, _ := (&mirror.Remote{Base: base(nodeapi.PortMirror), HC: phone.hc}).Index(ctx, f.ID, 0, 0); rep.Files["заметка.md"].By != pcHost {
		t.Fatalf("автор правки: %q, ждали %q", rep.Files["заметка.md"].By, pcHost)
	}

	// Пропущенное в буфере.
	resp, err = phone.hc.Get(base(nodeapi.PortClipboard) + "/missed")
	if err != nil {
		t.Fatal(err)
	}
	var missed struct {
		Items []struct {
			ID   string `json:"id"`
			From string `json:"from"`
		} `json:"items"`
		Seq int64 `json:"seq"`
	}
	json.NewDecoder(resp.Body).Decode(&missed)
	resp.Body.Close()
	if len(missed.Items) != 1 || missed.Items[0].ID != "e2e-1" || missed.Items[0].From != "ПК" {
		t.Fatalf("пропущенное в буфере: %+v", missed)
	}
	resp, _ = phone.hc.Get(base(nodeapi.PortClipboard) + "/data?id=e2e-1")
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(data) != "скопировано на ПК" {
		t.Fatalf("содержимое буфера: %q", data)
	}

	// Файл ПК виден телефону.
	resp, _ = phone.hc.Get(base(nodeapi.PortFiles) + "/list?s=server&p=/")
	var list struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Entries) != 1 || list.Entries[0].Name != "отчёт.txt" {
		t.Fatalf("общая папка у телефона: %+v", list)
	}

	// Резервная копия ПК: телефону видна, только если meshd назвал их одним
	// SSH-пользователем; иначе (без пользователя) — нет.
	bkPh := &backup.Remote{Base: base(nodeapi.PortBackup), HC: phone.hc}
	sets, err := bkPh.Sets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmu.Lock()
	var phUser string
	for _, c := range callers {
		if c.Name == "Телефон" {
			phUser = c.User
		}
	}
	sameUser := phUser != "" && phUser == callers[pcID].User
	cmu.Unlock()
	if sameUser != (len(sets) == 1) {
		t.Fatalf("копии ПК у телефона: %+v (один пользователь: %v)", sets, sameUser)
	}
	if _, err := bkPh.Manifest(ctx, pcID, "docs", res.Version.ID); (err == nil) != sameUser {
		t.Fatalf("список файлов копии ПК у телефона: %v (один пользователь: %v)", err, sameUser)
	}
	phone.stop()

	// ПК вернулся и восстанавливает свою копию.
	pc2 := startDeviceID(t, addr, key, "ПК", pcID)
	out := t.TempDir()
	if _, err := backup.Restore(ctx, &backup.Remote{Base: base(nodeapi.PortBackup), HC: pc2.hc},
		backup.RestoreReq{Device: pcID, Set: "docs", Version: res.Version.ID, Target: out}, time.Now()); err != nil {
		t.Fatalf("восстановление: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "важное.txt")); string(b) != "резервная копия" {
		t.Fatalf("восстановлено %q", b)
	}

	// Другая сеть (другой ключ) не видит ничего.
	other := startDevice(t, addr, mesh.NewKey(), "Чужой")
	if fs, err := (&mirror.Remote{Base: base(nodeapi.PortMirror), HC: other.hc}).Folders(ctx); err != nil || len(fs) != 0 {
		t.Fatalf("чужая сеть видит папки: %v %v", fs, err)
	}
	if sets, err := (&backup.Remote{Base: base(nodeapi.PortBackup), HC: other.hc}).Sets(ctx); err != nil || len(sets) != 0 {
		t.Fatalf("чужая сеть видит копии: %v %v", sets, err)
	}
}

// startDeviceID — устройство с заданным id (вернулось после выключения).
func startDeviceID(t *testing.T, addr, key, name, id string) *device {
	t.Helper()
	cfg := mesh.Config{Key: key, DeviceID: id, Name: name, Addr: addr, NoDirect: true, NoNATProbe: true, Platform: "windows"}
	c := mesh.New(cfg, func(network, a string) (net.Conn, error) { return net.Dial(network, a) }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	d := &device{c: c, cancel: cancel, hc: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext:       func(_ context.Context, _, a string) (net.Conn, error) { return c.DialPeer(a) }}}}
	t.Cleanup(d.stop)
	deadline := time.Now().Add(10 * time.Second)
	for c.Status().State != "online" {
		if time.Now().After(deadline) {
			t.Fatalf("%s не подключился", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return d
}
