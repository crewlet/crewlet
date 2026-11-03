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
// That is not a theoretical shape. `crewlet config`, `crewlet migrate` and
// `crewlet search eval` open a node's database from a second OS process as
// their documented gesture, and every command that reaches the company's
// secrets asks this lock ([Probe]) whether the engine runs. Before this lock
// the only defence was a printed warning, which is a defence against an
// operator who reads it.
//
// # An OS lock, on a sidecar
//
// The lock itself is [flock.Try]'s — an advisory lock the KERNEL releases when
// its holder exits, however it exits, so a crashed engine leaves nothing to
// reap; that package says why that beats a pid file, why flock rather than
// fcntl, why the sidecar is never removed and why a lock that is only advisory
// is still the right one. It is taken on a SIDECAR file rather than the
// database itself, because the driver opens and closes the database on its own
// schedule and a lock on a descriptor it owns is a lock we cannot reason
// about. What this file adds on top is what only the store needs: the
// refcount below, and the discard.
//
// # Cross-PROCESS, not cross-handle
//
// The hazard the package doc names is "a second binary pointed at the same
// path". Two handles inside ONE process are a different thing and are safe:
// the driver serialises its own connections, and two pools on one file are
// the same as one pool with more of them. So the lock is REFCOUNTED per
// process — a second Open here shares the claim rather than being refused —
// and only the last Close releases it. That sharing is this file's, on top of
// one [flock.Lock]: the OS lock itself excludes a second taker in this process
// as firmly as one in another. Refusing in-process would break the
// legitimate case (a test proving that mutual exclusion lives in a SQL
// statement rather than in a pool) while catching nothing the real bug does.

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
// its running engine, or an offline command working on it — as [ErrLocked]
// naming the holder, WITHOUT OPENING THE FILE AND WITHOUT KEEPING THE CLAIM.
// A claim this process holds already is shared for the moment, as a second
// open's is, and answers nil.
//
// For a process that does not use the database and needs to know whether the
// engine runs: a `crewlet` command deciding how to reach the fleet's store.
// Opening the file to learn it would migrate a database the command never
// reads, and on a node that has never booted create one. And KEEPING the
// claim would lock a file the command does not use: a second command would
// then read the first as a running engine, and the engine would be refused
// over a database nobody had open. What a command does use — a broker on the
// stream's store directory — has a lock of its own, which is what keeps an
// engine from starting under it.
//
// Not for a scratch store, which the engine deletes before it opens: a node
// that holds no data has nothing here to ask about.
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
// For a caller that has to exclude another PROCESS across a stretch in which
// it holds no handle of its own. `crewlet run` is the one: it decides what the
// engine boots on by opening this file, reading it and closing it again, and
// opens it for the engine only once the broker is up — a gap of seconds, in
// which an offline `crewlet config import` would replace the very row the boot
// has already decided on. The claim is REFCOUNTED per process ([claimStore]),
// so the engine's own open shares this one and the boot's release leaves the
// engine holding the file with nothing in between.
//
// The release is idempotent, so a caller gives the claim back where it knows
// it is done and defers it for every path that does not.
//
// Not for a SCRATCH store, which the engine deletes as it opens it: a discard
// takes the path alone ([discard]) and is refused while a claim here stands.
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
// THE LOCK IS WHAT MAKES A DELETE SAFE TO ASK FOR. Without it, a node told its
// store is scratch and pointed by mistake at a path another engine is running
// on would unlink that engine's database from under it. With it, that node is
// refused naming the holder, exactly as a second opener is. A handle THIS
// process already holds is refused too: the in-process claim is shared rather
// than exclusive, so the lock alone would let a scratch open delete a
// database a caller here is reading.
//
// THE CHECK AND THE CLAIM ARE ONE STEP, and the claim is never shared
// ([claimStore]). They were two — a look for a handle here, then an ordinary
// shared claim — so an open landing between them shared the claim and had its
// database deleted under it, and one landing during the delete opened a file
// that was being removed.
//
// remove is what deletes the files: [removeDatabaseFiles], on the one path
// that discards. A PARAMETER because the claim's exclusivity is a property of
// the moment between the claim and the release, and only a case that can hold
// a discard inside that moment can see whether an open is refused there —
// [claimStore]'s own case could not tell this function taking the claim from
// it looking first and then sharing one.
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
