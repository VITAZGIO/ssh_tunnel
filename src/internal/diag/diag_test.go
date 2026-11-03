package diag

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sshtunnel/internal/config"
	"sshtunnel/internal/events"
)

type fakeHost struct {
	cfg config.Config
	bus *events.Bus
}

func (f fakeHost) DialTunnel(string, string) (net.Conn, error) { return nil, errors.New("off") }
func (f fakeHost) DialDirect(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("off")
}
func (f fakeHost) DirectResolver() *net.Resolver { return net.DefaultResolver }
func (f fakeHost) TunnelUp() bool                { return false }
func (f fakeHost) Bypass() bool                  { return false }
func (f fakeHost) VPNMode() bool                 { return false }
func (f fakeHost) ProxyURL() string              { return "" }
func (f fakeHost) Config() config.Config         { return f.cfg }
func (f fakeHost) Reconnect() error              { return nil }
func (f fakeHost) Bus() *events.Bus              { return f.bus }

// Секреты тестового конфига и окружения — ни одного не должно быть в отчёте.
const (
	secretMeshKey = "mk_9f8e7d6c5b4a39281706f5e4d3c2b1a0Zz"
	secretToken   = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
	secretPEM     = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n-----END OPENSSH PRIVATE KEY-----"
	secretPub     = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
)

// TestReportHasNoSecrets — в отчёте (все файлы zip) нет ни одного секрета:
// ни из полей настроек, ни из api.json, ни из журнала.
func TestReportHasNoSecrets(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	p := &cfg.Profiles[0]
	p.Host, p.User, p.KeyPath = "203.0.113.7", "tun_user", `C:\Users\me\.ssh\id_ed25519`
	p.MeshEnabled, p.MeshKey = true, secretMeshKey
	h := fakeHost{cfg: cfg, bus: events.NewBus()}

	os.WriteFile(filepath.Join(dir, "api.json"), []byte(`{"url":"http://127.0.0.1:1","token":"`+secretToken+`"}`), 0o600)
	os.WriteFile(filepath.Join(dir, "journal.log"), []byte(strings.Join([]string{
		"2099-01-01 10:00:00 info  ключ сети " + secretMeshKey,
		"2099-01-01 10:00:01 info  открыл ?t=" + secretToken,
		"2099-01-01 10:00:02 warn  " + strings.ReplaceAll(secretPEM, "\n", " "),
		"2099-01-01 10:00:03 info  ключ " + secretPub,
	}, "\n")), 0o600)
	os.WriteFile(filepath.Join(dir, "service.log"), []byte("ключ сервера:\n"+secretPEM+"\n"), 0o600)

	st := OpenStore(dir)
	st.addHistory(SiteResult{Site: "example.com?t=" + secretToken})
	files := BuildReport(h, st, dir)
	zb, err := Zip(files)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		all.Write(b)
	}
	text := all.String()
	for _, s := range []string{secretMeshKey, secretToken, "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ", "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0"} {
		if strings.Contains(text, s) {
			t.Errorf("в отчёте секрет %q", s[:12])
		}
	}
	if !strings.Contains(text, "203.0.113.7") || !strings.Contains(text, "ssh_tunnel") {
		t.Error("отчёт потерял несекретное (адрес сервера, версию)")
	}
	if !strings.Contains(text, masked) {
		t.Error("ничего не замаскировано")
	}
}

// TestVerdicts — итог и предложенное правило по двум путям.
func TestVerdicts(t *testing.T) {
	ok := Path{OK: true}
	fail := func(k string) Path { return Path{Fail: k} }
	cases := []struct {
		d, v          Path
		code, suggest string
	}{
		{ok, ok, "ok_both", ""},
		{fail(kTLSReset), ok, "blocked_dpi", "vpn"},
		{fail(kDNSSpoof), ok, "blocked_dns", "vpn"},
		{fail(kHTTPStub), ok, "blocked_stub", "vpn"},
		{fail(kTCPTimeout), ok, "blocked", "vpn"},
		{ok, fail(kTCPReset), "vpn_fail", "direct"},
		{fail(kTCPTimeout), fail(kTCPTimeout), "down", ""},
		{fail(kDNSFail), fail(kDNSFail), "no_such", ""},
		{ok, fail(kOff), "direct_ok_vpn_off", ""},
	}
	for _, c := range cases {
		code, sug := verdict(c.d, c.v)
		if code != c.code || sug != c.suggest {
			t.Errorf("%s/%s: %s %q, нужно %s %q", c.d.Fail, c.v.Fail, code, sug, c.code, c.suggest)
		}
		if phrase(code, "ru") == code || phrase(code, "en") == code {
			t.Errorf("нет фразы для %s", code)
		}
	}
}

// TestStub — страница блокировки провайдера и заглушки в DNS.
func TestStub(t *testing.T) {
	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	if !isStub(r, []byte("<html>Доступ к ресурсу ограничен по решению Роскомнадзора</html>")) {
		t.Error("заглушка не распознана")
	}
	if isStub(r, []byte(strings.Repeat("x", 10000)+"blocked")) {
		t.Error("большая страница сайта принята за заглушку")
	}
	r.StatusCode, r.Header = 302, http.Header{"Location": {"http://warning.rt.ru/?id=1"}}
	if !isStub(r, nil) {
		t.Error("перенаправление на заглушку не распознано")
	}
	if !bogon([]string{"0.0.0.0"}) || !bogon([]string{"127.0.0.1"}) || bogon([]string{"142.250.74.14"}) {
		t.Error("bogon")
	}
}

// TestExclusionFile — «Помирить» находит список исключений zapret.
func TestExclusionFile(t *testing.T) {
	f, kind := exclusionFile(`--wf-tcp=80,443 --ipset-exclude="lists\ipset-exclude.txt" --dpi-desync=fake`, `C:\zapret`)
	if kind != "ip" || f != filepath.Join(`C:\zapret`, `lists\ipset-exclude.txt`) {
		t.Fatalf("%q %q", f, kind)
	}
	if f, _ := exclusionFile("--dpi-desync=fake", "/opt"); f != "" {
		t.Fatal("нашёл несуществующее")
	}
	file := filepath.Join(t.TempDir(), "hosts-exclude.txt")
	u := Undo{Kind: "dpi", Args: "--hostlist-exclude=" + file}
	if !canReconcile(u) {
		t.Fatal("canReconcile")
	}
	if err := reconcile(&u, "vpn.example", []string{"203.0.113.7"}); err != nil {
		t.Skip("на этой системе не пишем:", err)
	}
	if err := reconcile(&u, "vpn.example", nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	if strings.Count(string(b), "vpn.example") != 1 {
		t.Fatalf("исключение записано %d раз: %q", strings.Count(string(b), "vpn.example"), b)
	}
}

// TestStoreHistory — история ограничена 10, «Что я отключал» переживает
// перезапуск.
func TestStoreHistory(t *testing.T) {
	dir := t.TempDir()
	s := OpenStore(dir)
	for i := 0; i < 15; i++ {
		s.addHistory(SiteResult{Site: string(rune('a' + i))})
	}
	s.putUndo(Undo{ID: "svc:zapret", Name: "zapret", State: "stopped"})
	s2 := OpenStore(dir)
	d := s2.read()
	if len(d.History) != historyMax || d.History[0].Site != "o" {
		t.Fatalf("история: %d, первая %q", len(d.History), d.History[0].Site)
	}
	if u, ok := s2.getUndo("svc:zapret"); !ok || u.Name != "zapret" {
		t.Fatal("«Что я отключал» потерялось после перезапуска")
	}
	if len(d.Popular) == 0 {
		t.Fatal("нет списка популярных по умолчанию")
	}
}
