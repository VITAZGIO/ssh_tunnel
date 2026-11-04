package machines

import "testing"

func TestParseScript(t *testing.T) {
	out := "0.52 0.40 0.30 1/200 999\n---\n4\n---\ncpu  100 0 100 800 0 0 0 0 0 0\ncpu  150 0 150 900 0 0 0 0 0 0\n---\n" +
		"MemTotal:       8000000 kB\nMemAvailable:   2000000 kB\n---\n/dev/sda1 100000 40000 60000 40% /\n---\n51000\n---\n3600.5 100.0\n"
	r := parseScript(out)
	if r.Load != 0.52 || r.Cores != 4 || r.CPU < 49 || r.CPU > 51 || r.MemTotal != 8000000*1024 || r.MemUsed != 6000000*1024 ||
		r.DiskTot != 100000 || r.DiskUsed != 40000 || r.Temp != 51 || r.Uptime != 3600 {
		t.Fatalf("%+v", r)
	}
}
