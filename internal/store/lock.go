package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/flock"
)

// Enforcing "one file, one process".
//
// The package doc states the rule; this makes it true. The driver does not
// support multi-process access to a database file and says so only sometimes:
// turso refuses with "Locking error: File is locked by another process", wrapped
// in a connect failure that names no holder, and only while the first process
// has live connections. This lock is taken before any driver work, on every
// path, and names the holder. `crewlet config`, `crewlet migrate` and
// `crewlet search eval` open a node's database from a second process.
//
// The lock is [flock.Try]'s, on a SIDECAR file: the driver opens and closes the
// database on its own schedule, so a lock on its descriptor can't be reasoned about.

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
// the last of them closes. It is refcounted per process because two handles in
// ONE process are safe (the driver serialises its connections); only another
// process is the hazard. Refusing in-process would break legitimate cases.
type fileLock struct {
	// dbPath is the key this claim is filed under in locksHeld, so the
	// last release finds its entry without walking the map.
	dbPath string
	held   *flock.Lock

	// holds is how many live handles share this claim. Guarded by
	// locksHeld's mutex, never by a field of its own: the count and the
	// map entry have to move together, or a release racing an open would
	// drop a lock the opener is about to depend on.
	holds int

	// discarding marks the claim a scratch discard holds while it deletes
	// the file ([discard]), which no handle may share. Guarded by
	// locksHeld's mutex, like holds.
	discarding bool

	// writes orders the write transactions every one of those handles
	// runs on this file. Here rather than on the handle for the same
	// reason the claim itself is: two handles on one file share one
	// driver-level write lock, so they have to share one line for it.
	// See writequeue.go.
	writes writeQueue

	// migrating is held for the whole of a migration run on this file,
	// by whichever of those handles is running it. Here for the reason
	// writes is: the handles that can race one file's migration are
	// exactly the handles that share this claim. See [DB.migrate].
	migrating sync.Mutex
}

// queue is the write queue a handle on this path takes its place in.
//
// A nil claim is an IN-MEMORY database, which has no file and no peer handle
// to share anything with, so it gets a queue of its own — one per handle,
// which is exactly the scope of the lock it is ordering.
func (l *fileLock) queue() *writeQueue {
	if l == nil {
		return &writeQueue{}
	}
	return &l.writes
}

// migrations is the lock a migration run on this path holds.
//
// A nil claim is an in-memory database, which no other handle can reach, so it
// gets a lock of its own for the reason [fileLock.queue] gives a queue of its
// own: that is exactly the scope of what it excludes.
func (l *fileLock) migrations() *sync.Mutex {
	if l == nil {
		return &sync.Mutex{}
	}
	return &l.migrating
}

// locksHeld is this process's claims, one per database path.
var locksHeld = struct {
	mu sync.Mutex
	by map[string]*fileLock
}{by: map[string]*fileLock{}}

// lockStore takes the exclusive lock for a database path, sharing a claim
// this process already holds on it.
func lockStore(dbPath string) (*fileLock, error) {
	return claimStore(dbPath, false)
}

// claimStore takes the exclusive lock for a database path. A claim this
// process already holds is shared — unless either side is a scratch discard
// ([discard]), which takes the path ALONE: it is refused while any handle here
// holds the path, and any handle asking while it holds the path is refused in
// turn.
//
// Who holds it is [flock.Try]'s to stamp into the sidecar, and what a refused
// opener's error names.
func claimStore(dbPath string, discarding bool) (*fileLock, error) {
	if dbPath == "" || strings.HasPrefix(dbPath, ":memory:") {
		// An in-memory database is per-connection by construction: there
		// is no file for a second process to find, so there is nothing to
		// exclude and nothing to write a sidecar beside.
		return nil, nil
	}
	locksHeld.mu.Lock()
	defer locksHeld.mu.Unlock()
	if held := locksHeld.by[dbPath]; held != nil {
		switch {
		case discarding:
			return nil, fmt.Errorf("store: %s is open in this process, and a scratch "+
				"store is discarded before it is opened — never under a live handle", dbPath)
		case held.discarding:
			return nil, fmt.Errorf("store: %s is being discarded as a scratch store "+
				"by this process; open it once that open has returned", dbPath)
		}
		// Already ours. A second handle in this process shares the claim.
		held.holds++
		return held, nil
	}

	path := dbPath + lockSuffix
	held, err := flock.Try(path)
	var other *flock.HeldError
	switch {
	case errors.As(err, &other):
		return nil, fmt.Errorf("%w: %s is held by %s. Stop it before running "+
			"this — the driver does not support two processes on one file, "+
			"and does not reliably refuse the second one either",
			ErrLocked, dbPath, other.Holder)
	case err != nil:
		return nil, fmt.Errorf("store: locking %s: %w", dbPath, err)
	}
	lock := &fileLock{dbPath: dbPath, held: held, holds: 1, discarding: discarding}
	locksHeld.by[dbPath] = lock
	return lock, nil
}

// Probe reports whether another process holds the node database at dbPath —
// its running engine, or an offline command — as [ErrLocked] naming the holder,
// WITHOUT OPENING THE FILE AND WITHOUT KEEPING THE CLAIM. A claim this process
// holds already is shared, and answers nil.
//
// Opening would migrate the file, or create one on a node that never booted;
// KEEPING the claim would make a second command read the first as a running
// engine and refuse the engine over a database nobody had open. Not for a
// scratch store, which the engine deletes before it opens.
func Probe(dbPath string) error {
	lock, err := lockStore(dbPath)
	if err != nil {
		return err
	}
	lock.release()
	return nil
}

// Hold takes this process's claim on the node database at dbPath and KEEPS
// it, answering the release that gives it back — [ErrLocked] naming the holder
// where another process has it, as an open is refused.
//
// For a caller excluding another PROCESS while it holds no handle: `crewlet run`
// opens the file for the engine only once the broker is up, and in that gap an
// offline `crewlet config import` would replace the row the boot decided on.
// The claim is refcounted ([claimStore]), so the engine's open shares it; the
// release is idempotent. Not for a SCRATCH store: [discard] is refused here.
func Hold(dbPath string) (func(), error) {
	lock, err := lockStore(dbPath)
	if err != nil {
		return nil, err
	}
	return sync.OnceFunc(lock.release), nil
}

// release drops this handle's share of the claim, freeing the lock when it
// was the last. Safe on a nil lock, which is what an in-memory database gets,
// so every caller can defer it unconditionally.
func (l *fileLock) release() {
	if l == nil || l.held == nil {
		return
	}
	locksHeld.mu.Lock()
	defer locksHeld.mu.Unlock()
	if l.holds--; l.holds > 0 {
		return
	}
	// The entry is this claim — each handle releases its share once, its
	// Close being guarded — but compared rather than assumed, so a release
	// that ever ran twice could not drop a newer claim filed under the path.
	if locksHeld.by[l.dbPath] == l {
		delete(locksHeld.by, l.dbPath)
	}
	// The sidecar stays where it is — see [flock]'s package doc.
	l.held.Release()
}

// discard deletes a database and every sidecar beside it, holding the store's
// own lock while it does — see [Options.Scratch].
//
// THE LOCK MAKES A DELETE SAFE: a scratch node pointed at a path another engine
// runs on is refused naming the holder. A handle THIS process holds is refused
// too, since the in-process claim is shared and the lock alone would let a
// scratch open delete a database a caller here is reading.
//
// THE CHECK AND THE CLAIM ARE ONE STEP, never shared ([claimStore]): as two, an
// open landing between them lost its database, and one landing during the
// delete opened a file being removed. remove is a PARAMETER so a case can hold
// a discard between claim and release and see an open refused there.
func discard(dbPath string, remove func(dbPath string) error) error {
	if dbPath == "" || strings.HasPrefix(dbPath, ":memory:") {
		// Nothing on disk: an in-memory database starts empty by
		// construction.
		return nil
	}
	lock, err := claimStore(dbPath, true)
	if err != nil {
		return err
	}
	defer lock.release()
	if err := remove(dbPath); err != nil {
		return fmt.Errorf("store: discard the scratch store: %w", err)
	}
	return nil
}
