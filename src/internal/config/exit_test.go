package config

import (
	"reflect"
	"testing"
)

func TestExitViaЧиститсяНоУдалённыйОстаётся(t *testing.T) {
	c := Default()
	b := c.AddProfile("Лондон", "")
	v := c.AddProfile("Варшава", "")
	a := c.Profiles[0]
	a.ExitVia = []string{a.ID, b.ID, "", b.ID, v.ID, "p_gone"}
	c.SetProfile(a)
	c.normalize()
	got := c.Profiles[0].ExitVia
	if !reflect.DeepEqual(got, []string{b.ID, v.ID}) {
		t.Fatalf("цепочка после чистки: %v", got)
	}
	// Удалили Лондон — id остаётся, цепочка не укорачивается молча.
	c.RemoveProfile(b.ID)
	c.normalize()
	chain := c.ExitChain(c.Profiles[0])
	if len(chain) != 2 || chain[0].Host != "" || chain[0].Name != "удалённый сервер" {
		t.Fatalf("цепочка с удалённым сервером: %+v", chain)
	}
}
