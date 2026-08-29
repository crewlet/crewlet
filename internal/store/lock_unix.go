//go:build !windows

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes an exclusive, non-blocking advisory lock.
//
// NON-BLOCKING deliberately. A blocking lock would make `crewlet secrets set`
// hang against a running engine with no output, which reads as a wedged
// command — and the honest answer is not "wait for the engine to stop" but
// "the engine has this; stop it or use another route".
//
// flock rather than fcntl: an fcntl lock is dropped when ANY descriptor for
// the file is closed in this process, so an unrelated open/close of the
// sidecar would silently release a lock the engine believes it holds.
func tryLock(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EWOULDBLOCK):
		return false, nil
	default:
		return false, err
	}
}

func unlock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
