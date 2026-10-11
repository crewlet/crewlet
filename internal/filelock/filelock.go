// Package filelock is ONE PROCESS'S EXCLUSIVE CLAIM ON A PATH: an advisory
// flock on a sidecar file, taken without blocking, stamped with who holds it,
// and released by the KERNEL when the holder exits — however it exits. A crash
// frees it, a kill -9 frees it, a container OOM frees it; there is no stale
// state to reap and no liveness check to get wrong, which is why it is an OS
// lock rather than a pid file (a pid file has to answer "is that process still
// alive?", the portable answer is worse than the question — os.FindProcess
// succeeds for any pid on Unix — and pids are reused).
//
// A path exactly one process may own at a time takes one. The store's
// database file is that (internal/store, which adds the per-process refcount
// and the scratch discard on top). One implementation, because the four
// decisions in it — flock rather than fcntl, non-blocking, unlock before
// close, never unlink the sidecar — are each a way a hand-rolled copy goes
// quietly wrong (ADR-0008):
//
//   - FLOCK, NOT FCNTL: an fcntl lock is dropped when ANY descriptor for the
//     file is closed in this process, so an unrelated open and close of the
//     sidecar would release a lock the holder believes it has.
//   - NON-BLOCKING: a blocked claim reads as a wedged command; the honest
//     answer is "this is held, by whom", at once.
//   - UNLOCK BEFORE CLOSE: on a descriptor this process duplicated (a fork, a
//     test harness), closing one copy is not the release of the lock.
//   - THE SIDECAR IS NEVER REMOVED: unlinking it races a peer that has opened
//     it and is about to lock — that peer would lock an unlinked inode while a
//     third process created and locked a new file, and both would believe they
//     hold the path. An empty file is cheap; a lock that excludes nobody is not.
//
// The claim is on a SIDECAR rather than on the thing it protects, because the
// owner of that thing — a database driver, a git process — opens and closes it
// on its own schedule, and a lock on a descriptor somebody else owns is a lock
// nobody can reason about.
//
// flock is per OPEN FILE DESCRIPTION, so two claims of one path in one process
// exclude each other exactly as two processes do. Sharing a claim inside a
// process is a decision for the caller, made above this package — the store's
// refcount is that.
//
// ADVISORY, and honest about it: a process that never asks does not notice.
// Every claimant of these paths is this binary, so what it guarantees is that
// two crewlet processes cannot both hold one — every occurrence of the bug
// that exists.
//
// The stamp ("pid N on HOST since T") is written AFTER the lock is held, so a
// refused claimant always reads a live holder's line, never a racing one's
// half; it is diagnostic only — the kernel's answer is the decision.
package filelock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ErrHeld is a claim refused because another holder has the path.
//
// Its own sentinel so a caller can tell "somebody else has it" from every
// other way a claim fails — a directory that cannot hold the sidecar, a
// filesystem that cannot lock — and answer each in its own words.
var ErrHeld = errors.New("filelock: held by another process")

// HeldError is that refusal, naming the path and the holder's stamp.
type HeldError struct {
	// Path is the sidecar.
	Path string
	// Holder is the holder's stamp, or "another crewlet process" when it is
	// empty or unreadable.
	Holder string
}

// Error names the sidecar and who holds it.
func (e *HeldError) Error() string {
	return "filelock: " + e.Path + " is held by " + e.Holder
}

// Is makes a HeldError match [ErrHeld].
func (e *HeldError) Is(target error) bool { return target == ErrHeld }

// Claim is one held claim.
type Claim struct {
	// file is the locked descriptor, swapped to nil by the first Release
	// so a second one — from another goroutine too — finds nothing to drop.
	file atomic.Pointer[os.File]
}

// Take claims path for this process: it opens the sidecar at path —
// creating it 0600 if it is missing; its directory must exist — and takes an
// exclusive, non-blocking flock on it. A path held elsewhere answers a
// *[HeldError] (errors.Is [ErrHeld]) naming the holder. Any other failure is
// returned wrapped, naming the path.
func Take(path string) (*Claim, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filelock: opening %s: %w", path, err)
	}
	held, err := tryLock(file)
	switch {
	case err != nil:
		_ = file.Close()
		return nil, fmt.Errorf("filelock: locking %s: %w", path, err)
	case !held:
		holder := readHolder(file)
		_ = file.Close()
		return nil, &HeldError{Path: path, Holder: holder}
	}
	stamp(file)
	claim := &Claim{}
	claim.file.Store(file)
	return claim, nil
}

// Release drops the claim: unlock, then close; the sidecar stays (see the
// package doc for both). Safe on a nil claim and idempotent, so every caller
// can defer it unconditionally.
func (c *Claim) Release() {
	if c == nil {
		return
	}
	file := c.file.Swap(nil)
	if file == nil {
		return
	}
	_ = unlock(file)
	_ = file.Close()
}

// stamp records who holds the lock, best effort.
//
// DIAGNOSTIC ONLY — nothing reads it to make a decision, the kernel's answer
// is the decision — so a failure to write it must not fail a claim that has
// already succeeded.
func stamp(file *os.File) {
	if err := file.Truncate(0); err != nil {
		return
	}
	host, _ := os.Hostname()
	_, _ = file.WriteAt([]byte(fmt.Sprintf("pid %d on %s since %s\n",
		os.Getpid(), host, time.Now().UTC().Format(time.RFC3339))), 0)
	_ = file.Sync()
}

// maxHolderStamp bounds the sidecar read.
//
// Not a truncation of the stamp — stamp() writes one line of well under a
// kilobyte — but a bound on a file this process does not fully control: the
// path is operator-supplied, and reading an arbitrary file whole into an error
// message is how a mistyped path becomes a gigabyte in the heap. 8 KiB is an
// order of magnitude past the longest stamp a 253-byte hostname can produce.
const maxHolderStamp = 8 << 10

// readHolder describes the process holding the lock, for the refusal.
//
// THE WHOLE STAMP. It used to be read into a fixed 256-byte buffer, and
// stamp() writes "pid N on HOST since TIMESTAMP" — a hostname may be up to 253
// bytes, so the one diagnostic an operator gets for a held path could lose the
// host, which is the single field telling them WHICH machine to go and look
// at. The file is one line a holder wrote; reading it whole costs nothing.
func readHolder(file *os.File) string {
	raw, err := io.ReadAll(io.NewSectionReader(file, 0, maxHolderStamp))
	if err != nil {
		return "another crewlet process"
	}
	if line := strings.TrimSpace(string(raw)); line != "" {
		return line
	}
	// An empty sidecar is the window between a peer's lock and its stamp,
	// or a release that left the file behind. Both mean "somebody, and we
	// cannot say who" — which is more useful than an invented pid.
	return "another crewlet process"
}

// PID reads the holder's pid from a stamp, for tests and diagnostics.
func PID(stamp string) (int, bool) {
	fields := strings.Fields(stamp)
	if len(fields) < 2 || fields[0] != "pid" {
		return 0, false
	}
	pid, err := strconv.Atoi(fields[1])
	return pid, err == nil
}
