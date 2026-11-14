package node

// ТЗ 15: история машин, общий список блокировки, проверка сайта с сервера —
// без настоящего meshd (кто пришёл — в контексте, как в node_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sshtunnel/internal/diag/probe"
	"sshtunnel/internal/nodeapi"
)

// clock — подменяемые часы.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newHost15(t *testing.T, opt Options) *Host {
	t.Helper()
	if opt.Dir == "" {
		opt.Dir = t.TempDir()
	}
	opt.Version = "test"
	h, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func push(t *testing.T, url string, shared bool, ms ...MachinePush) *http.Response {
	t.Helper()
	b, _ := json.Marshal(MachinesPush{Shared: shared, Machines: ms})
	resp, err := http.Post(url+"/push", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func history(t *testing.T, url, q string) History {
	t.Helper()
	resp, err := http.Get(url + "/history" + q)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h History
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

func mp(id, name string, cpu float64) MachinePush {
	return MachinePush{ID: id, Name: name, Src: "ssh", Reading: &MachineReading{CPU: cpu, MemUsed: 1, MemTotal: 4, Online: true}}
}

func TestМашиныИсторияСвояИОбщая(t *testing.T) {
	ck := &clock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	h := newHost15(t, Options{Now: ck.Now})
	net := strings.Repeat("a", 64)
	a := serverFor(h, nodeapi.PluginMachines, nodeapi.Caller{Net: net, Device: "devA", Name: "ПК"})
	defer a.Close()
	b := serverFor(h, nodeapi.PluginMachines, nodeapi.Caller{Net: net, Device: "devB", Name: "Домашний"})
	defer b.Close()
	other := serverFor(h, nodeapi.PluginMachines, nodeapi.Caller{Net: strings.Repeat("b", 64), Device: "devC", Name: "Чужой"})
	defer other.Close()

	// ПК не показывает свои машины всем, домашний — показывает.
	for i := 0; i < 10; i++ {
		if r := push(t, a.URL, false, mp("m1", "NAS", float64(10+i))); r.StatusCode != 200 {
			t.Fatalf("push: %s", r.Status)
		}
		push(t, b.URL, true, mp("m2", "Proxmox", 50), MachinePush{ID: "../x", Name: "плохой", Reading: &MachineReading{}})
		// Чаще minGap — точка не пишется.
		push(t, a.URL, false, mp("m1", "NAS", 99))
		ck.Add(time.Minute)
	}
	since := fmt.Sprintf("?since=%d", time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).Unix()-1)
	own := history(t, a.URL, since)
	if len(own.Reporter) != 2 {
		t.Fatalf("ПК видит своих и общих: %+v", own.Reporter)
	}
	var mine, shared *HistoryReporter
	for i := range own.Reporter {
		if own.Reporter[i].Self {
			mine = &own.Reporter[i]
		} else {
			shared = &own.Reporter[i]
		}
	}
	if mine == nil || shared == nil || shared.Name != "Домашний" || !shared.Shared {
		t.Fatalf("кто чей: %+v", own.Reporter)
	}
	if mine.ID != nodeapi.PeerID("devA") || strings.Contains(fmt.Sprint(own), "devA") {
		t.Fatalf("сырой id устройства ушёл наружу: %+v", own)
	}
	if len(mine.Machines) != 1 || len(mine.Machines[0].T) != 10 {
		t.Fatalf("10 точек ПК (без повторов чаще минуты): %+v", mine.Machines)
	}
	for _, v := range mine.Machines[0].CPU {
		if v == 99 {
			t.Fatal("точка чаще minGap записалась")
		}
	}
	if mine.Machines[0].Mem[0] != 25 {
		t.Fatalf("память в процентах: %v", mine.Machines[0].Mem)
	}
	if len(shared.Machines) != 1 || shared.Machines[0].ID != "m2" {
		t.Fatalf("плохой id машины пропущен: %+v", shared.Machines)
	}
	// Домашний не видит машины ПК — тот их не показывает.
	hb := history(t, b.URL, "")
	if len(hb.Reporter) != 1 || !hb.Reporter[0].Self {
		t.Fatalf("домашний видит только себя: %+v", hb.Reporter)
	}
	// Другая сеть — ничего.
	if ho := history(t, other.URL, ""); len(ho.Reporter) != 0 {
		t.Fatalf("сети разделены: %+v", ho.Reporter)
	}
	// Сутки с шагом в час — точки сливаются.
	hs := history(t, a.URL, since+"&from=self&step=3600")
	if n := len(hs.Reporter[0].Machines[0].T); n > 2 {
		t.Fatalf("шаг в час: %d точек", n)
	}

	st := h.State()
	if len(st.Networks) == 0 || st.Networks[0].Machines == nil || len(st.Networks[0].Machines.Reporters) != 2 {
		t.Fatalf("панель видит историю: %+v", st.Networks)
	}
	if st.Networks[0].LastActive[nodeapi.PluginMachines] == 0 {
		t.Fatal("последнее обращение не записано")
	}
	if err := h.ClearMachines(net); err != nil {
		t.Fatal(err)
	}
	if hh := history(t, a.URL, ""); len(hh.Reporter) != 0 {
		t.Fatalf("после стирания: %+v", hh.Reporter)
	}
}

func TestМашиныСрокИКвота(t *testing.T) {
	ck := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	h := newHost15(t, Options{Now: ck.Now})
	days := 3
	if _, err := h.SetConfig(nodeapi.ConfigPatch{MachinesDays: &days}); err != nil {
		t.Fatal(err)
	}
	net := strings.Repeat("c", 64)
	a := serverFor(h, nodeapi.PluginMachines, nodeapi.Caller{Net: net, Device: "d1", Name: "ПК"})
	defer a.Close()
	for i := 0; i < 6; i++ {
		push(t, a.URL, false, mp("m1", "NAS", 1))
		ck.Add(24 * time.Hour)
	}
	nd, _ := h.netFor(net)
	h.pruneMachines(nd)
	files, _ := filepath.Glob(filepath.Join(h.opt.Dir, "nets", net, "machines", nodeapi.PeerID("d1"), "*.jsonl"))
	if len(files) != 3 {
		t.Fatalf("за 3 дня — 3 файла, а их %d", len(files))
	}
	// Квота кончилась — 507 с объяснением, и ничего не пишется.
	q := int64(1)
	if _, err := h.SetConfig(nodeapi.ConfigPatch{QuotaMB: &q}); err != nil {
		t.Fatal(err)
	}
	h.grow(nd, "", 2<<20)
	if r := push(t, a.URL, false, mp("m1", "NAS", 1)); r.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("квота: %s", r.Status)
	}
	if bad := 0; true {
		if _, err := h.SetConfig(nodeapi.ConfigPatch{MachinesDays: &bad}); err == nil {
			t.Fatal("0 дней принято")
		}
	}
}

// fakeSources — источники списка блокировки без сети.
type fakeSources struct {
	mu   sync.Mutex
	data map[string]string
}

func (f *fakeSources) fetch(_ context.Context, url string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.data[url]
	if !ok {
		return nil, errors.New("нет связи")
	}
	return io.NopCloser(strings.NewReader(d)), nil
}

func waitAd(t *testing.T, h *Host) nodeapi.AdblockState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := h.adState()
		if !st.Updating {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatal("список не обновился")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func getList(t *testing.T, url, etag string) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url+"/list", nil)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("ETag"), string(b)
}

func TestБлокировкаОбщийСписок(t *testing.T) {
	src := &fakeSources{data: map[string]string{
		"https://a.example/hosts": "# комментарий\n0.0.0.0 ads.example.com\n0.0.0.0 tracker.net  # x\n127.0.0.1 localhost\n",
		"https://b.example/list":  "||doubleclick.net^\n! adblock\n@@||good.doubleclick.net^\nmetrics.example.org\n",
	}}
	h := newHost15(t, Options{AdFetch: src.fetch})
	if _, err := h.SetAdblock(nodeapi.AdblockConf{Sources: []string{"file:///etc/passwd"}}); err == nil {
		t.Fatal("file:// принят как источник")
	}
	if _, err := h.SetAdblock(nodeapi.AdblockConf{Sources: []string{"https://a.example/hosts"}, Deny: []string{"bad name!"}}); err == nil {
		t.Fatal("не имя принято")
	}
	on := true
	if _, err := h.SetAdblock(nodeapi.AdblockConf{Sources: []string{"https://a.example/hosts", "https://b.example/list", "https://down.example/x"},
		Deny: []string{"My.Own.Ads."}, Allow: []string{"tracker.net", "safe.doubleclick.net"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetConfig(nodeapi.ConfigPatch{Adblock: &on}); err != nil {
		t.Fatal(err)
	}
	st := waitAd(t, h)
	if st.Version == "" || st.Names != 4 {
		// ads.example.com, doubleclick.net, metrics.example.org, my.own.ads
		t.Fatalf("итог списка: %+v", st)
	}
	if len(st.Fetched) != 3 || st.Fetched[2].Error == "" || st.Fetched[0].Names != 2 {
		t.Fatalf("по источникам: %+v", st.Fetched)
	}
	srv := serverFor(h, nodeapi.PluginAdblock, nodeapi.Caller{Net: strings.Repeat("a", 64), Device: "d1"})
	defer srv.Close()
	code, etag, body := getList(t, srv.URL, "")
	if code != 200 || etag == "" {
		t.Fatalf("список: %d %q", code, etag)
	}
	for _, want := range []string{"\nads.example.com\n", "\nmy.own.ads\n", "@@safe.doubleclick.net\n", "@@tracker.net\n"} {
		if !strings.Contains(body, want) {
			t.Fatalf("в списке нет %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "\ntracker.net\n") || strings.Contains(body, "localhost") {
		t.Fatalf("исключение или localhost в списке:\n%s", body)
	}
	if code, _, _ := getList(t, srv.URL, etag); code != http.StatusNotModified {
		t.Fatalf("тот же список — 304, а не %d", code)
	}
	if h.adState().Devices != 1 {
		t.Fatal("устройство, забравшее список, не посчитано")
	}
	// Ни один источник не скачался — раздаётся прежний список.
	src.mu.Lock()
	src.data = map[string]string{}
	src.mu.Unlock()
	h.UpdateAdblock()
	st = waitAd(t, h)
	if st.Error == "" || st.Names != 4 {
		t.Fatalf("прежний список должен остаться: %+v", st)
	}
	if code, e2, _ := getList(t, srv.URL, ""); code != 200 || e2 != etag {
		t.Fatalf("после неудачи раздаётся прежний: %d %q", code, e2)
	}
	// Свои записи применяются сразу, без скачивания.
	if _, err := h.SetAdblock(nodeapi.AdblockConf{Sources: st.Sources, Deny: []string{"more.ads"}}); err != nil {
		t.Fatal(err)
	}
	if _, e3, body := getList(t, srv.URL, ""); e3 == etag || !strings.Contains(body, "\nmore.ads\n") {
		t.Fatalf("свои записи не применились: %q", body)
	}
	// Перезапуск хоста — список и метка на месте.
	h.Close()
	h2 := newHost15(t, Options{Dir: h.opt.Dir, AdFetch: src.fetch})
	if s := h2.adState(); s.Version == "" || s.Names == 0 {
		t.Fatalf("после перезапуска: %+v", s)
	}
}

func TestПроверкаСайтаЛимитИТело(t *testing.T) {
	var calls atomic.Int32
	ck := &clock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	h := newHost15(t, Options{Now: ck.Now, SiteCheck: func(ctx context.Context, site, lang string) (probe.ServerResult, error) {
		calls.Add(1)
		return probe.ServerResult{Site: site, Verdict: "server_ok", Text: probe.ServerPhrase("server_ok", lang)}, nil
	}})
	net := strings.Repeat("a", 64)
	a := serverFor(h, nodeapi.PluginSitecheck, nodeapi.Caller{Net: net, Device: "d1"})
	defer a.Close()
	b := serverFor(h, nodeapi.PluginSitecheck, nodeapi.Caller{Net: net, Device: "d2"})
	defer b.Close()
	ask := func(url, body string) int {
		resp, err := http.Post(url+"/check", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < siteBurst; i++ {
		if c := ask(a.URL, `{"site":"example.com"}`); c != 200 {
			t.Fatalf("проверка %d: %d", i, c)
		}
	}
	if c := ask(a.URL, `{"site":"example.com"}`); c != http.StatusTooManyRequests {
		t.Fatalf("шестая подряд должна быть отвергнута: %d", c)
	}
	if c := ask(b.URL, `{"site":"example.com"}`); c != 200 {
		t.Fatalf("у другого устройства своё ведро: %d", c)
	}
	ck.Add(siteRefill)
	if c := ask(a.URL, `{"site":"example.com"}`); c != 200 {
		t.Fatalf("через %v можно снова: %d", siteRefill, c)
	}
	// Чужие поля (тело, заголовки, метод) не принимаются: это не прокси.
	if c := ask(b.URL, `{"site":"example.com","body":"x","headers":{"a":"b"}}`); c != http.StatusBadRequest {
		t.Fatalf("лишние поля: %d", c)
	}
	if c := ask(b.URL, `{"site":"a b"}`); c != http.StatusBadRequest {
		t.Fatalf("не адрес: %d", c)
	}
	if n := calls.Load(); n != siteBurst+2 {
		t.Fatalf("дошло до проверки %d раз", n)
	}
}

func TestПроверкаСайтаНастоящиеОтказы(t *testing.T) {
	h := newHost15(t, Options{})
	for _, site := range []string{"localhost", "https://127.0.0.1", "10.0.0.1", "192.168.1.1", "[::1]", "nas.lan", "pc.mesh", "198.19.0.5", "169.254.1.1", "100.64.1.1"} {
		r, err := h.CheckSite(context.Background(), nodeapi.SiteReq{Site: site})
		if err != nil {
			t.Fatalf("%s: %v", site, err)
		}
		if r.Verdict != "server_refused" || r.Server.Fail != probe.KindRefused {
			t.Fatalf("%s должен быть отвергнут: %+v", site, r)
		}
		if strings.Contains(r.Text, "server_") {
			t.Fatalf("нет фразы: %q", r.Text)
		}
	}
	if _, err := h.CheckSite(context.Background(), nodeapi.SiteReq{Site: "example.com:22"}); !errors.Is(err, probe.ErrPort) {
		t.Fatalf("порт 22: %v", err)
	}
}

// Одновременно: много устройств шлют показания и читают историю, панель
// смотрит состояние и стирает историю. Гонять с -count=20.
func TestМашиныОдновременно(t *testing.T) {
	h := newHost15(t, Options{})
	net := strings.Repeat("d", 64)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		srv := serverFor(h, nodeapi.PluginMachines, nodeapi.Caller{Net: net, Device: fmt.Sprintf("dev%d", i), Name: fmt.Sprintf("У%d", i)})
		defer srv.Close()
		wg.Add(1)
		go func(i int, url string) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				b, _ := json.Marshal(MachinesPush{Shared: i%2 == 0, Machines: []MachinePush{mp(fmt.Sprintf("m%d", j%3), "M", float64(j))}})
				if resp, err := http.Post(url+"/push", "application/json", bytes.NewReader(b)); err == nil {
					resp.Body.Close()
				}
				if resp, err := http.Get(url + "/history"); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}(i, srv.URL)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 10; j++ {
			h.State()
			if j == 5 {
				_ = h.ClearMachines(net)
			}
		}
	}()
	wg.Wait()
	// Каждая строка файлов истории — целый JSON (записи не перемешались).
	files, _ := filepath.Glob(filepath.Join(h.opt.Dir, "nets", net, "machines", "*", "*.jsonl"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var p point
			if l != "" && json.Unmarshal([]byte(l), &p) != nil {
				t.Fatalf("испорченная строка в %s: %q", f, l)
			}
		}
	}
}
