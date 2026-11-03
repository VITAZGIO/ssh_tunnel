package sshapp

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"sshtunnel/internal/devnet"
)

// ErrHostKeyChanged — сервер показал не тот ключ, что при первом входе.
var ErrHostKeyChanged = errors.New("ключ сервера изменился")

// hostInfo — что узнали о ключе сервера при входе.
type hostInfo struct {
	Fingerprint string // SHA256:…
	New         bool   // ключ увиден впервые и запомнен
}

func addr(s Server) string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// hostKeyLine — ключ сервера одной строкой («ssh-ed25519 AAAA…»).
func hostKeyLine(k ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

// dial входит на сервер. Ключ сервера: первый раз запоминается, потом
// сверяется (TOFU, как ssh с accept-new).
func dial(s Server, auth []ssh.AuthMethod) (*ssh.Client, hostInfo, error) {
	var info hostInfo
	cfg := &ssh.ClientConfig{
		User:    s.User,
		Auth:    auth,
		Timeout: 20 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			info.Fingerprint = ssh.FingerprintSHA256(key)
			line := hostKeyLine(key)
			if s.HostKey == "" {
				info.New = true
				return setHostKey(s.ID, line)
			}
			if s.HostKey != line {
				return fmt.Errorf("%w (сейчас %s). Если сервер переустанавливали — «Забыть ключ сервера»", ErrHostKeyChanged, info.Fingerprint)
			}
			return nil
		},
	}
	conn, err := devnet.Dial("tcp", addr(s))
	if err != nil {
		return nil, info, fmt.Errorf("нет связи с %s: %w", addr(s), err)
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr(s), cfg)
	if err != nil {
		conn.Close()
		return nil, info, explain(err)
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), info, nil
}

// explain — понятная ошибка входа.
func explain(err error) error {
	if errors.Is(err, ErrHostKeyChanged) {
		return err
	}
	t := err.Error()
	switch {
	case strings.Contains(t, "unable to authenticate"):
		return errors.New("сервер не принял вход (неверный пароль или ключ ещё не установлен)")
	case strings.Contains(t, "i/o timeout"):
		return errors.New("сервер не ответил вовремя")
	}
	return err
}

// keyAuth — вход ключом программы.
func keyAuth() ([]ssh.AuthMethod, error) {
	sg, err := signer()
	if err != nil {
		return nil, err
	}
	return []ssh.AuthMethod{ssh.PublicKeys(sg)}, nil
}

// InstallKey входит паролем (один раз, нигде не хранится) и дописывает ключ
// программы в ~/.ssh/authorized_keys, если его там ещё нет. Сервер — Linux
// (или другой Unix с sh).
func InstallKey(s Server, password string) (hostInfo, error) {
	pub, err := PublicKey()
	if err != nil {
		return hostInfo{}, err
	}
	auth := []ssh.AuthMethod{
		ssh.Password(password),
		ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range ans {
				ans[i] = password
			}
			return ans, nil
		}),
	}
	c, info, err := dial(s, auth)
	if err != nil {
		return info, err
	}
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		return info, err
	}
	defer sess.Close()
	// Ключ — без кавычек и спецсимволов (base64 и имя без пробелов), его
	// можно вставить в одинарные кавычки.
	cmd := "umask 077; mkdir -p ~/.ssh && touch ~/.ssh/authorized_keys && " +
		"(grep -qF '" + strings.Fields(pub)[1] + "' ~/.ssh/authorized_keys || echo '" + pub + "' >> ~/.ssh/authorized_keys)"
	if out, err := sess.CombinedOutput(cmd); err != nil {
		return info, fmt.Errorf("не удалось записать ключ: %v %s", err, strings.TrimSpace(string(out)))
	}
	// Проверяем, что ключом теперь пускает.
	s, _ = Get(s.ID)
	ka, err := keyAuth()
	if err != nil {
		return info, err
	}
	c2, _, err := dial(s, ka)
	if err != nil {
		return info, fmt.Errorf("ключ записан, но вход по нему не прошёл: %w", err)
	}
	c2.Close()
	return info, nil
}
