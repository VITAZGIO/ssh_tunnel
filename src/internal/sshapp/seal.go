package sshapp

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Sealer шифрует секрет ключом, который держит система: Windows — DPAPI
// (учётная запись пользователя), Android — Keystore (ставит ядро телефона
// через SetSealer). На Linux своего нет: файл с правами 600 (см.
// docs/decisions.md).
type Sealer interface {
	Seal(plain []byte) ([]byte, error)
	Open(sealed []byte) ([]byte, error)
}

var (
	sealMu sync.Mutex
	sealer = osSealer() // nil — шифровать нечем
)

// SetSealer — внешний шифровальщик (Android Keystore).
func SetSealer(s Sealer) {
	sealMu.Lock()
	sealer = s
	sealMu.Unlock()
}

func curSealer() Sealer {
	sealMu.Lock()
	defer sealMu.Unlock()
	return sealer
}

// Файл ключа: первый байт — чем закрыт (S — шифровальщик системы, P —
// открыто, только права 600), дальше — 32 байта зерна ed25519.
func keyPath() string { return filepath.Join(dir(), "ssh_key.bin") }

var keyMu sync.Mutex

// privateKey — ключ программы; нет — создаёт.
func privateKey() (ed25519.PrivateKey, error) {
	keyMu.Lock()
	defer keyMu.Unlock()
	if data, err := os.ReadFile(keyPath()); err == nil && len(data) > 1 {
		var seed []byte
		switch data[0] {
		case 'S':
			s := curSealer()
			if s == nil {
				return nil, errors.New("ключ зашифрован, а расшифровать сейчас нечем")
			}
			if seed, err = s.Open(data[1:]); err != nil {
				return nil, errors.New("не удалось расшифровать ключ SSH: " + err.Error())
			}
		case 'P':
			seed = data[1:]
		}
		if len(seed) != ed25519.SeedSize {
			return nil, errors.New("файл ключа SSH испорчен: удали ssh_key.bin и установи ключ на серверы заново")
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	out := append([]byte{'P'}, priv.Seed()...)
	if s := curSealer(); s != nil {
		sealed, err := s.Seal(priv.Seed())
		if err != nil {
			return nil, errors.New("не удалось зашифровать ключ SSH: " + err.Error())
		}
		out = append([]byte{'S'}, sealed...)
	}
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath(), out, 0o600); err != nil {
		return nil, err
	}
	return priv, nil
}

func signer() (ssh.Signer, error) {
	k, err := privateKey()
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(k)
}

// Signer — ключ программы для входа на свои серверы (плагин «Машины»
// читает показания по SSH тем же ключом, что ставит плагин SSH).
func Signer() (ssh.Signer, error) { return signer() }

// PublicKey — открытая часть ключа одной строкой для authorized_keys.
func PublicKey() (string, error) {
	s, err := signer()
	if err != nil {
		return "", err
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "device"
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
	return line + " ssh_tunnel@" + strings.ReplaceAll(host, " ", "_"), nil
}
