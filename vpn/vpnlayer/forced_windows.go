package vpnlayer

import "net/netip"

// forcedSource — источники «только через сервер» есть только на Linux
// (контейнеры Docker, docker_linux.go).
func (s *sys) forcedSource(netip.Addr) bool { return false }
