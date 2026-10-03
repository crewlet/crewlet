package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestTheReaderPoolIsDerivedFromTheMachineAndKeepsItsFloor.
//
// The bound was a literal 4, chosen as "the dashboard admits four concurrent
// queries" — a number describing ONE socket, on a host of any size. A company
// watched from three tabs offered twelve concurrent scans to a pool of four
// and the engine's own reads queued behind whichever four arrived first, and
// nothing about it moved with the machine: a 16-core node ran the same four
// connections as a laptop.
func TestTheReaderPoolIsDerivedFromTheMachineAndKeepsItsFloor(t *testing.T) {
	t.Parallel()
	got := defaultReaderConns()
	if want := max(minReaderConns, runtime.GOMAXPROCS(0)); got != want {
		t.Errorf("defaultReaderConns() = %d, want max(%d, GOMAXPROCS=%d) = %d",
			got, minReaderConns, runtime.GOMAXPROCS(0), want)
	}
	// THE FLOOR IS LOAD-BEARING and not decoration: two full dashboards is
	// eight concurrent queries, and a one-core host would otherwise size
	// its pool at one and make a single tab queue against itself.
	if got < minReaderConns {
		t.Errorf("the derived bound %d fell below the floor of %d", got, minReaderConns)
	}
	// And the pool built from it carries the identity reserve, or the
	// reserve would be taken out of the readers rather than kept beside
	// them.
	if got, want := (Options{}).poolSize(), defaultReaderConns()+identityReserve; got != want {
		t.Errorf("poolSize() = %d, want %d", got, want)
	}
	// A caller that sets the bound owns the arithmetic: it wins outright.
	if got := (Options{MaxOpenConns: 3}).poolSize(); got != 3 {
		t.Errorf("an explicit bound became %d, want the 3 that was asked for", got)
	}
}

// TestTheReserveIsSkippedWhereThereIsNoRoomForIt.
//
// A handle opened at one connection owns its own arithmetic and has to be able
// to serve somebody. Holding a connection back there would leave a handle with
// none, which is a deadlock rather than a tight fit — and a backup's own
// handle and an adoption's are both exactly that case.
func TestTheReserveIsSkippedWhereThereIsNoRoomForIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts Options
		pins int
		want bool
	}{
		{"one connection, no room to hold any back", Options{MaxOpenConns: 1}, 0, false},
		{"two connections, one held", Options{MaxOpenConns: 2}, 0, true},
		{"the pins are spoken for too", Options{MaxOpenConns: 4}, 3, false},
		{"and one more than the pins is room", Options{MaxOpenConns: 5}, 3, true},
		{"the derived default always has room", Options{}, 0, true},
	} {
		if got := tc.opts.reserveRoom(tc.pins); got != tc.want {
			t.Errorf("%s: reserveRoom = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestASocketStormCannotStarveIdentityWork is the reservation doing its job.
//
// A socket storm is N dashboards × four in-flight queries each, every one of
// them a scan. They take every connection first-come-first-served, and the
// identity read that would let those very requests be decided queues behind
// all of them — so the queue feeds itself, exactly as the reader/writer loop
// [Writer] exists to break does. One connection is HELD from the open, and
// [Identity] is the only way to spend it.
func TestASocketStormCannotStarveIdentityWork(t *testing.T) {
	t.Parallel()
	// Four connections: one held for identity, three left for readers.
	const pool = 4
	opts := Options{MaxOpenConns: pool}
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "storm.db"), opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if !opts.reserveRoom(0) {
		t.Fatalf("the fixture is not the shape this case is about: a pool of "+
			"%d holds nothing back", pool)
	}
	ordinary := pool - identityReserve

	// Saturate the readers: every remaining connection held by a read that
	// will not return until this test says so.
	held := make(chan struct{})
	entered := make(chan struct{}, ordinary)
	var storm sync.WaitGroup
	for range ordinary {
		storm.Go(func() {
			_ = db.Read(t.Context(), func(*sql.Tx) error {
				entered <- struct{}{}
				<-held
				return nil
			})
		})
	}
	for range ordinary {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the storm never took its connections")
		}
	}
	defer func() {
		close(held)
		storm.Wait()
	}()

	// THE CONTROL, and it is what makes the assertion below able to fail:
	// one more ORDINARY read is held for as long as the storm runs.
	// Without it, an identity read that succeeded would prove only that
	// the pool was never full.
	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		_ = db.Read(t.Context(), func(*sql.Tx) error { return nil })
	}()
	select {
	case <-blocked:
		t.Fatal("an ordinary read got through a saturated pool, so the " +
			"reserved connection is not being held and this case proves nothing")
	case <-time.After(250 * time.Millisecond):
	}

	// AND THAT WAIT IS STILL DATABASE/SQL'S. This is why the reserve is a
	// held connection rather than a semaphore in front of the pool: a gate
	// would bound ordinary work below the pool's own limit, database/sql
	// would never reach it, WaitCount would stop counting and the
	// `pool_starved` alarm — whose entire input is this counter — would go
	// quiet for good.
	if got := db.SQL().Stats().WaitCount; got == 0 {
		t.Error("a reader queued and sql.DBStats.WaitCount stayed at 0: the " +
			"pool_starved alarm reads that counter, so it has gone blind")
	}

	// And the identity read goes straight through, on the connection the
	// reserve is holding.
	done := make(chan error, 1)
	go func() {
		done <- db.Read(Identity(context.Background()), func(tx *sql.Tx) error {
			var one int
			return tx.QueryRowContext(context.Background(), `SELECT 1`).Scan(&one)
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the identity read failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("identity work was starved by a socket storm: the reserved " +
			"connection was spent on ordinary reads")
	}
}

// TestIdentityWorkFallsBackWhereNoConnectionCouldBeHeld: a handle with no room
// for a reserve must still answer an identity read, from the pool like
// everybody else. That is where those reads were before the reserve existed,
// and refusing a question the handle can answer would be worse.
func TestIdentityWorkFallsBackWhereNoConnectionCouldBeHeld(t *testing.T) {
	t.Parallel()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "one.db"),
		Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Read(Identity(t.Context()), func(tx *sql.Tx) error {
		var one int
		return tx.QueryRowContext(t.Context(), `SELECT 1`).Scan(&one)
	}); err != nil {
		t.Errorf("an identity read on a single-connection handle failed: %v", err)
	}
}

// TestTheIdentityMarkIsOffByDefault: only work that says it is identity work
// reaches the reserved connection, or the reservation is headroom that the
// first burst spends.
func TestTheIdentityMarkIsOffByDefault(t *testing.T) {
	t.Parallel()
	if isIdentity(context.Background()) {
		t.Error("a plain context reads as identity work")
	}
	if !isIdentity(Identity(context.Background())) {
		t.Error("a marked context does not read as identity work")
	}
	// The mark survives a derived context, which is what threading it
	// through a request means.
	ctx, cancel := context.WithTimeout(Identity(context.Background()), time.Minute)
	defer cancel()
	if !isIdentity(ctx) {
		t.Error("the identity mark was lost by a derived context")
	}
}
