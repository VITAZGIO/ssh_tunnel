package vpnlayer

import "testing"

func TestEgressTableFromEnv(t *testing.T) {
	cases := map[string]int{
		"":      0,
		"0":     0,
		"-1":    0,
		"abc":   0,
		"253":   0, // системные таблицы (main=254, default=253, local=255)
		"255":   0,
		"7098":  7098,
		"1":     1,
		"65535": 65535,
	}
	for v, want := range cases {
		t.Setenv("SSH_TUNNEL_ROUTE_TABLE", v)
		if got := egressTableFromEnv(); got != want {
			t.Errorf("egressTableFromEnv() с %q = %d, ожидалось %d", v, got, want)
		}
	}
}
