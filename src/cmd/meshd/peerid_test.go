package main

import "testing"

// ТЗ 18: тот же id, что internal/mesh.PeerID и nodeapi.PeerID (у них тот
// же образец в тестах).
func TestPeerIDОбразец(t *testing.T) {
	if got := peerID("dev-1"); got != "436521497eb1603d3e022e80" {
		t.Fatalf("peerID: %q", got)
	}
	if peerID("") != "" {
		t.Fatal("пустой id")
	}
}
