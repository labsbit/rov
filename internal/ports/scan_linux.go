//go:build linux

package ports

// Scan reads the listening TCP sockets of the current network namespace and
// the process table from /proc. It is cheap enough to call on every CLI
// invocation and every couple of seconds from the TUI.
func Scan() (*Snapshot, error) {
	return ScanAt("/proc")
}
