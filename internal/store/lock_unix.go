//go:build (linux || darwin) && (amd64 || arm64)

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes an exclusive, non-blocking advisory lock.
//
// There is no Windows twin any more. There was one — LockFileEx, mandatory
// rather than advisory — and it went with the Windows release target: the
// Turso driver embeds no native library for windows/arm64, so half that
// matrix shipped a binary that could not open a store at all. See platform.go for the message an unsupported platform gets
// instead of a link error about this function.
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
