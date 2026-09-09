package store

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// WHAT THESE CASES PROTECT: Pending is a second way into a connection, and it
// had drifted from the first.
//
// [Pending] answers "what would `crewlet migrate` apply?" without applying it,
// which means it opens a pool and reads. It used to do that with its own copy
// of Open's preamble — resolve, build a pool, ping — and the copy was missing
// two things Open does and one it does not:
//
//   - it never called prepareTursoLibrary, so the FIRST connection the
//     diagnostic command made went straight into the driver's own loader,
//     whose answer to a half-written cache is a panic inside a sync.Once. The
//     command that exists to report the schema safely was the one command
//     that could take the process down on the exact failure the preparation
//     was written for. See turso.go.
//   - it never bounded the pool, so `crewlet migrate` ran on whatever
//     database/sql felt like opening rather than on the four connections
//     every other opener gets.
//   - it took no file lock, so it read the database of a LIVE engine and only
//     refused at the point it tried to change it. The check that runs first
//     was the one with no guard.
//
// All three are the same defect — a second code path — so the fix is one
// path (openPrepared) and these cases are what hold it there.

// pendingChildEnv marks the child half of the panic case, so it is inert in an
// ordinary run.
const pendingChildEnv = "CREWLET_TEST_PENDING_CHILD"

// PENDING REPORTS A BROKEN LIBRARY CACHE INSTEAD OF TAKING THE PROCESS DOWN.
//
// The child process is load-bearing: the preparation is memoised per process,
// so this has to be the first thing that process touches. In the parent, some
// other case has already prepared the library successfully and a panic here
// would prove nothing.
func TestPendingReportsABrokenLibraryCacheInsteadOfPanicking(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], //nolint:gosec // os.Args[0] is this test binary
		"-test.run=^TestPendingWithABrokenLibraryCacheInAChildProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), pendingChildEnv+"=1", tursoCacheEnv+"="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Pending did not report the broken cache: %v\n%s", err, out)
	}
}

// TestPendingWithABrokenLibraryCacheInAChildProcess is the child half.
func TestPendingWithABrokenLibraryCacheInAChildProcess(t *testing.T) {
	if os.Getenv(pendingChildEnv) == "" {
		t.Skip("not a child process; see TestPendingReportsABrokenLibraryCacheInsteadOfPanicking")
	}
	_, err := Pending(t.Context(), filepath.Join(t.TempDir(), "state.db"), Options{})
	if err == nil {
		t.Fatal("Pending succeeded against a library cache that cannot exist")
	}
	if !strings.Contains(err.Error(), os.Getenv(tursoCacheEnv)) {
		t.Fatalf("error = %v; it must name the cache an operator has to look at", err)
	}
}

// PENDING REFUSES A DATABASE ANOTHER PROCESS HOLDS, with the same sentinel
// Open answers with — so `crewlet migrate -check` against a live engine says
// who has the file, rather than reading it or failing with a driver message
// that names nobody.
func TestPendingRefusesADatabaseAnotherProcessHolds(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "held.db")

	// A peer is simulated by taking the lock from this process and then
	// hiding it from the refcount, which is what a different process is:
	// the claim exists on the file and this caller does not share it.
	lock, err := lockStore(path)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	defer lock.release()
	locksHeld.mu.Lock()
	delete(locksHeld.by, path)
	locksHeld.mu.Unlock()
	defer func() {
		locksHeld.mu.Lock()
		locksHeld.by[path] = lock
		locksHeld.mu.Unlock()
	}()

	_, err = Pending(t.Context(), path, Options{})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("Pending on a held database = %v, want ErrLocked — reading a "+
			"file a live engine is writing is the case the lock exists for", err)
	}
}

// AND IT GIVES THE LOCK BACK.
//
// Asserted against the REFCOUNT rather than against a following Open, and
// that distinction is the whole test. The claim is shared per process, so a
// Pending that never released would still let the Open in `crewlet migrate`
// through — the leak is invisible from the outside and only bites a LATER
// process, after this one has already exited and hidden the evidence. So what
// is checked is that the claim on this path is gone: no entry, not merely a
// second caller getting past it.
func TestPendingReleasesTheLockForTheMigrationThatFollows(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "seq.db")

	if schemas, err := Pending(t.Context(), path, Options{}); err != nil {
		t.Fatalf("Pending: %v", err)
	} else if len(estateOf(t, schemas, EstateNode).Pending) == 0 {
		t.Fatal("a fresh database reported nothing pending")
	}

	locksHeld.mu.Lock()
	held := locksHeld.by[path]
	locksHeld.mu.Unlock()
	if held != nil {
		t.Fatalf("Pending kept its claim on %s (%d holder(s)) — the process "+
			"never gives the file back, and the next one to want it is refused "+
			"by a lock nothing is using", path, held.holds)
	}

	db, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("Open after Pending: %v", err)
	}
	_ = db.Close()
}

// PENDING AND OPEN AGREE. Pending's whole contract is that it predicts what
// Open would do, so the two answers are compared rather than each asserted
// against a hand-written list that could drift from both.
func TestPendingPredictsWhatOpenApplies(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agree.db")

	schemas, err := Pending(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	before := estateOf(t, schemas, EstateNode)
	if len(before.Applied) != 0 {
		t.Errorf("a fresh database reports %d applied, want none", len(before.Applied))
	}

	db, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	now, err := db.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatalf("AppliedMigrations: %v", err)
	}
	if len(now) != len(before.Pending) {
		t.Fatalf("Open applied %d migrations, Pending predicted %d", len(now), len(before.Pending))
	}

	// And afterwards there is nothing left to do, which is the answer the
	// deploy gate exits 0 on.
	_ = db.Close()
	schemas, err = Pending(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("second Pending: %v", err)
	}
	after := estateOf(t, schemas, EstateNode)
	if len(after.Pending) != 0 || len(after.Applied) != len(now) {
		t.Fatalf("after migrating: %d applied, %d pending",
			len(after.Applied), len(after.Pending))
	}
}

// estateOf picks one estate's report out of Pending's answer.
func estateOf(t *testing.T, schemas []Schema, want Estate) Schema {
	t.Helper()
	for _, s := range schemas {
		if s.Estate == want {
			return s
		}
	}
	t.Fatalf("Pending reported no %s estate: %+v", want, schemas)
	return Schema{}
}

// THE POOL BOUNDS REACH EVERY OPENER. Open used to apply them and Pending did
// not; asserting the shared helper is what keeps that from happening again,
// because it is the one place either of them can get a connection.
func TestOpenPreparedAppliesThePoolBounds(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bounds.db")

	pool, err := openPrepared(t.Context(), path, Options{MaxOpenConns: 3})
	if err != nil {
		t.Fatalf("openPrepared: %v", err)
	}
	defer func() { _ = pool.Close() }()
	if got := pool.Stats().MaxOpenConnections; got != 3 {
		t.Errorf("max open conns = %d, want the requested 3", got)
	}

	unset, err := openPrepared(t.Context(), filepath.Join(t.TempDir(), "d.db"), Options{})
	if err != nil {
		t.Fatalf("openPrepared: %v", err)
	}
	defer func() { _ = unset.Close() }()
	if got := unset.Stats().MaxOpenConnections; got != defaultReaderConns {
		t.Errorf("max open conns = %d, want the store default %d — an unset "+
			"bound must reach the pool as \"you choose\", not as unbounded",
			got, defaultReaderConns)
	}

	// AND A DECLARED PIN WIDENS THE ESTATE THAT HOLDS IT, rather than being
	// taken out of the readers' share. The whole reason PinnedWriters
	// exists is that a pin counts against MaxOpenConns like any other
	// connection, so a handle running three statelog domains on a fixed
	// four leaves one connection for every reader on the node.
	//
	// AND ONLY THAT ESTATE. An applier writes to the replicated estate, so
	// widening the node estate for its pins would be headroom nothing ever
	// takes on the file that is not being written.
	pinned, err := openPrepared(t.Context(), filepath.Join(t.TempDir(), "p.db"),
		Options{PinnedWriters: 3}.forEstate(EstateReplicated))
	if err != nil {
		t.Fatalf("openPrepared: %v", err)
	}
	defer func() { _ = pinned.Close() }()
	if got, want := pinned.Stats().MaxOpenConnections, defaultReaderConns+3; got != want {
		t.Errorf("max open conns with 3 pinned writers = %d, want %d: a pin "+
			"has to be ADDED to the readers' bound, not carved out of it",
			got, want)
	}
	node, err := openPrepared(t.Context(), filepath.Join(t.TempDir(), "n.db"),
		Options{PinnedWriters: 3}.forEstate(EstateNode))
	if err != nil {
		t.Fatalf("openPrepared: %v", err)
	}
	defer func() { _ = node.Close() }()
	if got := node.Stats().MaxOpenConnections; got != defaultReaderConns {
		t.Errorf("the node estate widened to %d for pins it never holds, want %d",
			got, defaultReaderConns)
	}
	var busyMS int
	if err := unset.QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&busyMS); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyMS != int(defaultBusyTimeout.Milliseconds()) {
		t.Errorf("busy_timeout = %dms, want the store default %dms",
			busyMS, defaultBusyTimeout.Milliseconds())
	}
}
