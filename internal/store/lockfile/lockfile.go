// Package lockfile lets one process at a time hold a store that only one
// server may use: the file and sqlite drivers.
package lockfile

import (
	"errors"
	"os"
)

// ErrLocked says another process holds the lock.
var ErrLocked = errors.New("another process holds the lock")

// Acquire creates path if needed and locks it. The lock lasts until the
// returned file is closed or the process ends.
func Acquire(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
