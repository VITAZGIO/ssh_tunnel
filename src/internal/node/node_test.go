package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sshtunnel/internal/mirror"
	"sshtunnel/internal/nodeapi"
)

// Эти тесты работают с internal/node напрямую, без настоящего meshd:
// personality (Caller) соединения подставляется прямо в контекст запроса,
// как это в бою делает ConnContext в admit(). Сама проверка «meshd сказал,
// кто это» (op who, владелец порта) — в cmd/meshd и в TestAskMeshd* ниже.

func newTestHost(t *testing.T) *Host {
	t.Helper()
	h, err := New(Options{Dir: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// serverFor — httptest-сервер плагина plugin, который на каждый запрос
// отвечает так, будто пришло устройство c.
func serverFor(h *Host, plugin string, c nodeapi.Caller) *httptest.Server {
	inner := h.pluginHandler(plugin)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), callerKey{}, c)
		inner.ServeHTTP(w, r.WithContext(ctx))
	}))
}

func remoteOf(srv *httptest.Server) *mirror.Remote {
	return &mirror.Remote{Base: srv.URL, HC: srv.Client()}
}

func TestРазныеСетиРазделены(t *testing.T) {
	h := newTestHost(t)
	on := true
	if _, err := h.SetConfig(nodeapi.ConfigPatch{Mirror: &on}); err != nil {
		t.Fatal(err)
	}
	netA := strings.Repeat("a", 64)
	netB := strings.Repeat("b", 64)
	if _, err := h.AddFolder(nodeapi.FolderReq{Net: netA, Name: "Папка A"}); err != nil {
		t.Fatal(err)
	}

	srvA := serverFor(h, nodeapi.PluginMirror, nodeapi.Caller{Net: netA, Device: "d1", Host: "pc", IP: "198.19.0.1"})
	defer srvA.Close()
	srvB := serverFor(h, nodeapi.PluginMirror, nodeapi.Caller{Net: netB, Device: "d2", Host: "tv", IP: "198.19.0.2"})
	defer srvB.Close()

	fs, err := remoteOf(srvA).Folders(context.Background())
	if err != nil || len(fs) != 1 {
		t.Fatalf("сеть A: %v %v", fs, err)
	}
	fs, err = remoteOf(srvB).Folders(context.Background())
	if err != nil || len(fs) != 0 {
		t.Fatalf("сеть B видит чужую папку: %v %v", fs, err)
	}
}

func TestЗеркальныеПапкиДваУстройства(t *testing.T) {
	h := newTestHost(t)
	on := true
	if _, err := h.SetConfig(nodeapi.ConfigPatch{Mirror: &on}); err != nil {
		t.Fatal(err)
	}
	net := strings.Repeat("c", 64)
	f, err := h.AddFolder(nodeapi.FolderReq{Net: net, Name: "Общая"})
	if err != nil {
		t.Fatal(err)
	}

	srvA := serverFor(h, nodeapi.PluginMirror, nodeapi.Caller{Net: net, Device: "pc", Host: "pc", IP: "198.19.0.1"})
	defer srvA.Close()
	srvB := serverFor(h, nodeapi.PluginMirror, nodeapi.Caller{Net: net, Device: "phone", Host: "phone", IP: "198.19.0.2"})
	defer srvB.Close()

	dirA, dirB := t.TempDir(), t.TempDir()
	repA := &mirror.Replica{Folder: f.ID, Path: dirA, Mode: mirror.ModeBoth, Device: "pc",
		State: filepath.Join(t.TempDir(), "a.json"), R: remoteOf(srvA)}
	repB := &mirror.Replica{Folder: f.ID, Path: dirB, Mode: mirror.ModeBoth, Device: "phone",
		State: filepath.Join(t.TempDir(), "b.json"), R: remoteOf(srvB)}

	if err := os.WriteFile(filepath.Join(dirA, "привет.txt"), []byte("от ПК"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := repA.Sync(ctx, 0, false); err != nil {
		t.Fatalf("sync A: %v", err)
	}
	if _, err := repB.Sync(ctx, 0, false); err != nil {
		t.Fatalf("sync B: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dirB, "привет.txt"))
	if err != nil || string(data) != "от ПК" {
		t.Fatalf("файл не дошёл до телефона: %v %q", err, data)
	}

	// Никогда не online вместе: сервер (а не одно из устройств) свёл их.
	st := h.State()
	if len(st.Networks) != 1 || st.Networks[0].Folders[0].Files != 1 {
		t.Fatalf("состояние: %+v", st)
	}
}

func TestФайлыУстройстваНаСервере(t *testing.T) {
	h := newTestHost(t)
	on, write := true, true
	if _, err := h.SetConfig(nodeapi.ConfigPatch{Files: &on, FilesWrite: &write}); err != nil {
		t.Fatal(err)
	}
	net := strings.Repeat("d", 64)
	srv := serverFor(h, nodeapi.PluginFiles, nodeapi.Caller{Net: net, Device: "pc", Host: "pc", IP: "198.19.0.1"})
	defer srv.Close()

	// Список папок: своя папка «Сервер» с write=true.
	resp, err := srv.Client().Get(srv.URL + "/shares")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Shares []struct {
			ID    string `json:"id"`
			Write bool   `json:"write"`
		} `json:"shares"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Shares) != 1 || out.Shares[0].ID != "server" || !out.Shares[0].Write {
		t.Fatalf("shares: %+v", out)
	}

	// Загрузка файла.
	body := "содержимое файла"
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/put?s=server&p=/doc.txt&off=0&done=1", strings.NewReader(body))
	resp, err = srv.Client().Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("put: %v %v", err, resp)
	}
	resp.Body.Close()

	data, err := os.ReadFile(filepath.Join(h.netDir(net), "files", "doc.txt"))
	if err != nil || string(data) != body {
		t.Fatalf("файл не записался: %v %q", err, data)
	}

	// Другая сеть этот файл не видит (своя папка «files»).
	net2 := strings.Repeat("e", 64)
	srv2 := serverFor(h, nodeapi.PluginFiles, nodeapi.Caller{Net: net2, Device: "x", Host: "x", IP: "198.19.0.3"})
	defer srv2.Close()
	resp, _ = srv2.Client().Get(srv2.URL + "/list?s=server&p=/")
	var list struct {
		Entries []struct{ Name string } `json:"entries"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Entries) != 0 {
		t.Fatalf("другая сеть видит файлы: %+v", list)
	}
}

func TestОбщийБуферПропущенное(t *testing.T) {
	h := newTestHost(t)
	on := true
	if _, err := h.SetConfig(nodeapi.ConfigPatch{Clipboard: &on}); err != nil {
		t.Fatal(err)
	}
	net := strings.Repeat("f", 64)
	devA := nodeapi.Caller{Net: net, Device: "pc", Host: "pc", Name: "ПК"}
	devB := nodeapi.Caller{Net: net, Device: "phone", Host: "phone", Name: "Телефон"}
	srvA := serverFor(h, nodeapi.PluginClipboard, devA)
	defer srvA.Close()
	srvB := serverFor(h, nodeapi.PluginClipboard, devB)
	defer srvB.Close()

	// ПК отправляет запись, пока сеть «видит» сервер (push на сервер прямо).
	req, _ := http.NewRequest(http.MethodPost, srvA.URL+"/clip?id=1&kind=text&name=&mime=", strings.NewReader("скопированный текст"))
	resp, err := srvA.Client().Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("push: %v %v", err, resp)
	}
	resp.Body.Close()

	// Телефон был не в сети — забирает пропущенное через /missed.
	resp, err = srvB.Client().Get(srvB.URL + "/missed")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Items []struct {
			ID   string `json:"id"`
			From string `json:"from"`
		} `json:"items"`
		Seq int64 `json:"seq"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Items) != 1 || out.Items[0].ID != "1" || out.Items[0].From != "ПК" {
		t.Fatalf("missed: %+v", out)
	}

	// Автору не присылается его же запись (не-эхо): ПК спрашивает missed —
	// пусто (своя отметка сразу встала на «сейчас»).
	resp, _ = srvA.Client().Get(srvA.URL + "/missed")
	var self struct {
		Items []any `json:"items"`
	}
	json.NewDecoder(resp.Body).Decode(&self)
	resp.Body.Close()
	if len(self.Items) != 0 {
		t.Fatalf("автору прислали его же запись: %+v", self)
	}

	// Подтверждение — повторный missed пуст.
	ackReq, _ := http.NewRequest(http.MethodPost, srvB.URL+"/ack?seq="+strconv.FormatInt(out.Seq, 10), nil)
	resp, _ = srvB.Client().Do(ackReq)
	resp.Body.Close()
	resp, _ = srvB.Client().Get(srvB.URL + "/missed")
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Items) != 0 {
		t.Fatalf("после ack не пусто: %+v", out)
	}
}

func TestКвотаПапкиОстанавливаетЗагрузку(t *testing.T) {
	h := newTestHost(t)
	on := true
	if _, err := h.SetConfig(nodeapi.ConfigPatch{Mirror: &on}); err != nil {
		t.Fatal(err)
	}
	net := strings.Repeat("1", 64)
	f, err := h.AddFolder(nodeapi.FolderReq{Net: net, Name: "Малая", QuotaMB: 1})
	if err != nil {
		t.Fatal(err)
	}
	srv := serverFor(h, nodeapi.PluginMirror, nodeapi.Caller{Net: net, Device: "pc", Host: "pc"})
	defer srv.Close()

	dir := t.TempDir()
	big := make([]byte, 2<<20) // 2 МБ > квота 1 МБ
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	rep := &mirror.Replica{Folder: f.ID, Path: dir, Mode: mirror.ModeBoth, Device: "pc",
		State: filepath.Join(t.TempDir(), "s.json"), R: remoteOf(srv)}
	_, err = rep.Sync(context.Background(), 0, false)
	if err == nil {
		t.Fatal("загрузка сверх квоты прошла")
	}

	// Файл не появился целиком у сервера — ни на диске папки, ни в индексе.
	h.mu.Lock()
	fc := h.conf.Folders[net][0]
	h.mu.Unlock()
	if data, statErr := os.ReadFile(filepath.Join(fc.Path, "big.bin")); statErr == nil {
		t.Fatalf("файл попал в папку сервера сверх квоты: %d байт", len(data))
	}
	st := h.State()
	if st.Networks[0].Folders[0].Files != 0 {
		t.Fatalf("файл всё же учтён: %+v", st.Networks[0].Folders[0])
	}
}

func TestQuotaReaderНеПортитФайлНаКраю(t *testing.T) {
	q := &quotaReader{r: strings.NewReader("1234567890"), left: 5, why: "нет места"}
	buf := make([]byte, 100)
	n, err := q.Read(buf)
	if err != nil || n != 5 {
		t.Fatalf("первая часть: %d %v", n, err)
	}
	n, err = q.Read(buf)
	if n != 0 || !errors.As(err, new(errFull)) {
		t.Fatalf("за краем нет ошибки: %d %v", n, err)
	}
}

// fakeDeviceConn — настоящее TCP-соединение, которое для AskMeshd играет
// роль «соединение устройства с плагином» (нужны TCPAddr у Local/RemoteAddr).
func fakeDeviceConn(t *testing.T) net.Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	srv := <-accepted
	t.Cleanup(func() { srv.Close() })
	return srv
}

func TestAskMeshdСтарыйИНедоступный(t *testing.T) {
	// Старый meshd: отвечает «неизвестная операция» на неизвестный op.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte(`{"op":"error","error":"неизвестная операция"}` + "\n"))
	}()
	_, err = AskMeshd(ln.Addr().String(), fakeDeviceConn(t))
	if !errors.Is(err, errMeshdOld) {
		t.Fatalf("ожидали errMeshdOld, получили %v", err)
	}

	// meshd не отвечает (порт закрыт).
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := closed.Addr().String()
	closed.Close()
	_, err = AskMeshd(addr, fakeDeviceConn(t))
	if !errors.Is(err, errMeshdDown) {
		t.Fatalf("ожидали errMeshdDown, получили %v", err)
	}
}

// TestAdmitЗакрываетЧужиеСоединения — настоящий TCP-порт плагина: соединение,
// про которое Who не знает (или отвечает ошибкой), закрывается молча —
// устройство увидит просто обрыв/таймаут, как от любого закрытого порта.
func TestAdmitЗакрываетЧужиеСоединения(t *testing.T) {
	var mu sync.Mutex
	known := map[string]nodeapi.Caller{}
	h, err := New(Options{Dir: t.TempDir(), Version: "test", Who: func(c net.Conn) (nodeapi.Caller, error) {
		deadline := time.Now().Add(time.Second)
		for {
			mu.Lock()
			caller, ok := known[c.RemoteAddr().String()]
			mu.Unlock()
			if ok {
				return caller, nil
			}
			if time.Now().After(deadline) {
				return nodeapi.Caller{}, errors.New("неизвестное соединение")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := h.SetConfig(nodeapi.ConfigPatch{Files: &on}); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)

	addr := h.Addr(nodeapi.PluginFiles)
	if addr == "" {
		t.Fatal("порт файлов не слушает")
	}

	// Чужое соединение: Who про него не знает.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	if n, err := c.Read(buf); err == nil && n > 0 {
		t.Fatalf("чужое соединение получило ответ: %q", buf[:n])
	}
	c.Close()

	// Опознанное соединение (Who знает его локальный адрес) — работает.
	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	mu.Lock()
	known[c2.LocalAddr().String()] = nodeapi.Caller{Net: strings.Repeat("3", 64), Device: "pc", Host: "pc"}
	mu.Unlock()
	req, _ := http.NewRequest(http.MethodGet, "http://node/shares", nil)
	req.Write(c2)
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := c2.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("опознанное соединение не ответило: %v", err)
	}
}
