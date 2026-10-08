package store

import (
	"context"
	"database/sql/driver"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// TWO HANDLES ON ONE FILE MIGRATE IT ONCE, the second waiting for the first.
//
// Two handles share one claim on a path, and a migration run reads the ledger
// once and then applies every file it did not find, so two runs that overlap
// both apply what neither had found. Measured with the lock removed: a second
// handle opened while the first was held after its ledger read ran the whole
// sequence, and the first, let go, failed at 0001 with `table crewlet_events
// already exists`. So a run holds its FILE's lock from before it reads the
// ledger until its last file commits.
//
// NO WALL CLOCK DECIDES IT, and neither does the race's outcome. The first
// handle is held at the BEGIN of its first file, after it read an empty
// ledger, and while it is there the lock the second handle's run would take is
// asked whether it is free — which is the whole question, answered rather than
// waited out. Two opens left to race prove nothing either way: without the
// lock the second still usually arrives after the first has finished. Only
// once the lock has answered is the second handle opened and the first let go,
// and both must end with the sequence applied once.
//
// Mutation: drop the lock from [DB.migrate], or take one per handle rather
// than the claim's, and the lock reads free while the first run is in it.
func TestTwoHandlesOnOneFileMigrateItOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "shared.db")
	held := newBeginGate(true)
	t.Cleanup(held.open)

	first := openAsync(ctx, path, Options{WrapDriver: held.wrap})
	held.waitReached(t, "the first handle's migration")

	locksHeld.mu.Lock()
	claim := locksHeld.by[path]
	locksHeld.mu.Unlock()
	if claim == nil {
		t.Fatal("the first handle is migrating a file this process holds no claim on")
	}
	if running := claim.migrations(); running.TryLock() {
		running.Unlock()
		t.Fatal("the first handle is part way through migrating the file and its " +
			"migration lock is free: a second handle would read the same empty " +
			"ledger and apply 0001 again")
	}

	second := openAsync(ctx, path, Options{})
	held.open()
	for name, opened := range map[string]chan openResult{"first": first, "second": second} {
		got := <-opened
		if got.err != nil {
			t.Fatalf("the %s handle on one file: %v", name, got.err)
		}
		applied, err := got.db.AppliedMigrations(ctx)
		_ = got.db.Close()
		if err != nil {
			t.Fatalf("the %s handle's ledger: %v", name, err)
		}
		if want := SchemaVersions(EstateNode); !slices.Equal(applied, want) {
			t.Errorf("the %s handle reads the ledger as %v, want the sequence "+
				"applied once: %v", name, applied, want)
		}
	}
}

// TWO FILES MIGRATE AT THE SAME TIME, because nothing about one file's ledger
// can race another's.
//
// The lock is the FILE's. It was once the PROCESS's, on the premise that a
// process migrates once, at boot — and every process that opens more than one
// store ran every file's migrations one after another: an in-process fleet,
// and every test binary, which spent most of its wall clock queued behind DDL
// that could never have raced its own.
//
// The first file is held at the BEGIN of its first migration, and the second
// must reach the BEGIN of ITS first migration while the first is still held.
// The wait for that is BOUNDED, because the regression this catches is a lock
// wider than one file — under which the second run never begins while the
// first is held, and an unbounded wait would hang the package instead of
// failing it.
//
// Mutation: put the process-wide mutex back, and the second run never begins
// inside the bound.
func TestMigrationsOfDifferentFilesRunTogether(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	held, watched := newBeginGate(true), newBeginGate(false)
	t.Cleanup(held.open)

	first := openAsync(ctx, filepath.Join(t.TempDir(), "first.db"), Options{WrapDriver: held.wrap})
	held.waitReached(t, "the first file's migration")
	second := openAsync(ctx, filepath.Join(t.TempDir(), "second.db"), Options{WrapDriver: watched.wrap})

	select {
	case <-watched.reached:
	case <-time.After(migrationStartBound):
		t.Errorf("the second file's migration did not begin within %v while the "+
			"first file's was held: one file's migration is waiting on another's, "+
			"which nothing about either ledger needs", migrationStartBound)
	}
	held.open()
	for name, opened := range map[string]chan openResult{"first": first, "second": second} {
		got := <-opened
		if got.err != nil {
			t.Fatalf("the %s file: %v", name, got.err)
		}
		_ = got.db.Close()
	}
}

// migrationStartBound is how long a run on an idle file may take to reach its
// first migration's BEGIN: a lock, a pool, a ping and one read of an empty
// ledger, which is milliseconds even under the race detector on a saturated
// runner. Ten seconds is the same order of headroom [waitForWaiters] allows
// for the write queue, and short enough that a regression fails here naming
// the cause rather than at the package's timeout.
const migrationStartBound = 10 * time.Second

// openResult is one asynchronous [OpenNode]'s answer.
type openResult struct {
	db  *DB
	err error
}

// openAsync opens a node's store at path on its own goroutine, so a test can
// act while the open is held part way through.
func openAsync(ctx context.Context, path string, opts Options) chan openResult {
	out := make(chan openResult, 1)
	go func() {
		db, err := OpenNode(ctx, path, opts)
		out <- openResult{db: db, err: err}
	}()
	return out
}

// beginGate notes the FIRST transaction a pool begins — on a fresh file, the
// first migration's, since nothing before the migration begins one — and,
// when it holds, keeps that BEGIN from reaching the driver until it is
// opened. Held there, a migration has read its ledger and holds no lock of the
// driver's, so whatever else is running is held by the store's locks alone.
type beginGate struct {
	holds   bool
	reached chan struct{}
	once    sync.Once
	opened  chan struct{}
	release sync.Once
}

func newBeginGate(holds bool) *beginGate {
	return &beginGate{holds: holds, reached: make(chan struct{}), opened: make(chan struct{})}
}

// open lets a held BEGIN through. Idempotent, so a test can register it as a
// cleanup and still open the gate itself.
func (g *beginGate) open() { g.release.Do(func() { close(g.opened) }) }

// waitReached waits for the gated BEGIN, bounded for the reason
// [migrationStartBound] gives.
func (g *beginGate) waitReached(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(migrationStartBound):
		t.Fatalf("%s did not begin within %v", what, migrationStartBound)
	}
}

func (g *beginGate) wrap(d driver.Driver) driver.Driver {
	return (&driverHooks{begin: g.begin}).wrap(d)
}

// begin is called on every BEGIN, and only the first is noted or held.
func (g *beginGate) begin(ctx context.Context) error {
	first := false
	g.once.Do(func() {
		first = true
		close(g.reached)
	})
	if !first || !g.holds {
		return nil
	}
	select {
	case <-g.opened:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
