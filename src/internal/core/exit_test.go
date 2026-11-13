package core

import "testing"

type exitDriver struct {
	*fakeDriver
	exit *ExitPath
}

func (d exitDriver) Exit() *ExitPath { return d.exit }

func TestStateПоказываетЦепочкуТолькоПриСвязи(t *testing.T) {
	d := exitDriver{fakeDriver: &fakeDriver{}, exit: &ExitPath{Path: []string{"Франкфурт", "Лондон"}, State: ExitDown, Hop: "Лондон"}}
	m := NewMachine(d, Options{})
	defer m.Shutdown()
	if st := idle(t, m); st.Exit != nil {
		t.Fatal("цепочка показана без связи")
	}
	on := true
	m.Set(&on, nil)
	st := idle(t, m)
	if st.Exit == nil || st.Exit.State != ExitDown || st.Exit.Hop != "Лондон" {
		t.Fatalf("цепочка в состоянии: %+v", st.Exit)
	}
	// Цепочка не гасит круглую кнопку: интернет выключен ею, а не связью.
	if st.Tunnel.Phase != PhaseOn {
		t.Fatalf("кнопка %v", st.Tunnel.Phase)
	}
}
