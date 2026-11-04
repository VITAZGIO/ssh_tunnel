package vault

import (
	"testing"

	"sshtunnel/internal/addons"
)

func TestVaultRoundTrip(t *testing.T) {
	addons.New(t.TempDir())
	if err := Create("short"); err == nil {
		t.Fatal("короткий главный пароль принят")
	}
	if err := Create("главный-пароль-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(Entry{Site: "https://www.Example.com/login", Login: "vit", Pass: "секрет"}); err != nil {
		t.Fatal(err)
	}
	Lock()
	if Unlocked() || len(Match("example.com")) != 0 {
		t.Fatal("закрытое хранилище отдаёт записи")
	}
	if err := Unlock("не тот"); err == nil {
		t.Fatal("чужой пароль открыл хранилище")
	}
	if err := Unlock("главный-пароль-1"); err != nil {
		t.Fatal(err)
	}
	m := Match("login.example.com")
	if len(m) != 1 || m[0].Pass != "секрет" || m[0].Site != "example.com" {
		t.Fatalf("поиск по поддомену: %+v", m)
	}
	if g := Gen(20); len(g) != 20 {
		t.Fatalf("генератор: %q", g)
	}
	Lock()
}
