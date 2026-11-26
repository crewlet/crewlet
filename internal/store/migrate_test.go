package store

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/nativelib"
)

// WHAT THESE CASES PROTECT: [Pending] shares Open's preamble (openPrepared), as
// a second copy would miss prepareTursoLibrary (the driver's own loader panics
// inside a sync.Once on a half-written cache, turso.go), the pool bound every
// other opener gets, and the file lock, so a LIVE engine's database would be
// read and refused only when a change was tried.

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
	cmd.Env = childEnv(pendingChildEnv+"=1", nativelib.CacheEnv+"="+root)
	out, err := cmd.CombinedOutput()
	requireChildRan(t, "Pending did not report the broken cache", out, err)
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
	if !strings.Contains(err.Error(), os.Getenv(nativelib.CacheEnv)) {
		t.Fatalf("error = %v; it must name the cache an operator has to look at", err)
	}
	fmt.Println(childRan)
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

	db, err := OpenNode(t.Context(), path, Options{})
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

	db, err := OpenNode(t.Context(), path, Options{})
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
	// A CONFIGURED BOUND IS THE READERS', and the connection kept for
	// identity work is added beside them rather than taken out of them.
	if got, want := pool.Stats().MaxOpenConnections, 3+identityReserve; got != want {
		t.Errorf("max open conns = %d, want the requested 3 readers and the "+
			"identity reserve, %d", got, want)
	}

	unset, err := openPrepared(t.Context(), filepath.Join(t.TempDir(), "d.db"), Options{})
	if err != nil {
		t.Fatalf("openPrepared: %v", err)
	}
	defer func() { _ = unset.Close() }()
	// The derived default: the readers' budget PLUS the connection kept
	// for identity work. Both terms, because a pool sized for the readers
	// alone is a pool whose reserve is the first thing a read burst takes.
	wantUnset := defaultReaderConns() + identityReserve
	if got := unset.Stats().MaxOpenConnections; got != wantUnset {
		t.Errorf("max open conns = %d, want the store default %d — an unset "+
			"bound must reach the pool as \"you choose\", not as unbounded",
			got, wantUnset)
	}

	// AND THE REPLICATED ESTATE'S PINS WIDEN THE FILE THAT HOLDS THEM,
	// rather than being taken out of the readers' share: a pin counts
	// against the pool like any other connection, so an estate carrying
	// three logs on a fixed four would leave one connection for every
	// reader of it.
	//
	// AND ONLY THAT FILE. An applier writes the replicated estate, so
	// widening the node's own file for the pins would be headroom nothing
	// ever takes on the file that is not being written.
	dir := t.TempDir()
	node, err := OpenNode(t.Context(), filepath.Join(dir, "n.db"), Options{})
	if err != nil {
		t.Fatalf("open the node: %v", err)
	}
	defer func() { _ = node.Close() }()
	replicated, err := node.OpenReplicated(t.Context(), 3)
	if err != nil {
		t.Fatalf("open the replicated estate: %v", err)
	}
	if got, want := replicated.sql.Stats().MaxOpenConnections, wantUnset+3; got != want {
		t.Errorf("max open conns with 3 pinned writers = %d, want %d: a pin "+
			"has to be ADDED to the readers' bound, not carved out of it",
			got, want)
	}
	if got := node.sql.Stats().MaxOpenConnections; got != wantUnset {
		t.Errorf("the node estate widened to %d for pins it never holds, want %d",
			got, wantUnset)
	}

	// A CONFIGURED BOUND IS THE READERS' TOO, with the reserve and the
	// pins added on top and never fewer than two readers. Bounded at the
	// configured figure itself, a pool of three with three loops pinned
	// would leave no reader at all — and one at or below the pin count
	// leaves the last loop waiting for a connection none of the others
	// gives back.
	for _, c := range []struct{ conns, want int }{
		{conns: 3, want: 3 + identityReserve + 3},
		{conns: 1, want: replicatedReadFloor + identityReserve + 3},
	} {
		configured, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "c.db"),
			Options{MaxOpenConns: c.conns})
		if err != nil {
			t.Fatalf("open a node bounded at %d: %v", c.conns, err)
		}
		defer func() { _ = configured.Close() }()
		estate, err := configured.OpenReplicated(t.Context(), 3)
		if err != nil {
			t.Fatalf("open the replicated estate of a node bounded at %d: %v", c.conns, err)
		}
		if got := estate.sql.Stats().MaxOpenConnections; got != c.want {
			t.Errorf("a node bounded at %d opened its replicated estate with %d "+
				"connections for 3 pinned writers, want %d", c.conns, got, c.want)
		}
		if got, want := configured.sql.Stats().MaxOpenConnections,
			c.conns+identityReserve; got != want {
			t.Errorf("a node bounded at %d opened its own file with %d, want %d",
				c.conns, got, want)
		}
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

// A MIGRATION'S NUMBER IS ITS ORDER, so two files may not share one.
//
// The prefix IS the ordering ([schemaVersions] sorts filenames), so two files at
// one number run in whatever alphabetical order follows the underscore, which
// nobody chose. Nothing else notices: both apply and get their own
// `schema_migrations` row, until two touch the same table and one estate has a
// column the other lacks.
//
// The numbers are also CONTIGUOUS from 1: a gap is a migration never committed,
// or deleted after being applied somewhere, a divergence no later file repairs.
func TestEveryMigrationHasItsOwnNumberAndNoneAreMissing(t *testing.T) {
	t.Parallel()
	for _, estate := range Estates {
		t.Run(string(estate), func(t *testing.T) {
			seen := map[int]string{}
			for _, name := range SchemaVersions(estate) {
				prefix, _, ok := strings.Cut(name, "_")
				if !ok {
					t.Errorf("%q has no numeric prefix, so its place in the "+
						"order is whatever sorting says", name)
					continue
				}
				n, err := strconv.Atoi(prefix)
				if err != nil || n < 1 {
					t.Errorf("%q is prefixed %q, which is not a migration "+
						"number", name, prefix)
					continue
				}
				if first, held := seen[n]; held {
					t.Errorf("%q and %q are both migration %d — the prefix is "+
						"the ordering, so these two run in alphabetical order "+
						"of what follows it, which is an order nobody chose",
						first, name, n)
					continue
				}
				seen[n] = name
			}
			for n := 1; n <= len(seen); n++ {
				if _, held := seen[n]; !held {
					t.Errorf("the %s estate has %d migrations and none numbered "+
						"%d — a gap is a file that was written and never "+
						"committed, or one deleted after it had already run",
						estate, len(seen), n)
				}
			}
		})
	}
}

// A ROLLED-BACK NODE KNOWS WHAT IT CANNOT READ. The migration runner steps
// over a version this binary does not carry — a newer build's, applied before
// the node was rolled back — so the node boots on that file, and
// ForeignMigrations is how a caller about to act on "nothing names this", the
// object store's collector above all, learns that tables it cannot read are in
// it.
//
// Mutation: answer nothing, or answer Pending's question (what this binary
// carries and the file lacks), and the newer build's version goes unreported.
func TestARolledBackDatabaseReportsTheMigrationsItDoesNotCarry(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "rolled-back.db"), Options{})
	if err != nil {
		t.Fatalf("OpenNode: %v", err)
	}
	defer func() { _ = db.Close() }()
	if foreign, err := db.ForeignMigrations(t.Context()); err != nil || len(foreign) != 0 {
		t.Fatalf("a file this binary migrated reports %v as foreign (err %v)", foreign, err)
	}

	const newer = "9999_a_newer_build.sql"
	if _, err := db.SQL().ExecContext(t.Context(),
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		newer, EncodeTime(time.Now())); err != nil {
		t.Fatalf("record a newer build's migration: %v", err)
	}
	foreign, err := db.ForeignMigrations(t.Context())
	if err != nil {
		t.Fatalf("ForeignMigrations: %v", err)
	}
	if !slices.Equal(foreign, []string{newer}) {
		t.Fatalf("ForeignMigrations = %v, want [%s]: a rolled-back node cannot "+
			"tell that tables it cannot read are in its file", foreign, newer)
	}
}
