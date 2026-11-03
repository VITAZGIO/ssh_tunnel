package sshapp

import (
	"crypto/ed25519"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeServer — SSH-сервер для тестов: пароль «secret», ключи — те, что
// «дописаны» командой установки; оболочка отвечает эхом.
type fakeServer struct {
	addr    string
	mu      sync.Mutex
	keys    []string
	command string
}

func startFake(t *testing.T, hostKey ed25519.PrivateKey) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	hk, _ := ssh.NewSignerFromKey(hostKey)
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == "secret" {
				return nil, nil
			}
			return nil, errors.New("нет")
		},
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			fs.mu.Lock()
			defer fs.mu.Unlock()
			for _, line := range fs.keys {
				if strings.Contains(line, strings.Fields(hostKeyLine(k))[1]) {
					return nil, nil
				}
			}
			return nil, errors.New("нет")
		},
	}
	cfg.AddHostKey(hk)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	fs.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go fs.serve(c, cfg)
		}
	}()
	return fs
}

func (fs *fakeServer) serve(c net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for r := range creqs {
				switch r.Type {
				case "exec":
					cmd := string(r.Payload[4:])
					fs.mu.Lock()
					fs.command = cmd
					// «Дописываем» ключ из команды: echo '<ключ>' >> …
					if i := strings.Index(cmd, "echo '"); i >= 0 {
						rest := cmd[i+6:]
						fs.keys = append(fs.keys, rest[:strings.Index(rest, "'")])
					}
					fs.mu.Unlock()
					r.Reply(true, nil)
					ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					return
				case "pty-req", "window-change":
					r.Reply(true, nil)
				case "shell":
					r.Reply(true, nil)
					go func() {
						buf := make([]byte, 256)
						for {
							n, err := ch.Read(buf)
							if err != nil {
								return
							}
							ch.Write([]byte("эхо:" + string(buf[:n])))
						}
					}()
				default:
					r.Reply(false, nil)
				}
			}
		}()
	}
}

func TestInstallKeyAndTerminal(t *testing.T) {
	SetDir(t.TempDir())
	_, hostKey, _ := ed25519.GenerateKey(nil)
	fs := startFake(t, hostKey)
	host, port, _ := net.SplitHostPort(fs.addr)
	p, _ := strconv.Atoi(port)
	s, err := Save(Server{Name: "тест", Host: host, Port: p, User: "root"})
	if err != nil {
		t.Fatal(err)
	}

	// Без ключа на сервере вход ключом не проходит.
	if _, _, err := StartTerm(s, 80, 24); err == nil {
		t.Fatal("вошли без установленного ключа")
	}
	s, _ = Get(s.ID)
	if _, err := InstallKey(s, "wrong"); err == nil {
		t.Fatal("неверный пароль принят")
	}
	info, err := InstallKey(s, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fs.command, "authorized_keys") {
		t.Fatalf("команда: %q", fs.command)
	}
	_ = info
	s, _ = Get(s.ID)
	if s.HostKey == "" {
		t.Fatal("ключ сервера не запомнен")
	}

	term, _, err := StartTerm(s, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := term.input("cHJpdmV0"); err != nil { // «privet»
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) && !strings.Contains(got, "эхо:privet") {
		select {
		case <-term.wake:
		case <-time.After(100 * time.Millisecond):
		}
		d, _, _ := term.take()
		got += string(d)
	}
	if !strings.Contains(got, "эхо:privet") {
		t.Fatalf("вывод: %q", got)
	}
	term.finish(nil)

	// Сервер «переустановили» — другой ключ: отказ с понятной ошибкой.
	_, other, _ := ed25519.GenerateKey(nil)
	fs2 := startFake(t, other)
	fs2.keys = fs.keys
	_, port2, _ := net.SplitHostPort(fs2.addr)
	p2, _ := strconv.Atoi(port2)
	setHostKey(s.ID, s.HostKey)
	s.Port = p2
	// Save забыл бы ключ (сменился порт) — ставим порт напрямую.
	mu.Lock()
	st := load()
	st.Servers[0].Port = p2
	save(st)
	mu.Unlock()
	s, _ = Get(s.ID)
	if _, _, err := StartTerm(s, 80, 24); !errors.Is(err, ErrHostKeyChanged) {
		t.Fatalf("подмена ключа сервера не замечена: %v", err)
	}
}
