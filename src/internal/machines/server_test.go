package machines

import "testing"

// График: история сервера до первой своей точки, дальше — свои показания;
// без сервера — только свои.
func TestГрафикСерверПлюсСвои(t *testing.T) {
	h := &serverHistory{Since: 1000}
	h.Reporters = append(h.Reporters, struct {
		ID       string      `json:"id"`
		Name     string      `json:"name"`
		Self     bool        `json:"self"`
		Shared   bool        `json:"shared"`
		Last     int64       `json:"last"`
		Machines []srvSeries `json:"machines"`
	}{ID: "aaa", Self: true, Machines: []srvSeries{{ID: "m1", T: []int64{1000, 1100, 1200, 1300}, CPU: []float64{1, 2, 3, 4}}}})
	srv := &srvState{Name: "VPS"}
	local := &Reading{Hist: []float64{30, 40}, HistT: []int64{1250, 1280}}

	s := seriesOf(h, srv, true, "", "m1", local)
	if s.Src != "server" || s.Server != "VPS" || s.Join != 1250 {
		t.Fatalf("откуда: %+v", s)
	}
	want := []int64{1000, 1100, 1200, 1250, 1280}
	if len(s.T) != len(want) {
		t.Fatalf("точки: %v", s.T)
	}
	for i := range want {
		if s.T[i] != want[i] {
			t.Fatalf("точки: %v", s.T)
		}
	}
	if s.CPU[3] != 30 {
		t.Fatalf("свои после сервера: %v", s.CPU)
	}
	// Чужая машина того же id не берётся.
	if o := seriesOf(h, srv, false, "bbb", "m1", nil); o.Src != "local" || len(o.T) != 0 {
		t.Fatalf("чужой читатель: %+v", o)
	}
	// Сервера нет — только свои.
	if o := seriesOf(nil, nil, true, "", "m1", local); o.Src != "local" || len(o.T) != 2 || o.Join != 0 {
		t.Fatalf("без сервера: %+v", o)
	}
}
