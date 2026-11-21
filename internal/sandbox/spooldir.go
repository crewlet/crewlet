package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/flock"
)

// The spools a process writes live in a directory of its own under the node's
// spool directory, held by a lock for the process's life: the package doc's
// "Each process's own spool directory" has the argument.

const (
	// spoolDirPrefix names a process's own spool directory under the node's
	// ([CoordinatorOptions.SpoolDir]): spoolDirPrefix, a random part, and
	// spoolStaging while it is being made.
	spoolDirPrefix = "crewlet-sessions-"
	spoolStaging   = ".staging"
	// spoolLockName is the lock file inside it, held while its process lives.
	spoolLockName = "lock"
	// spoolDirAttempts is how many times a directory is made: one fails only
	// where another process's sweep took its lock in the instant between the
	// lock file's creation and its lock, so a third failure is a fault.
	spoolDirAttempts = 3
)

// ownSpools is a coordinator's own spool directory: made on first need
// ([Coordinator.spoolHere]), held by lock until the coordinator stops
// ([Coordinator.closeSpools]); dir stays named once released.
type ownSpools struct {
	mu     sync.Mutex
	dir    string
	lock   *flock.Lock
	closed bool
}

// errSpoolsClosed is a spool asked of a coordinator that has stopped.
var errSpoolsClosed = errors.New("sandbox: the node stopped, and spools no session on its disk")

// spoolHere is this coordinator's own spool directory, made on first need,
// and made again on the next where that failed: under a staging name, its lock
// taken, then renamed into place, so no sweep ever finds it under its own name
// unlocked while its process lives.
func (c *Coordinator) spoolHere() (string, error) {
	s := &c.spools
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return "", errSpoolsClosed
	case s.lock != nil:
		return s.dir, nil
	}
	var err error
	for range spoolDirAttempts {
		if s.dir, s.lock, err = makeSpoolDir(c.spoolDirectory()); err == nil {
			return s.dir, nil
		}
	}
	return "", err
}

// makeSpoolDir makes a spool directory under parent and takes its lock.
func makeSpoolDir(parent string) (string, *flock.Lock, error) {
	staged, err := os.MkdirTemp(parent, spoolDirPrefix+"*"+spoolStaging)
	if err != nil {
		return "", nil, fmt.Errorf("sandbox: a spool directory under %s: %w", parent, err)
	}
	lock, err := flock.Try(filepath.Join(staged, spoolLockName))
	if err != nil {
		_ = os.RemoveAll(staged)
		return "", nil, fmt.Errorf("sandbox: the lock of spool directory %s: %w", staged, err)
	}
	dir := strings.TrimSuffix(staged, spoolStaging)
	if err = os.Rename(staged, dir); err != nil {
		lock.Release()
		_ = os.RemoveAll(staged)
		return "", nil, fmt.Errorf("sandbox: spool directory %s: %w", dir, err)
	}
	return dir, lock, nil
}

// spoolDirectory is the node's spool directory, under which each process makes
// its own: [CoordinatorOptions.SpoolDir], or the node's temporary directory.
func (c *Coordinator) spoolDirectory() string {
	if c.spoolDir != "" {
		return c.spoolDir
	}
	return os.TempDir()
}

// sweepSpools removes every process's spool directory under the node's whose
// lock it can take, since the kernel dropped that lock when its process ended,
// however it ended; never one whose lock is held, its own included. Run by
// every start and every take: a container restart keeps the node's temporary
// directory, so a crash's spools would otherwise wait there for hours.
func (c *Coordinator) sweepSpools() {
	dirs, err := filepath.Glob(filepath.Join(c.spoolDirectory(), spoolDirPrefix+"*"))
	if err != nil {
		return
	}
	for _, dir := range dirs {
		lock, err := flock.Try(filepath.Join(dir, spoolLockName))
		if err != nil {
			continue
		}
		_ = os.RemoveAll(dir)
		lock.Release()
	}
}

// closeSpools releases this coordinator's spool directory once its last keep
// has ended ([Coordinator.Stop]), removing it where nothing is left in it; a
// spool a straggling take still writes keeps it, for the next sweep. Nothing
// is spooled after.
func (c *Coordinator) closeSpools() {
	s := &c.spools
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.lock == nil {
		return
	}
	s.lock.Release()
	s.lock = nil
	if os.Remove(filepath.Join(s.dir, spoolLockName)) == nil {
		_ = os.Remove(s.dir)
	}
}
