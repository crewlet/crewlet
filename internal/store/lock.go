package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Enforcing "one file, one process".
//
// # Why a lock exists at all
//
// The package doc states the rule; this is what makes it true. The driver
// does not support multi-process access to a database file, and — the part
// that turns a rule into an incident — it says so only sometimes. Measured,
// one process holding the file and a second opening it:
//
//	turso   refuses, as "Locking error: File is locked by another process"
//	        wrapped in a connect failure that names no holder — and only
//	        while the first process has live connections
//
// So the defence, where there is one, is a message an operator cannot act on,
// and in the window between the engine's connections there is none at all.
// This lock is taken before any driver work, on every path, and names the
// holder. (The retired fallback driver, mainline SQLite, did not refuse the
// second opener at all — two writers, no error, corruption on the first
// collision. That is what the measurement above was originally a comparison
// against, and it is why the answer is a lock rather than a better error.)
//
// That is not a theoretical shape. `crewlet secrets`, `crewlet llm export
// -secret-store` and every provisioner's secret-store sink all open the
// database the engine is running on, from a second OS process, as their
// documented gesture. Before this lock the only defence was a printed
// warning, which is a defence against an operator who reads it.
//
// # Why an OS lock rather than a pid file
//
// A pid file has to answer "is that process still alive?", and the portable
// answer is worse than the question: os.FindProcess succeeds for any pid on
// Unix, Signal(0) is unsupported on Windows, and pids are reused. A crashed
// engine would leave a lock nobody could safely clear.
//
// An advisory file lock is released by the KERNEL when the holder exits,
// however it exits. A crash frees it; a kill -9 frees it; a container OOM
// frees it. There is no stale state to reap and no liveness check to get
// wrong. The lock is taken on a SIDECAR file rather than the database itself,
// because the driver opens and closes the database on its own schedule and a
// lock on a descriptor it owns is a lock we cannot reason about.
//
// # Cross-PROCESS, not cross-handle
//
// The hazard the package doc names is "a second binary pointed at the same
// path". Two handles inside ONE process are a different thing and are safe:
// the driver serialises its own connections, and two pools on one file are
// the same as one pool with more of them. So the lock is REFCOUNTED per
// process — a second Open here shares the claim rather than being refused —
// and only the last Close releases it. Refusing in-process would break the
// legitimate case (a test proving that mutual exclusion lives in a SQL
// statement rather than in a pool) while catching nothing the real bug does.
//
// # Advisory, and honest about it
//
// A process that never asks does not notice. That is the correct trade here:
// every opener of this database is this binary, the lock is taken inside
// Open, and a foreign tool reading the file is a case no lock in Go would
// have stopped either. What it guarantees is that two crewlet processes
// cannot both hold one database — which is every occurrence of the bug that
// actually exists.

// lockSuffix names the sidecar. Beside the database rather than in a temp
// directory, so a bind mount or a copied data directory carries the lock with
// the file it protects.
const lockSuffix = ".lock"

// ErrLocked reports that another process holds this database.
//
// Its own sentinel so a caller can tell "somebody else has it" from every
// other way an open fails — the CLI turns it into a remediation naming the
// engine, and a test asserts the distinction rather than matching a string.
var ErrLocked = errors.New("store: the database is open in another process")

// fileLock is one process's claim on one database path.
//
// Shared by every handle this process has open on that path, and freed when
// the last of them closes — see the refcount note above.
type fileLock struct {
	path string
	file *os.File

	// holds is how many live handles share this claim. Guarded by
	// locksHeld's mutex, never by a field of its own: the count and the
	// map entry have to move together, or a release racing an open would
	// drop a lock the opener is about to depend on.
	holds int
}

// locksHeld is this process's claims, one per database path.
var locksHeld = struct {
	mu sync.Mutex
	by map[string]*fileLock
}{by: map[string]*fileLock{}}

// lockStore takes the exclusive lock for a database path.
//
// The holder's identity is written into the sidecar AFTER the lock is held,
// so what a refused opener reads is always a live holder's — never a
// half-written line from a racing one.
func lockStore(dbPath string) (*fileLock, error) {
	if dbPath == "" || strings.HasPrefix(dbPath, ":memory:") {
		// An in-memory database is per-connection by construction: there
		// is no file for a second process to find, so there is nothing to
		// exclude and nothing to write a sidecar beside.
		return nil, nil
	}
	locksHeld.mu.Lock()
	defer locksHeld.mu.Unlock()
	if held := locksHeld.by[dbPath]; held != nil {
		// Already ours. A second handle in this process shares the claim.
		held.holds++
		return held, nil
	}

	path := dbPath + lockSuffix
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: preparing %s: %w", dir, err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: opening the lock beside %s: %w", dbPath, err)
	}
	held, err := tryLock(file)
	switch {
	case err != nil:
		_ = file.Close()
		return nil, fmt.Errorf("store: locking %s: %w", dbPath, err)
	case !held:
		holder := readHolder(file)
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s is held by %s. Stop it before running "+
			"this — the driver does not support two processes on one file, "+
			"and does not reliably refuse the second one either",
			ErrLocked, dbPath, holder)
	}
	stamp(file)
	lock := &fileLock{path: path, file: file, holds: 1}
	locksHeld.by[dbPath] = lock
	return lock, nil
}

// release drops this handle's share of the claim, freeing the lock when it
// was the last. Safe on a nil lock, which is what an in-memory database gets,
// so every caller can defer it unconditionally.
func (l *fileLock) release() {
	if l == nil || l.file == nil {
		return
	}
	locksHeld.mu.Lock()
	defer locksHeld.mu.Unlock()
	if l.holds--; l.holds > 0 {
		return
	}
	for path, held := range locksHeld.by {
		if held == l {
			delete(locksHeld.by, path)
			break
		}
	}
	// UNLOCK BEFORE CLOSE even though closing releases it: on a descriptor
	// this process duplicated (a fork, a test harness), the close of one
	// copy is not the release of the lock.
	_ = unlock(l.file)
	_ = l.file.Close()
	// The sidecar is deliberately NOT removed. Unlinking it races a peer
	// that has already opened it and is about to lock: that peer would
	// take a lock on an unlinked inode and a third process would create a
	// new file and lock that, so both would believe they hold it. An empty
	// file is cheap; a lock that excludes nobody is not.
}

// stamp records who holds the lock, best effort.
//
// DIAGNOSTIC ONLY — nothing reads it to make a decision, the kernel's answer
// is the decision — so a failure to write it must not fail an open that has
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

// readHolder describes the process holding the lock, for the error message.
func readHolder(file *os.File) string {
	buf := make([]byte, 256)
	n, _ := file.ReadAt(buf, 0)
	if line := strings.TrimSpace(string(buf[:n])); line != "" {
		return line
	}
	// An empty sidecar is the window between a peer's lock and its stamp,
	// or a release that left the file behind. Both mean "somebody, and we
	// cannot say who" — which is more useful than an invented pid.
	return "another crewlet process"
}

// pidOf reads the holder's pid from a stamp, for tests and diagnostics.
func pidOf(stamped string) (int, bool) {
	fields := strings.Fields(stamped)
	if len(fields) < 2 || fields[0] != "pid" {
		return 0, false
	}
	pid, err := strconv.Atoi(fields[1])
	return pid, err == nil
}
