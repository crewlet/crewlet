package store

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// WHAT THESE CASES PROTECT: Pending is a second way into a connection, and it
// has to reach one exactly as Open does.
//
// [Pending] answers "what would `crewlet migrate` apply?" without applying it,
// which means it opens a pool and reads. It gets that pool from openPrepared,
// as Open does, so the diagnostic command:
//
//   - prepares the native library before the driver's own loader runs, whose
//     answer to a half-written cache is a panic inside a sync.Once (see
//     turso.go) — the command that exists to report the schema safely must not
//     be one that can take the process down;
//   - runs on the pool bounds every other opener gets;
//
// and it holds the file lock across its read, so the database of a LIVE
// engine is refused rather than read. These cases are what hold each of those
// in place.

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
	out, err := cmd.CombinedOutput()
	requireChildRan(t, "Pending did not report the broken cache", out, err)
}

// TestPendingWithABrokenLibraryCacheInAChildProcess is the child half.
func TestPendingWithABrokenLibraryCacheInAChildProcess(t *testing.T) {
	if os.Getenv(pendingChildEnv) == "" {
		t.Skip("not a child process; see TestPendingReportsABrokenLibraryCacheInsteadOfPanicking")
	}
	// A database that is THERE, so Pending has something to open: one that
	// is not is reported without the driver being asked anything. Empty is
	// what the driver reads as a new database.
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Pending(t.Context(), path, Options{})
	if err == nil {
		t.Fatal("Pending succeeded against a library cache that cannot exist")
	}
	if !strings.Contains(err.Error(), os.Getenv(tursoCacheEnv)) {
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
// is checked is that the claim on each estate's path is gone: no entry, not
// merely a second caller getting past it.
//
// EVERY WAY IT TAKES ONE: a sidecar it finds beside a database it reads, a
// sidecar it makes beside a database that has none — the shape a snapshot
// fetched for adoption arrives in, which [PendingEstate] inspects through the
// same path — and a sidecar it finds where there is no database to read.
func TestPendingReleasesTheLockForTheMigrationThatFollows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// stage leaves path the way Pending finds it.
		stage func(t *testing.T, path string)
		// sidecar is whether stage leaves a lock sidecar beside path, which
		// decides whether Pending finds its claim or has to make one.
		sidecar bool
	}{
		{"a database it reads", func(t *testing.T, path string) {
			db, err := Open(t.Context(), path, Options{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			_ = db.Close()
		}, true},
		{"a database with no sidecar beside it", func(t *testing.T, path string) {
			db, err := Open(t.Context(), path, Options{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			_ = db.Close()
			for _, file := range []string{path, ReplicatedPath(path, "")} {
				if err := os.Remove(file + lockSuffix); err != nil {
					t.Fatalf("remove the sidecar beside %s: %v", file, err)
				}
			}
		}, false},
		{"a sidecar with no database beside it", func(t *testing.T, path string) {
			lock, err := lockStore(path)
			if err != nil {
				t.Fatalf("lockStore: %v", err)
			}
			lock.release()
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "seq.db")
			tc.stage(t, path)
			if _, err := os.Stat(path + lockSuffix); (err == nil) != tc.sidecar {
				t.Fatalf("a sidecar beside %s: %t, want %t (stat: %v) — the premise",
					path, err == nil, tc.sidecar, err)
			}

			if _, err := Pending(t.Context(), path, Options{}); err != nil {
				t.Fatalf("Pending: %v", err)
			}

			for _, file := range []string{path, ReplicatedPath(path, "")} {
				locksHeld.mu.Lock()
				held := locksHeld.by[file]
				locksHeld.mu.Unlock()
				if held != nil {
					t.Fatalf("Pending kept its claim on %s (%d holder(s)) — the process "+
						"never gives the file back, and the next one to want it is refused "+
						"by a lock nothing is using", file, held.holds)
				}
			}

			db, err := Open(t.Context(), path, Options{})
			if err != nil {
				t.Fatalf("Open after Pending: %v", err)
			}
			_ = db.Close()
		})
	}
}

// PENDING MAKES NOTHING FOR A DATABASE THAT IS NOT THERE. The deploy gate runs
// before the node does, possibly as another user; a check that left a database,
// its -wal and its lock behind would leave the node files it then meets as
// somebody else's, and a database at a path the check was merely pointed at.
// What it reports instead is the truth about the path: nothing applied,
// everything pending.
func TestPendingMakesNothingForADatabaseThatIsNotThere(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	schemas, err := Pending(t.Context(), filepath.Join(dir, "absent.db"), Options{})
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	for _, sch := range schemas {
		if len(sch.Applied) != 0 || len(sch.Pending) != len(SchemaVersions(sch.Estate)) {
			t.Errorf("the absent %s estate reports %d applied and %d pending, want none and all %d",
				sch.Estate, len(sch.Applied), len(sch.Pending), len(SchemaVersions(sch.Estate)))
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("Pending left %v on a path that held no database", names)
	}
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

// THE POOL BOUNDS REACH EVERY OPENER, asserted on the shared helper because it
// is the one place any of them gets a pool on a database file.
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

// A MIGRATION'S NUMBER IS ITS ORDER, so two files may not share one.
//
// # Why this is a guard rather than a convention
//
// The prefix IS the ordering — [schemaVersions] sorts filenames and there is
// no second source of truth — so two files at one number are ordered by
// whatever follows the underscore, alphabetically. That is not an ordering
// anybody chose, and it is not one anybody reading the directory would
// predict: `0022_a…` runs before `0022_z…` for a reason nothing states.
//
// Nothing else notices. Both files apply, both get their own row in
// `schema_migrations` (which keys on the base filename), and a fresh database
// ends up identical to one that applied them in the other order — right up
// until the day the two touch the same table, when one estate has a column
// the other does not and the difference is a build artefact.
//
// The numbers are also CONTIGUOUS from 1, which is the half that catches the
// other mistake: a gap is a migration somebody wrote, numbered, and never
// committed — or one that was deleted after it had already been applied
// somewhere, which is a divergence no later file can repair.
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
