//go:build !windows && !linux

package procinfo

// Lookup на не-Windows системах не реализован: определение процесса по порту
// нужно ради интерфейса на Windows, а тесты и сборка на Linux обходятся без него.
func Lookup(localPort uint16) (string, int) { return "", 0 }

// LookupUDP — то же для UDP; здесь тоже не реализован.
func LookupUDP(localPort uint16) (string, int) { return "", 0 }
