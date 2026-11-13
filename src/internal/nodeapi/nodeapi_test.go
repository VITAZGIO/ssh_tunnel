package nodeapi

import "testing"

// ТЗ 18: тот же id, что internal/mesh.PeerID и peerID в cmd/meshd.
func TestPeerIDОбразец(t *testing.T) {
	if got := PeerID("dev-1"); got != "436521497eb1603d3e022e80" {
		t.Fatalf("PeerID: %q", got)
	}
}
