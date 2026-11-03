//go:build !windows && !linux

package lanscan

import (
	"net"
	"net/netip"
	"time"
)

// Остальные системы (сборка для разработки на macOS): только TCP.
func probe(_ LAN, a netip.Addr) probeResult { return tcpProbe(a, 600*time.Millisecond) }

func neighbors() map[netip.Addr]net.HardwareAddr { return nil }
