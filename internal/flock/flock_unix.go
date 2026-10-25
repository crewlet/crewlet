//go:build linux || darwin

package flock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockExclusive takes an exclusive, non-blocking flock on file, reporting
// whether it got it. A signal interrupting the call is asked again rather
// than read as the lock being held.
func lockExclusive(file *os.File) (bool, error) {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB) //nolint:gosec // a descriptor fits an int on every platform this builds for
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}

func unlock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN) //nolint:gosec // a descriptor fits an int on every platform this builds for
}
