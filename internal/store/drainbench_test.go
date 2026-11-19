package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// applierTables is the shape a state log's applier writes: one wide row per
// object plus a fan-out of narrow child rows, which is what a record's apply
// actually produces.
const applierTables = `
CREATE TABLE bench_items (
	id         TEXT    NOT NULL PRIMARY KEY,
	project    TEXT    NOT NULL,
	title      TEXT    NOT NULL,
	status     TEXT    NOT NULL,
	assignee   TEXT,
	version    INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX bench_items_project ON bench_items (project, status);
CREATE TABLE bench_edges (
	item_id TEXT NOT NULL,
	kind    TEXT NOT NULL,
	other   TEXT NOT NULL,
	PRIMARY KEY (item_id, kind, other)
);
`

// foreignTable is a table the applier never reads or writes, created in
// whichever estate an arm points its unrelated commits at.
const foreignTable = `
CREATE TABLE bench_foreign (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	payload TEXT NOT NULL
)`

const (
	// benchRows is one apply transaction's row budget — the plan's
	// ApplyTxRowBudget, which is what makes this transaction applier-shaped
	// rather than a microbenchmark of one insert.
	benchRows = 4000

	// itemColumns and edgeColumns are the bind widths the chunker divides
	// the probed variable limit by.
	itemColumns = 7
	edgeColumns = 3

	// THE APPLIER'S REAL SHAPE: an upsert guarded on the version, because
	// a record is applied idempotently and a replay must not move a row
	// backwards. It also has a plan to make, which is what the prepared
	// arm is measuring.
	//
	// IN THREE PARTS because that is what [store.InsertRows] takes, and the
	// single-row form every per-row arm uses is composed back out of them —
	// so the arms cannot drift into measuring two different statements.
	itemPrefix = `INSERT INTO bench_items
		(id, project, title, status, assignee, version, updated_at)
		VALUES`
	itemRow    = `(?, ?, ?, ?, ?, ?, ?)`
	itemSuffix = `ON CONFLICT(id) DO UPDATE SET
			project = excluded.project, title = excluded.title,
			status = excluded.status, assignee = excluded.assignee,
			version = excluded.version, updated_at = excluded.updated_at
		WHERE excluded.version > bench_items.version`
	itemInsert = itemPrefix + " " + itemRow + " " + itemSuffix

	edgePrefix = `INSERT INTO bench_edges (item_id, kind, other) VALUES`
	edgeRow    = `(?, ?, ?)`
	edgeSuffix = `ON CONFLICT(item_id, kind, other) DO NOTHING`
	edgeInsert = edgePrefix + " " + edgeRow + " " + edgeSuffix
)

// BenchmarkLogApplyDrain measures the applier's drain in rows per second,
// across the three statement shapes it could be written in.
//
// The drain rate sets a fleet's read latency: every data node applies the same
// records serially, so a bulk edit occupies each applier for rows ÷ drain
// seconds, during which a linearizable read is behind.
//
// Arms: unprepared (ExecContext per row), prepared (prepared once, bound per
// row) and prepared+multirow (child rows chunked to the bind-variable limit).
// Prepared buys nothing (the driver implements ExecerContext); the arm stays as
// the reason the engine ships no statement cache.
// Run: `go test ./internal/store -run xxx -bench LogApplyDrain -benchtime 3x`.
func BenchmarkLogApplyDrain(b *testing.B) {
	for _, arm := range []struct {
		name string
		run  func(context.Context, *store.Writer, *store.DB, []benchItem) error
	}{
		{"unprepared", applyUnprepared},
		{"prepared", applyPrepared},
		{"prepared+multirow", applyPreparedMultiRow},
	} {
		b.Run(arm.name, func(b *testing.B) {
			db, w := benchWriter(b)
			items := benchItems(benchRows)
			ctx := b.Context()

			b.ResetTimer()
			started := time.Now()
			for i := 0; b.Loop(); i++ {
				b.StopTimer()
				resetBench(ctx, b, w)
				b.StartTimer()
				if err := arm.run(ctx, w, db, items); err != nil {
					b.Fatalf("apply: %v", err)
				}
			}
			elapsed := time.Since(started)
			b.StopTimer()
			rows := float64(b.N) * float64(len(items)+len(items)) // items + one edge each
			b.ReportMetric(rows/elapsed.Seconds(), "rows/s")
			b.ReportMetric(float64(b.N)/elapsed.Seconds(), "commits/s")
		})
	}
}

// BenchmarkApplyTxUnderForeignCommits prices what a long write transaction
// and the commits beside it cost each other, when those commits land in the
// applier's own file and when they land in the other estate.
//
// Writes take the lock at BEGIN IMMEDIATE (begin.go), served in order
// (writequeue.go), so correctness is held by [TestAReadThenWriteCannotLoseARace]
// and [TestAForeignCommitDoesNotAbortAnApplierTransaction]. This measures lock
// contention: aborts/tx and foreign-refused/s should read zero, a nonzero one
// is the finding. Arms: same-file and other-estate (the NODE estate, as
// shipped). On tursogo v0.8.1 the foreign writer got 5 commits/s same-file
// against 1 300–1 900 other-estate: why the two-file split is unconditional.
func BenchmarkApplyTxUnderForeignCommits(b *testing.B) {
	for _, arm := range []struct {
		name string
		// foreign picks the estate the unrelated commits go to.
		foreign func(node, replicated *store.DB) *store.DB
	}{
		{"same-file", func(_, replicated *store.DB) *store.DB { return replicated }},
		{"other-estate", func(node, _ *store.DB) *store.DB { return node }},
	} {
		b.Run(arm.name, func(b *testing.B) {
			node, w := benchNode(b)
			replicated := replicatedOf(b, node)
			items := benchItems(benchRows)
			ctx := b.Context()

			// The foreign table exists in BOTH estates, so the two arms
			// differ only in which file the commits land in.
			writer := arm.foreign(node, replicated)
			if err := writer.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, foreignTable)
				return err
			}); err != nil {
				b.Fatalf("create the foreign table: %v", err)
			}

			var aborts, foreign, refused atomic.Int64
			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					// THE AUDIT LOG'S SHAPE: small, frequent, and
					// touching nothing the applier reads.
					err := writer.Tx(ctx, func(tx *sql.Tx) error {
						_, err := tx.ExecContext(ctx,
							`INSERT INTO bench_foreign (payload) VALUES (?)`, "phase")
						return err
					})
					if err == nil {
						foreign.Add(1)
					} else {
						// COUNTED, NOT SWALLOWED. A foreign
						// writer that is being REFUSED is a
						// different fact from one that is
						// merely waiting, and a benchmark
						// reporting only successes cannot
						// tell them apart.
						refused.Add(1)
					}
				}
			}()

			b.ResetTimer()
			started := time.Now()
			for b.Loop() {
				b.StopTimer()
				resetBench(ctx, b, w)
				b.StartTimer()
				// COUNTED BY ATTEMPTS. Writer.Tx runs the body again
				// only after a lock wait that outlasted the busy
				// timeout or on a connection retired dirty — a stale
				// snapshot is not retried at all, and fails the run
				// below — so the abort count is how many times fn ran
				// beyond the first, and how much of the retry budget
				// this load spends.
				var attempts int
				if err := w.Tx(ctx, func(tx *sql.Tx) error {
					attempts++
					return insertUnprepared(ctx, tx, items)
				}); err != nil {
					b.Fatalf("apply under foreign commits: %v", err)
				}
				aborts.Add(int64(attempts - 1))
			}
			elapsed := time.Since(started)
			b.StopTimer()
			close(stop)
			wg.Wait()

			b.ReportMetric(float64(aborts.Load())/float64(b.N), "aborts/tx")
			b.ReportMetric(float64(b.N)*float64(2*len(items))/elapsed.Seconds(), "rows/s")
			b.ReportMetric(float64(foreign.Load())/elapsed.Seconds(), "foreign-commits/s")
			b.ReportMetric(float64(refused.Load())/elapsed.Seconds(), "foreign-refused/s")
		})
	}
}

// A FOREIGN COMMIT NEVER ABORTS AN APPLIER TRANSACTION, which is the fact the
// applier's whole occupancy model rests on.
//
// It holds by construction: begin.go takes the write lock at BEGIN (the
// driver's own BeginTx issues a DEFERRED begin, under which a read-then-write
// aborts on any commit in the file), so a losing applier waits having done
// nothing. SLOWING IS NOT ABORTING: the two-file split removes the slowdown
// (priced above); a RETRY would break the design. A driver that gained MVCC, or
// a begin mode no longer applied, shows up here first.
func TestAForeignCommitDoesNotAbortAnApplierTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a 4 000-row apply against a concurrent writer")
	}
	t.Parallel()
	node, w := benchNodeT(t)
	ctx := t.Context()
	if err := replicatedOf(t, node).Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, foreignTable)
		return err
	}); err != nil {
		t.Fatalf("create the foreign table: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var foreign atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := replicatedOf(t, node).Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					`INSERT INTO bench_foreign (payload) VALUES (?)`, "phase")
				return err
			}); err == nil {
				foreign.Add(1)
			}
		}
	}()
	// The writer is given the file to itself first, so a run where it never
	// managed a single commit fails as a staging problem rather than
	// passing as an answer about an idle store.
	settleForeign(t, &foreign)

	var attempts int
	var during int64
	err := w.Tx(ctx, func(tx *sql.Tx) error {
		attempts++
		before := foreign.Load()
		// READ-THEN-WRITE, in the applier's own order. A body that
		// wrote first would take its snapshot and the lock together
		// and could not meet a stale snapshot at all, which is what
		// this case is meant to be able to detect.
		applyErr := applyShaped(ctx, tx, benchItems(benchRows))
		during = foreign.Load() - before
		return applyErr
	})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("the apply failed under a concurrent writer: %v", err)
	}
	if attempts != 1 {
		t.Errorf("the applier's transaction ran %d times against a writer that "+
			"committed %d times to a table it never touches.\n"+
			"\tthe begin mode (internal/store/begin.go) takes the write lock at "+
			"BEGIN, so the only ways to reach a second attempt are a lock wait "+
			"that outlasted the busy timeout or a begin mode that is no longer "+
			"being applied — check the store_tx_retry WARN's cause field, which "+
			"names which",
			attempts, foreign.Load())
	}
	t.Logf("%d foreign commit(s) in total, %d of them while the applier's "+
		"transaction was open; %d attempt(s)", foreign.Load(), during, attempts)
}

// AN APPLIER IS NOT STARVED BY A WRITER COMMITTING BACK TO BACK. The driver's
// lock has no queue, so a poller wins only when a poll lands between commits;
// on darwin (F_FULLFSYNC, ~4 ms) it starved. writequeue.go serves in order.
// It asserts the ORDER as a count, at most [maxCommitsAhead] foreign commits
// between the ask and the body, never a duration. Staged at darwin's odds: each
// foreign commit holds the lock for [foreignHold], and the ask is made inside a
// transaction held for [askHold], since a settle-loop wake-up lands in the gap
// a poller gets through. Unqueued the applier started behind 103 to 4 355
// commits; queued, one. The queue's own guarantee is
// TestWritersBeginInTheOrderTheyAsked's; the writer must commit again
// afterwards (a queue that stopped it would pass) and is never refused.
func TestAnApplierIsNotStarvedByAWriterCommittingBackToBack(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a 4 000-row apply against a concurrent writer")
	}
	t.Parallel()
	node, w := benchNodeQueued(t)
	ctx := t.Context()
	replicated := replicatedOf(t, node)
	if err := replicated.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, foreignTable)
		return err
	}); err != nil {
		t.Fatalf("create the foreign table: %v", err)
	}

	stop := make(chan struct{})
	holding := make(chan struct{}, 1)
	var holdForAsk atomic.Bool
	var wg sync.WaitGroup
	var foreign, refused atomic.Int64
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			hold, told := foreignHold, chan<- struct{}(nil)
			if holdForAsk.CompareAndSwap(true, false) {
				hold, told = askHold, holding
			}
			if err := commitForeign(ctx, replicated, hold, told); err == nil {
				foreign.Add(1)
			} else {
				refused.Add(1)
			}
		}
	})
	settleForeign(t, &foreign)

	// THE APPLIER ASKS WHILE A FOREIGN TRANSACTION HOLDS THE LOCK — one the
	// writer holds for [askHold], so the ask lands inside it — and not at
	// whatever instant the settle loop happened to return: that loop wakes
	// on the writer's commit counter, which moves in the GAP between one
	// commit and the next begin, so an ask timed by it was an ask timed to
	// land in a gap, which is the one moment a store without its queue lets
	// a polling waiter straight in.
	holdForAsk.Store(true)
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		t.Fatal("the concurrent writer never began the transaction the ask is made behind")
	}

	var attempts int
	var waitedBehind int64
	asked := foreign.Load()
	started := time.Now()
	err := w.Tx(ctx, func(tx *sql.Tx) error {
		attempts++
		// ON THE FIRST ATTEMPT ONLY: a retry's wait is a different
		// quantity, and attempts is asserted to be one anyway.
		if attempts == 1 {
			waitedBehind = foreign.Load() - asked
		}
		return applyShaped(ctx, tx, benchItems(benchRows))
	})
	elapsed := time.Since(started)
	if err == nil {
		// THE OTHER DIRECTION: the queue hands the lock on rather than
		// releasing it, so the writer that waited behind the apply
		// commits again once the apply is done. Without this the case
		// would pass on a store that had simply stopped the writer.
		settleAfter(t, &foreign, foreign.Load())
	}
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("the apply failed beside a writer committing back to back: %v", err)
	}
	if attempts != 1 {
		t.Errorf("the applier's transaction ran %d times beside a writer "+
			"committing back to back: it should have waited for the commits "+
			"queued ahead of it, not polled against them", attempts)
	}
	if waitedBehind > maxCommitsAhead {
		t.Errorf("the applier's body started behind %d foreign commits past its "+
			"ask, want at most %d: a writer that asked while one transaction "+
			"held the lock goes next, and one that polls instead waits out "+
			"commit after commit of a writer re-beginning the moment it lets go "+
			"— the store is not serving its writers in order",
			waitedBehind, maxCommitsAhead)
	}
	if n := refused.Load(); n != 0 {
		t.Errorf("the writer beside the apply was refused %d time(s): a writer "+
			"queued behind the apply waits for it rather than failing", n)
	}
	t.Logf("%d foreign commit(s) in total, %d of them before the applier's body "+
		"started; the apply took %v", foreign.Load(), waitedBehind,
		elapsed.Round(time.Millisecond))
}

// maxCommitsAhead is how many foreign commits may land between the applier's
// ask and its body when the store serves writers in order.
//
// ONE: the transaction holding the lock when the applier asked, which it waits
// for by design, and no other. The ask is made while that transaction holds
// the lock, after the counter has taken every commit before it, so the
// counter's lag — it moves after a commit rather than with it — can only
// UNDERCOUNT the one commit the applier waited for.
const maxCommitsAhead = 1

// askHold is how long the foreign transaction the applier asks behind holds
// the lock.
//
// TWO HUNDRED MILLISECONDS, as the margin the ask is made inside of: the test
// asks microseconds after the writer says it holds the lock, and a queued
// applier reads a second commit only if its own goroutine stalls past this
// before it reaches the line — forty of [foreignHold]'s commits, on a runner
// shared with the rest of the suite. A stall that long is a machine too loaded
// to tell anything, and the case says so by failing rather than by passing.
// It costs the case this once, before the apply.
const askHold = 200 * time.Millisecond

// foreignHold is how long each foreign commit in the back-to-back scenario
// holds the write lock.
//
// FIVE MILLISECONDS, darwin's commit: `PRAGMA fullfsync` makes each one an
// F_FULLFSYNC of about 4 ms against a gap of microseconds, which is the ratio
// the starvation was measured at. Held for that long on every platform, a
// waiter that polls meets a gap on a vanishing share of its polls, so the count
// [maxCommitsAhead] bounds is one a store without its queue cannot reach by
// luck — which on a bare insert's quarter of a millisecond it could, twice in
// nine runs. It costs the queued case one such hold before the apply.
const foreignHold = 5 * time.Millisecond

// commitForeign is one small commit to the foreign table, the audit log's
// shape, through the store's own write path — holding the lock for hold, as a
// commit on darwin holds it for [foreignHold], and saying on told, when it is
// given one, that it holds it.
func commitForeign(ctx context.Context, db *store.DB, hold time.Duration,
	told chan<- struct{}) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO bench_foreign (payload) VALUES (?)`, "phase"); err != nil {
			return err
		}
		if told != nil {
			told <- struct{}{}
		}
		time.Sleep(hold)
		return nil
	})
}

// applyShaped is an apply transaction's body in the order the applier's is:
// the deferral probe's read first, then the rows.
func applyShaped(ctx context.Context, tx *sql.Tx, items []benchItem) error {
	if err := probeFirst(ctx, tx); err != nil {
		return err
	}
	return insertUnprepared(ctx, tx, items)
}

// probeFirst stands in for the deferral probe: a read of the applier's own
// tables before anything is written, which is what gives a per-file conflict
// a window to abort through.
func probeFirst(ctx context.Context, tx *sql.Tx) error {
	var n int
	return tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bench_items WHERE project = ?`, "proj-00").Scan(&n)
}

// settleForeign waits until the concurrent writer has committed at least once,
// so a test that staged no contention says so rather than passing.
func settleForeign(t *testing.T, foreign *atomic.Int64) {
	t.Helper()
	settleAfter(t, foreign, 0)
}

// settleAfter waits until the concurrent writer has committed past mark.
func settleAfter(t *testing.T, foreign *atomic.Int64, mark int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if foreign.Load() > mark {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the concurrent writer never committed past %d, so this run "+
		"staged no contention and its answer would be about an idle store", mark)
}

// THE SHIPPED SHAPE COSTS ceil(rows/chunk) STATEMENTS AND NOT ONE PER ROW, and
// the statement COUNT is the assertion, not a wall-clock comparison: arms timed
// in sequence see different machines (parallel tests, one saturating the same
// database), so no margin survives. The count is arithmetic, identical under
// load and the detector: 8 000 rows are 8 statements through [store.InsertRows].
// The arms are still timed and the rates LOGGED, since the drain rate is what
// every read-latency bound derives from, but never asserted: a magnitude
// belongs to whoever runs BenchmarkLogApplyDrain on the hardware they size.
func TestTheMultiRowApplyIssuesOneStatementPerChunk(t *testing.T) {
	if testing.Short() {
		t.Skip("times three apply transactions of 4 000 rows each")
	}
	t.Parallel()
	db, w := benchWriterT(t)
	items := benchItems(benchRows)
	ctx := t.Context()

	limit := db.Caps().MaxVariables
	perItem := store.RowsPerInsert(limit, itemColumns)
	perEdge := store.RowsPerInsert(limit, edgeColumns)
	wantMulti := chunksFor(len(items), perItem) + chunksFor(len(items), perEdge)
	wantPerRow := 2 * len(items)

	timeArm := func(run func() error) time.Duration {
		t.Helper()
		resetBenchT(ctx, t, w)
		started := time.Now()
		if err := run(); err != nil {
			t.Fatalf("apply: %v", err)
		}
		return time.Since(started)
	}

	var perRow, multi stmtCount
	unprepared := timeArm(func() error { return applyUnpreparedThrough(ctx, w, items, &perRow) })
	prepared := timeArm(func() error { return applyPrepared(ctx, w, db, items) })
	multirow := timeArm(func() error { return applyMultiRowThrough(ctx, w, db, items, &multi) })

	rows := float64(2 * len(items))
	t.Logf("4 000-record apply (%.0f rows): unprepared %v (%.0f rows/s), "+
		"prepared %v (%.0f rows/s), multirow %v (%.0f rows/s)",
		rows, unprepared, rows/unprepared.Seconds(),
		prepared, rows/prepared.Seconds(),
		multirow, rows/multirow.Seconds())
	t.Logf("statements: per-row %d, multirow %d (%d items/stmt, %d edges/stmt "+
		"at a probed limit of %d)", perRow.n, multi.n, perItem, perEdge, limit)

	if perRow.n != wantPerRow {
		t.Errorf("the per-row arm issued %d statements for %d rows, want %d — "+
			"it is the control, and it is supposed to be one statement per row",
			perRow.n, wantPerRow, wantPerRow)
	}
	if multi.n != wantMulti {
		t.Errorf("the multi-row arm issued %d statements for %d rows, want %d "+
			"(%d item chunks of %d + %d edge chunks of %d at a probed limit of %d) "+
			"— the chunker has stopped chunking, and every read-latency bound in "+
			"the design is derived from this number",
			multi.n, wantPerRow, wantMulti,
			chunksFor(len(items), perItem), perItem,
			chunksFor(len(items), perEdge), perEdge, limit)
	}
	// A FLOOR ON THE RATIO, not a margin on the clock. Anything above one row
	// per statement is the chunker working; this is what catches a probe that
	// collapsed to a limit too small to batch, which is the one way the counts
	// above could both be "right" and the shape still be worthless.
	if got := float64(wantPerRow) / float64(multi.n); got < 10 {
		t.Errorf("multi-row inserts collapsed %d rows into only %d statements "+
			"(%.1f× fewer) — at a probed limit of %d the chunker is batching "+
			"%d items and %d edges per statement, which is not enough to pay for "+
			"the shape", wantPerRow, multi.n, got, limit, perItem, perEdge)
	}
}

// chunksFor is how many statements n rows become at size per statement.
func chunksFor(n, size int) int {
	if n <= 0 || size <= 0 {
		return 0
	}
	return (n + size - 1) / size
}

// ---- the three arms --------------------------------------------------- //

func applyUnprepared(ctx context.Context, w *store.Writer, _ *store.DB, items []benchItem) error {
	return w.Tx(ctx, func(tx *sql.Tx) error { return insertUnprepared(ctx, tx, items) })
}

// applyUnpreparedThrough is the same arm with a seam for the statement counter.
func applyUnpreparedThrough(ctx context.Context, w *store.Writer, items []benchItem,
	count *stmtCount) error {

	return w.Tx(ctx, func(tx *sql.Tx) error {
		return insertUnprepared(ctx, count.wrap(tx), items)
	})
}

func insertUnprepared(ctx context.Context, tx store.Execer, items []benchItem) error {
	for _, it := range items {
		if _, err := tx.ExecContext(ctx, itemInsert,
			it.id, it.project, it.title, it.status, it.assignee, it.version, it.updated); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, edgeInsert, it.id, "blocks", it.other); err != nil {
			return err
		}
	}
	return nil
}

func applyPrepared(ctx context.Context, w *store.Writer, _ *store.DB, items []benchItem) error {
	// Prepared on the PINNED connection, which is what makes the handle
	// reusable: database/sql prepares on one connection, and through the
	// pool it would be re-prepared on whichever answered.
	item, err := w.Conn().PrepareContext(ctx, itemInsert)
	if err != nil {
		return err
	}
	defer func() { _ = item.Close() }()
	edge, err := w.Conn().PrepareContext(ctx, edgeInsert)
	if err != nil {
		return err
	}
	defer func() { _ = edge.Close() }()
	return w.Tx(ctx, func(tx *sql.Tx) error {
		itemStmt, edgeStmt := tx.StmtContext(ctx, item), tx.StmtContext(ctx, edge)
		for _, it := range items {
			if _, err := itemStmt.ExecContext(ctx,
				it.id, it.project, it.title, it.status, it.assignee, it.version, it.updated); err != nil {
				return err
			}
			if _, err := edgeStmt.ExecContext(ctx, it.id, "blocks", it.other); err != nil {
				return err
			}
		}
		return nil
	})
}

// applyPreparedMultiRow is THE SHIPPED SHAPE, and it goes through
// [store.InsertRows] rather than expanding the statement itself.
//
// It used to carry its own expander. That made the benchmark a SECOND
// implementation of the chunker's rule — and for as long as the appliers wrote
// one row per statement, it was the ONLY implementation that ran, so the arm
// this file calls "shipped" measured code the engine did not execute. Measuring
// the real function is the whole point: a change to InsertRows now moves this
// number, which is what makes the benchmark a record of the engine rather than
// of the benchmark.
func applyPreparedMultiRow(ctx context.Context, w *store.Writer, db *store.DB, items []benchItem) error {
	return applyMultiRowThrough(ctx, w, db, items, nil)
}

// applyMultiRowThrough is the arm with a seam for the statement counter.
func applyMultiRowThrough(ctx context.Context, w *store.Writer, db *store.DB,
	items []benchItem, count *stmtCount) error {

	limit := db.Caps().MaxVariables
	return w.Tx(ctx, func(tx *sql.Tx) error {
		ex := count.wrap(tx)
		if _, err := store.InsertRows(ctx, ex, limit,
			itemPrefix, itemRow, itemSuffix, len(items), func(i int) []any {
				it := items[i]
				return []any{it.id, it.project, it.title, it.status, it.assignee, it.version, it.updated}
			}); err != nil {
			return err
		}
		_, err := store.InsertRows(ctx, ex, limit,
			edgePrefix, edgeRow, edgeSuffix, len(items), func(i int) []any {
				it := items[i]
				return []any{it.id, "blocks", it.other}
			})
		return err
	})
}

// stmtCount counts the statements an arm issues. A nil one wraps nothing, so
// the benchmark pays neither an allocation nor an indirection for it.
type stmtCount struct{ n int }

func (c *stmtCount) wrap(ex store.Execer) store.Execer {
	if c == nil {
		return ex
	}
	return countedExec{c: c, inner: ex}
}

type countedExec struct {
	c     *stmtCount
	inner store.Execer
}

func (e countedExec) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	e.c.n++
	return e.inner.ExecContext(ctx, q, args...)
}

// ---- the fixture ------------------------------------------------------ //

type benchItem struct {
	id, project, title, status, assignee, other string
	version, updated                            int64
}

func benchItems(n int) []benchItem {
	out := make([]benchItem, n)
	for i := range out {
		out[i] = benchItem{
			id:       fmt.Sprintf("item-%06d", i),
			project:  fmt.Sprintf("proj-%02d", i%16),
			title:    fmt.Sprintf("a title of ordinary length for item %d", i),
			status:   []string{"open", "in_progress", "done"}[i%3],
			assignee: fmt.Sprintf("handle-%02d", i%24),
			other:    fmt.Sprintf("item-%06d", (i+1)%n),
			version:  int64(i + 1),
			updated:  int64(1767225600 + i),
		}
	}
	return out
}

func benchWriter(b *testing.B) (*store.DB, *store.Writer) {
	b.Helper()
	node, w := benchNode(b)
	return replicatedOf(b, node), w
}

func benchWriterT(t *testing.T) (*store.DB, *store.Writer) {
	t.Helper()
	node, w := benchNodeT(t)
	return replicatedOf(t, node), w
}

// benchNode returns the NODE handle, so an arm can reach either estate.
func benchNode(b *testing.B) (*store.DB, *store.Writer) {
	b.Helper()
	db, w := openApplierStore(b, filepath.Join(b.TempDir(), "drain.db"))
	b.Cleanup(func() { _ = w.Close() })
	b.Cleanup(func() { _ = db.Close() })
	return db, w
}

func benchNodeT(t *testing.T) (*store.DB, *store.Writer) {
	t.Helper()
	db, w := openApplierStore(t, filepath.Join(t.TempDir(), "drain.db"))
	t.Cleanup(func() { _ = w.Close() })
	t.Cleanup(func() { _ = db.Close() })
	return db, w
}

// scenarioBusyTimeout is the lock wait the back-to-back scenario runs under,
// and it is deliberately NOT the production default.
//
// The case holds ORDER; what bounds a wait (the busy timeout and the store's one
// retry) is certified by writequeue_internal_test.go. At the 5 s default the
// writer was refused whenever the 4 000-row apply outlasted two waits (12.1 s
// under the race detector on a shared runner), a wall-clock assertion in
// disguise. A minute is five times that, so a refusal here is the queue failing
// to hand over the lock. It does not catch a store that stopped queueing: that
// is the COUNT, [maxCommitsAhead].
const scenarioBusyTimeout = time.Minute

// benchNodeQueued is benchNodeT under scenarioBusyTimeout.
func benchNodeQueued(t *testing.T) (*store.DB, *store.Writer) {
	t.Helper()
	db, w := openApplierStoreWith(t, filepath.Join(t.TempDir(), "drain.db"),
		store.Options{BusyTimeout: scenarioBusyTimeout})
	t.Cleanup(func() { _ = w.Close() })
	t.Cleanup(func() { _ = db.Close() })
	return db, w
}

// openApplierStore opens a node with its replicated estate carrying one log —
// one pin — and the applier-shaped tables in that estate, which is where an
// applier writes.
func openApplierStore(tb testing.TB, path string) (*store.DB, *store.Writer) {
	tb.Helper()
	return openApplierStoreWith(tb, path, store.Options{})
}

// openApplierStoreWith is openApplierStore under the caller's options.
func openApplierStoreWith(tb testing.TB, path string, opts store.Options) (*store.DB, *store.Writer) {
	tb.Helper()
	ctx := tb.Context()
	db, rep := openReplicated(tb, path, opts, 1)
	if err := rep.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, applierTables)
		return err
	}); err != nil {
		tb.Fatalf("create the applier tables: %v", err)
	}
	w, err := rep.Writer(ctx)
	if err != nil {
		tb.Fatalf("pin a writer: %v", err)
	}
	return db, w
}

func resetBench(ctx context.Context, b *testing.B, w *store.Writer) {
	b.Helper()
	if err := clearBench(ctx, w); err != nil {
		b.Fatalf("reset: %v", err)
	}
}

func resetBenchT(ctx context.Context, t *testing.T, w *store.Writer) {
	t.Helper()
	if err := clearBench(ctx, w); err != nil {
		t.Fatalf("reset: %v", err)
	}
}

func clearBench(ctx context.Context, w *store.Writer) error {
	return w.Tx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{`DELETE FROM bench_edges`, `DELETE FROM bench_items`} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
}
