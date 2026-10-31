package appprofiles

import (
	"testing"

	"sshtunnel/internal/config"
)

func cfgWith(host string) config.Config {
	c := config.Default()
	c.Profiles[0].Host = host
	return c
}

// Новый профиль — с нуля; переключение туда-обратно возвращает настройки
// каждого; удаление действующего переводит на соседний.
func TestAddSwitchDelete(t *testing.T) {
	s := New(t.TempDir())
	if st := s.Load(); len(st.Profiles) != 1 || st.Active != "p1" {
		t.Fatalf("начальное состояние %+v", st)
	}
	mine := cfgWith("my.example")
	st, fresh, err := s.Add(mine)
	if err != nil || st.Active != "p2" || len(st.Profiles) != 2 {
		t.Fatalf("Add: %+v %v", st, err)
	}
	if fresh.Active().Host != "" {
		t.Fatalf("новый профиль не пустой: %q", fresh.Active().Host)
	}

	mom := cfgWith("mom.example")
	st, back, err := s.Switch("p1", mom)
	if err != nil || st.Active != "p1" || back.Active().Host != "my.example" {
		t.Fatalf("Switch p1: %+v host=%q %v", st, back.Active().Host, err)
	}
	_, again, err := s.Switch("p2", back)
	if err != nil || again.Active().Host != "mom.example" {
		t.Fatalf("Switch p2: host=%q %v", again.Active().Host, err)
	}

	st, next, err := s.Delete("p2", again)
	if err != nil || st.Active != "p1" || next == nil || next.Active().Host != "my.example" {
		t.Fatalf("Delete: %+v next=%v %v", st, next, err)
	}
	if _, _, err := s.Delete("p1", *next); err == nil {
		t.Fatal("последний профиль удалился")
	}
}

func TestSetEnabledKeepsProfiles(t *testing.T) {
	s := New(t.TempDir())
	if _, _, err := s.Add(cfgWith("a")); err != nil {
		t.Fatal(err)
	}
	st, err := s.SetEnabled(true)
	if err != nil || !st.Enabled || len(st.Profiles) != 2 {
		t.Fatalf("%+v %v", st, err)
	}
	if st, _ = s.SetEnabled(false); st.Enabled || len(st.Profiles) != 2 {
		t.Fatalf("выключение потеряло профили: %+v", st)
	}
}
