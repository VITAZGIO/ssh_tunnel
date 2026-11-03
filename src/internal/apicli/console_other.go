//go:build !windows

package apicli

// AttachConsole — консоль нужна только оконному exe Windows.
func AttachConsole() {}
