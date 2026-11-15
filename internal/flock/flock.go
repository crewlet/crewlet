// Package flock is the engine's one implementation of flock(2), for two uses.
//
// The first is the one exclusive cross-process lock this engine takes on a
// file ([Try]): the node database's ([github.com/crewlet/crewlet/internal/store]),
// the embedded broker's stream directory
// ([github.com/crewlet/crewlet/internal/queue/jetstream]), a harness's
// shared login ([github.com/crewlet/crewlet/internal/hostbox]) and a Gitlet node's
// cache directory ([github.com/crewlet/crewlet/internal/gitlet/cache]), whose
// boot empties directories only the process that filled them may be using.
//
// The second is a PIN ([Pin], [Pinned]): a lock on an open file description
// the engine hands to a child, so that the kernel counts the description's
// holders for it. See "A pin on a description a child inherits" below.
//
// # One implementation, because the subtleties are not obvious
//
// Each of those once carried its own copy of the syscall (ADR-0008), and each
// copy had to rediscover the same three things on its own:
//
//   - flock, never fcntl. An fcntl lock is dropped when ANY descriptor for the
//     file is closed in the process, so an unrelated open and close of the lock
//     file would silently free a lock its holder still believes it has.
//   - Never unlink the lock file. A peer that has opened it and is about to
//     lock would take a lock on an unlinked inode while a third process created
//     a fresh file and locked that, and both would believe they held it. An
//     empty file left behind costs nothing; a lock that excludes nobody does.
//     That hazard is a peer opening the file BY NAME, so it belongs to [Try]'s
//     lock files alone: a pinned file is opened by nobody by name, and may go.
//   - Stamp the holder after the lock is held, never before, so a refused
//     taker always reads a live holder's line rather than a racing one's.
//
// # An OS lock rather than a pid file
//
// A pid file has to answer "is that process still alive?", and the portable
// answer is worse than the question: pids are reused and a crashed holder
// leaves a file nobody can safely clear. An advisory lock is released by the
// KERNEL when its holder exits, however it exits — a crash, a kill -9, an OOM —
// so there is no stale state to reap and no liveness check to get wrong.
//
// # Per descriptor, so two takers in one process exclude each other
//
// An flock belongs to the open file description, and [Try] opens a fresh one
// every call: two takers inside one process exclude each other exactly as two
// processes do. A caller that wants handles in one process to SHARE a claim —
// the node database, where two pools on one file are one pool with more
// connections — keeps one [Lock] and counts its own sharers on top of it.
//
// # A pin on a description a child inherits
//
// An flock belongs to the open file description, and fork and exec share the
// description rather than copy it. So an exclusive lock taken on a
// description before a child is handed it is held by every process that holds
// that description (the child, and whatever the child starts that inherits
// it), and the kernel drops it when the LAST of them closes it: the instant a
// pipe's reader would have seen its end. [Pin] takes that lock. [Pinned]
// asks whether anybody still holds it, by trying a shared lock on a SECOND
// description of the same file, which a description holding the pin refuses.
//
// The probe must be another description. On the pinned one flock does not
// refuse: it converts the pin to the shared lock asked for, and the probe's
// release would then drop the pin for every holder. A second open of the
// file, or of /proc/self/fd/<n> on Linux, is another description; a dup of the
// descriptor is the same one.
//
// What a pin cannot see is a holder that steps outside it: a process that
// flocks and unlocks the description it inherited drops the pin while it
// still holds the description, and one that opens the file again writes
// through a description the pin does not count. A pin counts the holders of
// the description it was taken on, and nothing else.
//
// A pinned file may be unlinked once it is pinned: the lock lives on the
// inode, which stays while any description of it is open.
//
// # Advisory, and honest about it
//
// A process that never asks does not notice. Every taker of these files is this
// binary, so that is the right trade; what it guarantees is that two crewlet
// processes never both hold one.
package flock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// ErrHeld reports that another holder has the lock: another process, or
// another taker in this one. Every refusal [Try] makes for that reason is a
// [*HeldError], which unwraps to it.
var ErrHeld = errors.New("flock: held by another holder")

// HeldError is a lock someone else holds, with who that is as far as their
// stamp says — the one diagnostic a refused taker has to go on.
type HeldError struct {
	// Path is the lock file.
	Path string
	// Holder is the holder's stamp, "pid N (PROGRAM) on HOST since T", or
	// [UnknownHolder] when there is none to read.
	Holder string
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("the lock %s is held by %s", e.Path, e.Holder)
}

func (e *HeldError) Unwrap() error { return ErrHeld }

// UnknownHolder is the holder a refusal names when the lock file carries no
// stamp: the window between a peer's lock and its stamp, or a file a release
// left behind. "Somebody, and we cannot say who" is more use than an invented
// pid.
const UnknownHolder = "another crewlet process"

// DirMode and FileMode are what [Try] creates the lock file and its directory
// with: the lock files sit beside a database, a broker's store and a login,
// none of which any other user of the host has any business reading.
const (
	DirMode  os.FileMode = 0o700
	FileMode os.FileMode = 0o600
)

// Lock is one held lock. Release it; the kernel also does when the process
// ends.
type Lock struct {
	// file is the locked descriptor, swapped to nil by the first Release so
	// a second one — from another goroutine too — finds nothing to drop.
	file atomic.Pointer[os.File]
}

// Try takes an exclusive lock on the file at path without waiting, creating
// the file and its directory if they are missing, and stamps the holder into
// it. A lock someone else holds is a [*HeldError] naming them.
//
// NON-BLOCKING, deliberately: every caller is a process that should be told
// "this is held, by whom" rather than hang, and a caller that does want to wait
// polls this with its own bound and its own words for the holder.
func Try(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), DirMode); err != nil {
		return nil, fmt.Errorf("flock: preparing %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, FileMode) //nolint:gosec // a lock file the caller names, beside its own state
	if err != nil {
		return nil, fmt.Errorf("flock: opening %s: %w", path, err)
	}
	held, err := lockExclusive(file)
	switch {
	case err != nil:
		_ = file.Close()
		return nil, fmt.Errorf("flock: locking %s: %w", path, err)
	case !held:
		holder := readHolder(file)
		_ = file.Close()
		return nil, &HeldError{Path: path, Holder: holder}
	}
	stamp(file)
	lock := &Lock{}
	lock.file.Store(file)
	return lock, nil
}

// Release frees the lock. Safe on nil, safe to call twice, and safe from two
// goroutines at once, so every caller can defer it without tracking whether
// it already ran.
//
// UNLOCK BEFORE CLOSE even though closing releases it: on a descriptor this
// process duplicated (a fork, a test harness), closing one copy is not the
// release of the lock. And the file stays where it is — see the package doc.
func (l *Lock) Release() {
	if l == nil {
		return
	}
	file := l.file.Swap(nil)
	if file == nil {
		return
	}
	_ = unlock(file)
	_ = file.Close()
}

// Pin takes an exclusive lock on f's open file description, for a child to
// inherit with it: see the package doc. Pin before the child is started, so
// that every process that ever holds the description holds the pin.
//
// A refusal is an error wrapping [ErrHeld], not an answer: f is a description
// the caller has just opened, and another description of the file already
// holding a lock means the file is not the caller's alone.
func Pin(f *os.File) error {
	held, err := lockExclusive(f)
	switch {
	case err != nil:
		return fmt.Errorf("flock: pinning %s: %w", f.Name(), err)
	case !held:
		return fmt.Errorf("flock: pinning %s: another open description of the file "+
			"holds a lock on it: %w", f.Name(), ErrHeld)
	}
	return nil
}

// Pinned reports whether a description of probe's file still holds a pin:
// whether anything still holds the description [Pin] was taken on.
//
// probe MUST be another open file description of the file than the pinned
// one. On the pinned description itself this would convert the pin and then
// drop it (the package doc says why), and answer false.
//
// Three answers, as every such question here has: true (a holder remains),
// false (none does), or an error, which says the kernel could not be asked
// and is neither. A probe that found the file free lets go of its own shared
// lock before it answers, so that it leaves nothing behind for a later [Pin]
// to be refused by; if that release fails, the answer is the error.
func Pinned(probe *os.File) (bool, error) {
	free, err := lockShared(probe)
	if err != nil {
		return false, fmt.Errorf("flock: asking whether %s is pinned: %w", probe.Name(), err)
	}
	if !free {
		return true, nil
	}
	if err := unlock(probe); err != nil {
		return false, fmt.Errorf("flock: releasing the probe of %s's pin: %w", probe.Name(), err)
	}
	return false, nil
}

// stamp records who holds the lock, best effort.
//
// DIAGNOSTIC ONLY — nothing decides anything from it; the kernel's answer is
// the decision — so a failure to write it must not fail a lock already held.
// And not synced: a reader in another process sees the page cache whether or
// not it has reached the disk, and a holder that crashes before it does has
// released the lock with it, so the stamp's durability buys nothing and an
// fsync per lock would put a disk flush on the shared login's millisecond
// compare-and-swap.
func stamp(file *os.File) {
	if err := file.Truncate(0); err != nil {
		return
	}
	host, _ := os.Hostname()
	_, _ = file.WriteAt([]byte(fmt.Sprintf("pid %d (%s) on %s since %s\n",
		os.Getpid(), command(os.Args), host, time.Now().UTC().Format(time.RFC3339))), 0)
}

// command names the holder's program for its stamp: the binary and the
// subcommands after it — `crewlet run`, `crewlet secrets set` — because a pid
// alone does not say whether the holder is the engine or a command that lets
// go when it finishes, which is what a refused taker most needs to know.
//
// AT MOST TWO WORDS AFTER THE BINARY, AND NONE FROM THE FIRST FLAG ON: a value
// can follow on a command line (`-value`, a name after the subcommand), and a
// stamp is a file, so it carries the command's verbs and never its arguments.
func command(args []string) string {
	if len(args) == 0 {
		return "unknown"
	}
	words := []string{filepath.Base(args[0])}
	for _, arg := range args[1:] {
		if len(words) == 3 || arg == "" || strings.HasPrefix(arg, "-") {
			break
		}
		words = append(words, arg)
	}
	return strings.Join(words, " ")
}

// maxHolderStamp bounds the read of a holder's stamp.
//
// Not a truncation of the stamp — [stamp] writes one line of well under a
// kilobyte — but a bound on a file this process does not fully control: the
// path is operator-supplied, and reading an arbitrary file whole into an error
// message is how a mistyped path becomes a gigabyte in the heap. 8 KiB is an
// order of magnitude past the longest stamp a 253-byte hostname can produce.
const maxHolderStamp = 8 << 10

// readHolder is the holder's stamp, whole.
//
// THE WHOLE STAMP: a hostname may be 253 bytes, and the host is the one field
// that says which machine to go and look at — which is the whole question a
// refused taker is asking.
func readHolder(file *os.File) string {
	raw, err := io.ReadAll(io.NewSectionReader(file, 0, maxHolderStamp))
	if err != nil {
		return UnknownHolder
	}
	if line := strings.TrimSpace(string(raw)); line != "" {
		return line
	}
	return UnknownHolder
}
