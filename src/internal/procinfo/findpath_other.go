//go:build !windows

package procinfo

// PathByName — значки программ есть только на Windows.
func PathByName(name string) string { return "" }
