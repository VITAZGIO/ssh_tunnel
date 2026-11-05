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

func TestImportExportCSV(t *testing.T) {
	addons.New(t.TempDir())
	if err := Create("главный-пароль-2"); err != nil {
		t.Fatal(err)
	}
	defer Lock()
	chrome := "\xef\xbb\xbfname,url,username,password,note\nexample.com,https://www.example.com/login,bob,p1,\nya.ru,https://ya.ru/,al,p2,hi\n"
	a, u, k, err := ImportCSV([]byte(chrome))
	if err != nil || a != 2 || u != 0 || k != 0 {
		t.Fatal(a, u, k, err)
	}
	yandex := "url,username,password\nhttps://example.com/,bob,p1\nhttps://ya.ru/,al,p3\nhttps://new.org/,x,p4\n"
	a, u, k, err = ImportCSV([]byte(yandex))
	if err != nil || a != 1 || u != 1 || k != 1 {
		t.Fatal(a, u, k, err)
	}
	if m := Match("ya.ru"); len(m) != 1 || m[0].Pass != "p3" {
		t.Fatal(m)
	}
	if _, _, _, err := ImportCSV([]byte("a,b\n1,2\n")); err == nil {
		t.Fatal("нет колонки пароля — должна быть ошибка")
	}
	if _, _, err := ExportCSV("не тот"); err == nil {
		t.Fatal("экспорт без главного пароля")
	}
}
