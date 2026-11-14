//go:build !linux

package ports

// Scan is not implemented on this OS yet and returns [ErrUnsupported].
func Scan() (*Snapshot, error) {
	return nil, ErrUnsupported
}
