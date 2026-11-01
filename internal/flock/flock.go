// Package flock is the one exclusive cross-process lock this engine takes on a
// file: the node database's ([github.com/crewlet/crewlet/internal/store]), the
// embedded broker's stream directory
// ([github.com/crewlet/crewlet/internal/queue/jetstream]) and a harness's
// shared login ([github.com/crewlet/crewlet/internal/hostbox]).
//
// # One implementation, because the subtleties are not obvious
//
// Each of those once carried its own copy of the syscall, and each copy had to
// rediscover the same three things on its own:
//
//   - flock, never fcntl. An fcntl lock is dropped when ANY descriptor for the
//     file is closed in the process, so an unrelated open and close of the lock
//     file would silently free a lock its holder still believes it has.
//   - Never unlink the lock file. A peer that has opened it and is about to
//     lock would take a lock on an unlinked inode while a third process created
//     a fresh file and locked that, and both would believe they held it. An
//     empty file left behind costs nothing; a lock that excludes nobody does.
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
	file *os.File
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
	return &Lock{file: file}, nil
}

// Release frees the lock. Safe on nil, and safe to call twice.
//
// UNLOCK BEFORE CLOSE even though closing releases it: on a descriptor this
// process duplicated (a fork, a test harness), closing one copy is not the
// release of the lock. And the file stays where it is — see the package doc.
func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = unlock(l.file)
	_ = l.file.Close()
	l.file = nil
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
