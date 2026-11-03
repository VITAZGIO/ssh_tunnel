package sshapp

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh/agent"

	"sshtunnel/internal/devnet"
)

// «Открыть» на ПК: внешний терминал с ssh. Ключ на диск не кладётся — его
// отдаёт свой агент программы (именованный канал Windows / сокет Linux),
// который живёт agentLife и закрывается: ssh нужен ключ только при входе.
const agentLife = 2 * time.Minute

// ErrNoTerminal — запустить внешний терминал негде: страница откроет
// встроенный.
var ErrNoTerminal = errors.New("нет внешнего терминала")

// Open открывает терминал с входом на сервер.
func Open(s Server) error {
	sshBin, ok := findSSH()
	if !ok {
		return ErrNoTerminal
	}
	k, err := privateKey()
	if err != nil {
		return err
	}
	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{PrivateKey: k, Comment: "ssh_tunnel"}); err != nil {
		return err
	}
	ag, err := startAgent(kr)
	if err != nil {
		return err
	}
	time.AfterFunc(agentLife, ag.Close)

	host, port := s.Host, s.Port
	// Окно с туннелем (обычная версия) ведёт ssh через ядро — так адреса
	// сети устройств работают и в режиме прокси. Окно VPN-версии туннеля не
	// держит: ssh идёт сам, его ловит адаптер.
	if devnet.Routed() {
		lp, err := forward(addr(s))
		if err != nil {
			ag.Close()
			return err
		}
		host, port = "127.0.0.1", lp
	}
	kh := filepath.Join(dir(), "ssh_known_hosts")
	writeKnownHosts(kh)
	args := []string{
		"-p", strconv.Itoa(port),
		"-o", "IdentityAgent=" + agentPath(ag),
		"-o", "IdentitiesOnly=no",
		"-o", "HostKeyAlias=sshtunnel-" + s.ID,
		"-o", "UserKnownHostsFile=" + kh,
		"-o", "StrictHostKeyChecking=accept-new",
		s.User + "@" + host,
	}
	if err := launch(s.Name, sshBin, args); err != nil {
		ag.Close()
		return err
	}
	return nil
}

// writeKnownHosts — запомненные ключи серверов для ssh терминала (под
// псевдонимом sshtunnel-<id>). Не секрет.
func writeKnownHosts(path string) {
	var out []byte
	if old, err := os.ReadFile(path); err == nil {
		out = old
	}
	for _, s := range Servers() {
		if s.HostKey == "" {
			continue
		}
		line := "sshtunnel-" + s.ID + " " + s.HostKey + "\n"
		if !containsLine(out, line) {
			out = append(out, line...)
		}
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, out, 0o600)
}

func containsLine(data []byte, line string) bool {
	for len(data) > 0 {
		i := 0
		for i < len(data) && data[i] != '\n' {
			i++
		}
		if string(data[:i])+"\n" == line {
			return true
		}
		if i == len(data) {
			break
		}
		data = data[i+1:]
	}
	return false
}

// forward — одноразовый проброс 127.0.0.1:порт → target через ядро.
// Ждёт ssh минуту, обслуживает одно соединение.
func forward(target string) (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	go func() {
		defer ln.Close()
		ln.(*net.TCPListener).SetDeadline(time.Now().Add(time.Minute))
		c, err := ln.Accept()
		if err != nil {
			return
		}
		r, err := devnet.Dial("tcp", target)
		if err != nil {
			c.Close()
			return
		}
		go func() { io.Copy(r, c); r.Close() }()
		io.Copy(c, r)
		c.Close()
	}()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
