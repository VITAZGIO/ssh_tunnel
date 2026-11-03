package mobile

// Устройства сети для дополнительных приложений (ТЗ 4): Карта сети, SSH,
// Файлы устройства. Сами приложения — общие (sshtunnel/internal/…), здесь
// только то, что есть лишь у телефона: туннель, Keystore, уведомление.

import (
	"net"

	"sshtunnel/internal/devfiles"
	"sshtunnel/internal/devnet"
	"sshtunnel/internal/mesh"
	"sshtunnel/internal/sshapp"
)

// devnetSrc — туннель телефона как источник сети устройств.
type devnetSrc struct{ t *Tunnel }

func (d devnetSrc) Mesh() *mesh.Client {
	d.t.mu.Lock()
	c := d.t.core
	d.t.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Mesh()
}

func (d devnetSrc) Dial(network, addr string) (net.Conn, error) {
	d.t.mu.Lock()
	c := d.t.core
	d.t.mu.Unlock()
	if c == nil {
		return nil, devnet.ErrOff
	}
	return c.DialApp(addr)
}

// KeySealer шифрует ключ SSH ключом Android Keystore (реализует Kotlin):
// сам ключ Keystore из телефона не достать.
type KeySealer interface {
	Seal(plain []byte) ([]byte, error)
	Open(sealed []byte) ([]byte, error)
}

// SetKeySealer — до первого обращения к SSH (при запуске приложения).
func SetKeySealer(s KeySealer) { sshapp.SetSealer(s) }

// TransferListener — передачи «Файлов устройства» для уведомления с
// прогрессом. active == 0 — передач нет, уведомление убрать.
type TransferListener interface {
	OnTransfer(active int, name string, done, total int64)
}

// SetTransferListener — nil снимает.
func SetTransferListener(l TransferListener) {
	if l == nil {
		devfiles.SetProgress(nil)
		return
	}
	devfiles.SetProgress(func(active int, name string, done, total int64) {
		l.OnTransfer(active, name, done, total)
	})
}
