//go:build unix

package file

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive lock that the kernel releases when the
// process ends, however it ends.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLocked
	}
	return err
}
