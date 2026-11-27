package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// The node file is a guard's ledger, checked where it is built.
var _ signedask.Ledger = store.TakenRequests{}

var takenAt = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

// takenAsk is a write a signed channel takes, as a guard over the node file
// keeps it.
type takenAsk struct {
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`
}

func (a takenAsk) Binding() (string, time.Time) { return a.ID, a.Deadline }
func (a takenAsk) Writes() bool                 { return true }

type takenAnswer struct {
	Request string `json:"request"`
}

func (a takenAnswer) Answers() string { return a.Request }

var ringForTaken = recordseal.OneKey("k1", "the taken-requests cases' fleet key material")

// A TAKEN WRITE'S ID OUTLIVES A CLOSE AND A REOPEN OF THE NODE FILE, by its
// channel, until its window ends: after the restart a Keep of the id is not
// fresh, a Forget before its until keeps it and one at its until deletes it,
// and an id of another channel is that channel's alone.
//
// Mutations: Keep's ON CONFLICT answers fresh, Forget drops its channel
// filter, or Forget deletes by `<` rather than `<=`: red.
func TestATakenWritesIDOutlivesTheNodeFilesRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "node.db")
	db := storetest.OpenNode(t, path, store.Options{})
	file := store.NewTakenRequests(db)
	until := takenAt.Add(time.Minute)
	held := func(channel string) bool {
		t.Helper()
		fresh, err := file.Keep(t.Context(), channel, "w-1", until)
		if err != nil {
			t.Fatal(err)
		}
		return !fresh
	}
	for _, channel := range []string{"estate", "seat.broker"} {
		if held(channel) {
			t.Fatalf("the first keep of w-1 on %s found it held already", channel)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = storetest.OpenNode(t, path, store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	file = store.NewTakenRequests(db)
	if !held("estate") {
		t.Fatal("after a restart the node file did not hold the estate's w-1")
	}
	if err := file.Forget(t.Context(), "estate", until.Add(-time.Nanosecond)); err != nil || !held("estate") {
		t.Fatalf("a forget before w-1's until (%v) deleted it", err)
	}
	if err := file.Forget(t.Context(), "estate", until); err != nil {
		t.Fatal(err)
	}
	if held("estate") {
		t.Fatal("a forget at w-1's until left it held")
	}
	if !held("seat.broker") {
		t.Fatal("the estate's forget deleted the broker's w-1")
	}
}

// THE TABLE IS BOUNDED BY THE WINDOW, NOT BY HOW MANY WRITES EVER RAN: a
// guard over the node file that takes many short writes and then sweeps leaves
// only the writes still inside their windows on the disk.
//
// Mutation: Guard.take skips the Forget after a sweep: every short write's row
// stays, red.
func TestTheNodeFileHoldsOnlyTheWritesInsideTheirWindows(t *testing.T) {
	t.Parallel()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	c, err := signedask.New[takenAsk, takenAnswer]("test.taken", ringForTaken)
	if err != nil {
		t.Fatal(err)
	}
	g, err := c.KeptGuard(t.Context(), store.NewTakenRequests(db), takenAt)
	if err != nil {
		t.Fatal(err)
	}
	admit := func(id string, deadline, now time.Time) {
		t.Helper()
		sealed, err := c.SealRequest(takenAsk{ID: id, Deadline: deadline})
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Admit(t.Context(), sealed, g, now); got.Disposition != signedask.Admitted {
			t.Fatalf("%s = %+v", id, got)
		}
	}
	// THE GUARD SWEEPS AT 1024 IDS (signedask's sweep floor): 1024 short
	// writes reach it, and the next admission, past all their windows, sweeps.
	for i := range 1024 {
		admit(fmt.Sprintf("w-%d", i), takenAt.Add(time.Second), takenAt)
	}
	later := takenAt.Add(time.Minute)
	admit("w-after", later.Add(time.Minute), later)
	var rows int
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT count(*) FROM signed_request_taken WHERE channel = 'test.taken'`).Scan(&rows)
	}); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("the node file holds %d rows after a sweep, want the one write still inside "+
			"its window", rows)
	}
}

// A NODE FILE THAT REFUSES THE WRITE REFUSES THE REQUEST: once the file is
// closed, a write is answered unrecorded and is not held, and a read is
// still admitted.
//
// Mutation: Guard.take admits a write whose Keep failed: red.
func TestAWriteTheNodeFileRefusesIsUnrecorded(t *testing.T) {
	t.Parallel()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	c, err := signedask.New[takenAsk, takenAnswer]("test.taken", ringForTaken)
	if err != nil {
		t.Fatal(err)
	}
	g, err := c.KeptGuard(t.Context(), store.NewTakenRequests(db), takenAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	sealed, err := c.SealRequest(takenAsk{ID: "w-1", Deadline: takenAt.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	got := c.Admit(t.Context(), sealed, g, takenAt)
	if got.Disposition != signedask.Unrecorded || !strings.Contains(got.Why, "could not keep") {
		t.Fatalf("a write the closed file refused = %+v, want it unrecorded", got)
	}
	if _, err := c.KeptGuard(t.Context(), store.NewTakenRequests(db), takenAt); err == nil ||
		!errors.Is(err, store.ErrNoEstate) {
		t.Errorf("a guard over a closed file started: %v", err)
	}
}

// A SCRATCH NODE FILE KEEPS NO ID, so a guard over it does not start: a file
// deleted at every boot would forget every write it took, and an IN channel
// is served only on a node with the data role, whose file is durable.
//
// Mutation: TakenRequests.usable ignores Scratch: the guard starts, red.
func TestAScratchNodeFileStartsNoGuard(t *testing.T) {
	t.Parallel()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{Scratch: true})
	t.Cleanup(func() { _ = db.Close() })
	c, err := signedask.New[takenAsk, takenAnswer]("test.taken", ringForTaken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.KeptGuard(t.Context(), store.NewTakenRequests(db), takenAt); !errors.Is(err, store.ErrScratchLedger) {
		t.Fatalf("a guard over a scratch node file = %v, want %v", err, store.ErrScratchLedger)
	}
	if _, err := store.NewTakenRequests(db).Keep(t.Context(), "test.taken", "w-1", takenAt.Add(time.Minute)); !errors.Is(err, store.ErrScratchLedger) {
		t.Errorf("a keep in a scratch node file = %v, want %v", err, store.ErrScratchLedger)
	}
}

// BenchmarkKeepingATakenRequest is one Keep, a committed and fsynced INSERT
// into the node file, as a write's admission pays it: writers=1 serially, and
// writers=16 at once (sixteen requests admitted together queue on the file's
// one write lock). Each arm reports the p50 and p99 of one Keep, for the cost
// beside an estate write's (internal/statelog's BenchmarkArbitratedWrite,
// members=1, sync=always, is that write's floor).
func BenchmarkKeepingATakenRequest(b *testing.B) {
	for _, writers := range []int{1, 16} {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			db := storetest.OpenNode(b, filepath.Join(b.TempDir(), "node.db"), store.Options{})
			b.Cleanup(func() { _ = db.Close() })
			file := store.NewTakenRequests(db)
			until := time.Now().Add(time.Hour)
			var (
				next atomic.Int64
				wg   sync.WaitGroup
				mu   sync.Mutex
				took []time.Duration
			)
			errs := make([]error, writers)
			b.ResetTimer()
			for w := range writers {
				wg.Go(func() {
					var mine []time.Duration
					for n := next.Add(1); n <= int64(b.N); n = next.Add(1) {
						at := time.Now()
						if _, err := file.Keep(context.Background(), "bench", fmt.Sprintf("w-%d", n), until); err != nil {
							errs[w] = err
							return
						}
						mine = append(mine, time.Since(at)) //fixedwait:measure one Keep's latency, reported and deciding nothing
					}
					mu.Lock()
					took = append(took, mine...)
					mu.Unlock()
				})
			}
			wg.Wait()
			b.StopTimer()
			if err := errors.Join(errs...); err != nil {
				b.Fatal(err)
			}
			slices.Sort(took)
			at := func(q float64) float64 {
				if len(took) == 0 {
					return 0
				}
				rank := max(int(q*float64(len(took))+0.999999), 1)
				return float64(took[rank-1]) / float64(time.Millisecond)
			}
			b.ReportMetric(at(0.50), "p50-ms")
			b.ReportMetric(at(0.99), "p99-ms")
		})
	}
}
