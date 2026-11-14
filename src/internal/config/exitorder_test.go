package config

import (
	"reflect"
	"strings"
	"testing"
)

// Цепочка выхода из панели (ТЗ 16).

func exitConfig() (Config, Profile, Profile, Profile) {
	c := Default()
	a := c.Profiles[0]
	a.Name, a.Host, a.SSHPort = "Франкфурт", "FRA.example", 22
	c.SetProfile(a)
	b := c.AddProfile("Лондон", "")
	b.Host, b.SSHPort = "lon.example", 2222
	c.SetProfile(b)
	v := c.AddProfile("Варшава", "")
	v.Host = "war.example"
	c.SetProfile(v)
	return c, a, b, v
}

func TestКлючСервера(t *testing.T) {
	for _, c := range []struct {
		p    Profile
		want string
	}{
		{Profile{Host: " FRA.Example "}, "fra.example:22"},
		{Profile{Host: "lon.example", SSHPort: 2222}, "lon.example:2222"},
		{Profile{Host: "2001:db8::1", SSHPort: 22}, "[2001:db8::1]:22"},
		{Profile{}, ""},
	} {
		if got := ServerKey(c.p); got != c.want {
			t.Errorf("%+v: %q, ждали %q", c.p, got, c.want)
		}
	}
}

func TestЦепочкаИзПанелиНовееПобеждает(t *testing.T) {
	c, a, b, v := exitConfig()
	ok, note, err := c.ApplyExitOrder(ExitOrder{Entry: "fra.example:22", Via: []string{"lon.example:2222"}, T: 100, By: "Франкфурт"})
	if !ok || note != "" || err != nil {
		t.Fatalf("первая: %v %q %v", ok, note, err)
	}
	got, _ := c.ProfileByID(a.ID)
	if !reflect.DeepEqual(got.ExitVia, []string{b.ID}) || got.ExitT != 100 || got.ExitBy != "Франкфурт" {
		t.Fatalf("профиль: %+v", got)
	}
	// Старая правка другой панели, пришедшая позже, — ничего не меняет.
	if ok, _, _ := c.ApplyExitOrder(ExitOrder{Entry: "fra.example:22", Via: []string{"war.example:22"}, T: 90, By: "Лондон"}); ok {
		t.Fatal("старая цепочка применилась")
	}
	// Та же самая ещё раз (переподключение) — тоже ничего.
	if ok, _, _ := c.ApplyExitOrder(ExitOrder{Entry: "fra.example:22", Via: []string{"lon.example:2222"}, T: 100}); ok {
		t.Fatal("повтор применился")
	}
	// Новее — применяется.
	if ok, _, _ := c.ApplyExitOrder(ExitOrder{Entry: "fra.example:22", Via: []string{"war.example:22", "lon.example:2222"}, T: 200, By: "Лондон"}); !ok {
		t.Fatal("новая не применилась")
	}
	got, _ = c.ProfileByID(a.ID)
	if !reflect.DeepEqual(got.ExitVia, []string{v.ID, b.ID}) || got.ExitBy != "Лондон" {
		t.Fatalf("профиль: %+v", got)
	}

	// Человек поменял сам, часы устройства отстают: его правка всё равно
	// новее приказа, на который он ответил, и тот больше не возвращается.
	TouchExit(&got, 50)
	got.ExitVia = nil
	c.SetProfile(got)
	if got.ExitT != 201 || got.ExitBy != "" {
		t.Fatalf("правка на устройстве: %+v", got)
	}
	if ok, _, _ := c.ApplyExitOrder(ExitOrder{Entry: "fra.example:22", Via: []string{"lon.example:2222"}, T: 200}); ok {
		t.Fatal("приказ панели перебил правку человека")
	}
	// Очистка цепочки из панели.
	if ok, _, _ := c.ApplyExitOrder(ExitOrder{Entry: "fra.example:22", T: 300, By: "Франкфурт"}); !ok {
		t.Fatal("очистка не применилась")
	}
	if got, _ := c.ProfileByID(a.ID); len(got.ExitVia) != 0 {
		t.Fatalf("цепочка не очищена: %v", got.ExitVia)
	}
}

func TestЦепочкаИзПанелиНуженПрофиль(t *testing.T) {
	c, a, b, _ := exitConfig()
	ok, note, err := c.ApplyExitOrder(ExitOrder{Entry: "fra.example:22", Via: []string{"ams.example:22", "lon.example:2222"}, T: 5, By: "X"})
	if !ok || err != nil || !strings.Contains(note, "нужен профиль сервера ams.example:22") {
		t.Fatalf("%v %q %v", ok, note, err)
	}
	got, _ := c.ProfileByID(a.ID)
	chain := c.ExitChain(got)
	if len(chain) != 2 || chain[0].Host != "" || chain[0].Name != "нужен профиль сервера ams.example:22" || chain[1].ID != b.ID {
		t.Fatalf("цепочка: %+v", chain)
	}
	if m := c.ExitMissing(got); len(m) != 1 || m[0] != "ams.example:22" {
		t.Fatalf("недостающие: %v", m)
	}
	// Цепочка не укорачивается молча при чистке.
	c.normalize()
	got, _ = c.ProfileByID(a.ID)
	if len(got.ExitVia) != 2 || !strings.HasPrefix(got.ExitVia[0], ExitNeedPrefix) {
		t.Fatalf("после чистки: %v", got.ExitVia)
	}
	// Добавили профиль Амстердама — звено стало им.
	ams := c.AddProfile("Амстердам", "")
	ams.Host = "AMS.example"
	c.SetProfile(ams)
	c.normalize()
	got, _ = c.ProfileByID(a.ID)
	if !reflect.DeepEqual(got.ExitVia, []string{ams.ID, b.ID}) || len(c.ExitMissing(got)) != 0 {
		t.Fatalf("после добавления профиля: %v", got.ExitVia)
	}
}

func TestЦепочкаИзПанелиОшибки(t *testing.T) {
	c, _, _, _ := exitConfig()
	if _, _, err := c.ApplyExitOrder(ExitOrder{Entry: "nope:22", T: 1, By: "X"}); err == nil {
		t.Error("вход без профиля принят")
	}
	if _, _, err := c.ApplyExitOrder(ExitOrder{Entry: "fra.example:22", Via: []string{"fra.example:22"}, T: 1}); err == nil {
		t.Error("цепочка через самого себя принята")
	}
	if _, _, err := c.ApplyExitOrder(ExitOrder{Entry: "fra.example:22", Via: []string{"a:1", "b:2", "c:3"}, T: 1}); err == nil {
		t.Error("длинная цепочка принята")
	}
}

func TestПравкаЦепочкиСЭкрана(t *testing.T) {
	c, a, b, v := exitConfig()
	c.ApplyExitOrder(ExitOrder{Entry: "fra.example:22", Via: []string{"lon.example:2222"}, T: 100, By: "Франкфурт"})
	// Экран сохранил всё, не трогая цепочку, — время и источник те же.
	next := c
	next.Profiles = append([]Profile(nil), c.Profiles...)
	if note := StampExitEdits(c, &next, 50); note != "" {
		t.Fatal(note)
	}
	if p, _ := next.ProfileByID(a.ID); p.ExitT != 100 || p.ExitBy != "Франкфурт" {
		t.Fatalf("без правки: %+v", p)
	}
	// Человек поменял цепочку в свежем окне — правка его и новее панели.
	p, _ := next.ProfileByID(a.ID)
	p.ExitVia = []string{v.ID}
	next.SetProfile(p)
	StampExitEdits(c, &next, 50)
	if p, _ := next.ProfileByID(a.ID); p.ExitT != 101 || p.ExitBy != "" || p.ExitVia[0] != v.ID {
		t.Fatalf("правка человека: %+v", p)
	}
	// Окно открыто до приказа панели: его правка не затирает свежую цепочку.
	stale := c
	stale.Profiles = append([]Profile(nil), c.Profiles...)
	sp, _ := stale.ProfileByID(a.ID)
	sp.ExitVia, sp.ExitT, sp.ExitBy = nil, 0, ""
	stale.SetProfile(sp)
	if note := StampExitEdits(c, &stale, 50); !strings.Contains(note, "Франкфурт") {
		t.Fatalf("нет предупреждения: %q", note)
	}
	if p, _ := stale.ProfileByID(a.ID); len(p.ExitVia) != 1 || p.ExitVia[0] != b.ID || p.ExitT != 100 {
		t.Fatalf("устаревшее окно затёрло цепочку: %+v", p)
	}
}
