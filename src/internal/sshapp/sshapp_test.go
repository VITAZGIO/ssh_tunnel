package sshapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServersAndKey(t *testing.T) {
	SetDir(t.TempDir())
	s, err := Save(Server{Name: "ubuntu", Host: "198.19.0.2", User: "root"})
	if err != nil || s.ID == "" || s.Port != 22 {
		t.Fatalf("save: %+v %v", s, err)
	}
	if _, err := Save(Server{Host: "x; rm -rf /", User: "root"}); err == nil {
		t.Fatal("адрес с «;» принят")
	}
	setHostKey(s.ID, "ssh-ed25519 AAAA")
	s.Port = 2222
	s2, _ := Save(s)
	if s2.HostKey != "" {
		t.Fatal("сменился порт, а ключ сервера остался")
	}
	pub, err := PublicKey()
	if err != nil || !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("pub: %q %v", pub, err)
	}
	again, _ := PublicKey()
	if again != pub {
		t.Fatal("ключ пересоздался")
	}
	raw, _ := os.ReadFile(filepath.Join(dir(), "ssh_key.bin"))
	if curSealer() != nil && raw[0] != 'S' {
		t.Fatal("ключ лежит незашифрованным, хотя шифровальщик есть")
	}
	if err := Delete(s.ID); err != nil || len(Servers()) != 0 {
		t.Fatal("delete")
	}
}
