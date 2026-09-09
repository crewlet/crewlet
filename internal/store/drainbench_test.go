package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
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
	itemInsert = `INSERT INTO bench_items
		(id, project, title, status, assignee, version, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project = excluded.project, title = excluded.title,
			status = excluded.status, assignee = excluded.assignee,
			version = excluded.version, updated_at = excluded.updated_at
		WHERE excluded.version > bench_items.version`
	edgeInsert = `INSERT INTO bench_edges (item_id, kind, other) VALUES (?, ?, ?)
		ON CONFLICT(item_id, kind, other) DO NOTHING`
)

// BenchmarkLogApplyDrain measures the applier's drain in rows per second,
// across the three statement shapes it could be written in.
//
// # Why this benchmark exists
//
// The drain rate is not a curiosity: it is the number a fleet's read latency
// is derived from. Every node applies the same records serially, so a bulk
// edit occupies every peer's applier for rows ÷ drain seconds — and during
// that window a linearizable read is behind and a write is pending. Halve
// this number and that window halves with it.
//
// # The three arms
//
//   - unprepared: one ExecContext per row, which is what the projection
//     writer this replaces does. The driver is handed SQL text per row and
//     parses and plans it before writing anything.
//   - prepared: the same statements prepared once on the pinned connection
//     and bound per row. The parse happens once for the whole batch — and
//     MEASURED, it buys nothing: this driver implements ExecerContext, so the
//     unprepared arm is already one round trip with its arguments and the
//     parse is not where the time goes. The arm stays because that result is
//     the reason the engine ships no statement cache.
//   - prepared+multirow: the same, with the child rows written as multi-row
//     INSERTs chunked to the probed bind-variable limit.
//
// Run it and read the rows/s: `go test ./internal/store -run xxx -bench
// LogApplyDrain -benchtime 3x`.
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
				resetBench(b, w, ctx)
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

// BenchmarkApplyTxUnderForeignCommits answers the question the applier's
// occupancy model rests on: what a long write transaction costs when other
// commits are happening beside it, and whether those commits ABORT it.
//
// # Why it has to be measured
//
// The store's transactions are OPTIMISTIC — a write transaction can be
// aborted at commit — and there are two possible granularities. Under
// row-level detection the applier never collides with the audit log, the
// diary or the config revisions, and its retry loop is dormant. Under
// database-level detection, every event insert that commits during a
// multi-second apply aborts it, eight times, and then the batch fails: the
// applier would stop committing exactly when the fleet is busiest, and a
// drain measured on an idle store would say nothing about it.
//
// The store's own TestTxRetriesAConflictedReadThenWrite cannot tell the two
// apart, because a same-row conflict is a conflict under both.
//
// # The two arms, and what they are for
//
//   - same-file: the foreign commits land in a table in the applier's OWN
//     database, which the applier never reads or writes. This is the
//     granularity question, asked directly.
//   - other-estate: the foreign commits land in the NODE estate, which is
//     where the audit log actually is. This is the shipped arrangement, and
//     the arm exists to price it against the one above.
//
// Measured (tursogo v0.8.0-pre.8, 4 vCPU): zero aborts per transaction in
// BOTH arms and zero refusals, so the granularity is not database-level — a
// commit to a table the applier never touches does not abort it, whichever
// file it is in.
//
// What the same-file arm shows instead is CONTENTION. Commits do land while
// an applier transaction is open, but under a continuously applying writer
// they land at a tiny fraction of the rate they manage against the other
// estate: 0.56/s against 1 676/s, three thousand times fewer. That is what the two-file split buys, and it is a throughput fact
// rather than a correctness one — which is exactly why the split is now
// unconditional rather than a response to this number.
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
			replicated := node.Replicated()
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
				resetBench(b, w, ctx)
				b.StartTimer()
				// COUNTED BY ATTEMPTS. Writer.Tx retries a stale
				// snapshot internally, so the abort count is how many
				// times fn ran beyond the first — the number that
				// decides whether the eight-attempt budget absorbs
				// this load or is spent by it.
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
// applier's whole occupancy model rests on and the one this repository could
// not previously answer.
//
// It is a test as well as a benchmark because the answer is an INVARIANT
// rather than a magnitude: if a driver bump made this driver's conflict
// detection database-level, every apply under load would burn its eight
// attempts and fail the batch — and the symptom would be a fleet that stops
// applying exactly when it is busiest, with nothing naming the cause.
//
// SLOWING IS NOT ABORTING, and the two are what this separates. A writer
// sharing the applier's file still commits while the applier's transaction is
// open — a hundred or so times, in the runs this logs — but far more slowly
// than the same writer against the other estate, which the benchmark above
// prices. The slowdown is what the two-file split removes; a RETRY is what
// would have broken the design, and it does not happen.
func TestAForeignCommitDoesNotAbortAnApplierTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a 4 000-row apply against a concurrent writer")
	}
	t.Parallel()
	node, w := benchNodeT(t)
	ctx := t.Context()
	if err := node.Replicated().Tx(ctx, func(tx *sql.Tx) error {
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
			if err := node.Replicated().Tx(ctx, func(tx *sql.Tx) error {
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
		insertErr := insertUnprepared(ctx, tx, benchItems(benchRows))
		during = foreign.Load() - before
		return insertErr
	})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("the apply failed under a concurrent writer: %v", err)
	}
	if attempts != 1 {
		t.Errorf("the applier's transaction ran %d times against a writer that "+
			"committed %d times to a table it never touches: this driver aborts "+
			"a write transaction because of commits elsewhere in the file, and "+
			"every apply under load will burn its retry budget",
			attempts, foreign.Load())
	}
	t.Logf("%d foreign commit(s) in total, %d of them while the applier's "+
		"transaction was open; %d attempt(s)", foreign.Load(), during, attempts)
}

// settleForeign waits until the concurrent writer has committed at least once,
// so a test that staged no contention says so rather than passing.
func settleForeign(t *testing.T, foreign *atomic.Int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if foreign.Load() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the concurrent writer never committed, so this run staged no " +
		"contention and its answer would be about an idle store")
}

// THE SHIPPED SHAPE IS THE FASTEST OF THE THREE, and this is the assertion
// rather than a number in a comment: the arms are compared against each other
// in one run, on this machine, so the claim survives a faster laptop and a
// slower CI box alike.
//
// # What it does NOT assert, and why
//
// That preparing beats not preparing. Measured here, it does not: the two are
// within noise of each other and the ordering flips between runs, because
// this driver implements ExecerContext and an "unprepared" exec is already
// one round trip carrying its arguments. Asserting a 1 % ordering would be a
// test that fails on a busy machine and tells nobody anything. The delta is
// LOGGED instead, so the day a driver bump makes the parse matter, the number
// is in the output of a test that already runs.
//
// What IS asserted is the shape that pays fourfold and that every read-latency
// bound in the design is derived from.
//
// It is a test rather than a benchmark because the ordering is the invariant.
// The magnitudes belong to whoever runs the benchmark on the hardware they
// are sizing.
func TestTheMultiRowApplyIsTheFastestShape(t *testing.T) {
	if testing.Short() {
		t.Skip("times three apply transactions of 4 000 rows each")
	}
	t.Parallel()
	db, w := benchWriterT(t)
	items := benchItems(benchRows)
	ctx := t.Context()

	timeArm := func(run func(context.Context, *store.Writer, *store.DB, []benchItem) error) time.Duration {
		t.Helper()
		resetBenchT(t, w, ctx)
		started := time.Now()
		if err := run(ctx, w, db, items); err != nil {
			t.Fatalf("apply: %v", err)
		}
		return time.Since(started)
	}

	unprepared := timeArm(applyUnprepared)
	prepared := timeArm(applyPrepared)
	multirow := timeArm(applyPreparedMultiRow)
	rows := float64(2 * len(items))
	t.Logf("4 000-record apply (%.0f rows): unprepared %v (%.0f rows/s), "+
		"prepared %v (%.0f rows/s), multirow %v (%.0f rows/s)",
		rows, unprepared, rows/unprepared.Seconds(),
		prepared, rows/prepared.Seconds(),
		multirow, rows/multirow.Seconds())

	// A MARGIN RATHER THAN AN ORDERING. Measured at four times faster; half
	// of that is still unambiguous and leaves room for a loaded machine,
	// while anything at parity means the chunker has stopped chunking.
	if float64(multirow) >= float64(prepared)/1.5 {
		t.Errorf("multi-row inserts (%v) are not materially faster than one row "+
			"per statement (%v) — the chunker is buying nothing, and every "+
			"read-latency bound in the design is derived from this number",
			multirow, prepared)
	}
	if float64(multirow) >= float64(unprepared)/1.5 {
		t.Errorf("multi-row inserts (%v) are not materially faster than the "+
			"per-row execs the projection writer uses (%v)", multirow, unprepared)
	}
}

// ---- the three arms --------------------------------------------------- //

func applyUnprepared(ctx context.Context, w *store.Writer, _ *store.DB, items []benchItem) error {
	return w.Tx(ctx, func(tx *sql.Tx) error { return insertUnprepared(ctx, tx, items) })
}

func insertUnprepared(ctx context.Context, tx *sql.Tx, items []benchItem) error {
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

func applyPreparedMultiRow(ctx context.Context, w *store.Writer, db *store.DB, items []benchItem) error {
	limit := db.Caps().MaxVariables
	return w.Tx(ctx, func(tx *sql.Tx) error {
		for start, end := range store.Chunks(len(items), limit, itemColumns) {
			stmt, args := multiRow(itemInsert, itemColumns, end-start, func(i int) []any {
				it := items[start+i]
				return []any{it.id, it.project, it.title, it.status, it.assignee, it.version, it.updated}
			})
			if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
				return err
			}
		}
		for start, end := range store.Chunks(len(items), limit, edgeColumns) {
			stmt, args := multiRow(edgeInsert, edgeColumns, end-start, func(i int) []any {
				it := items[start+i]
				return []any{it.id, "blocks", it.other}
			})
			if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// multiRow expands a single-row INSERT into an n-row one, with the arguments
// flattened in row order.
func multiRow(single string, columns, rows int, args func(int) []any) (string, []any) {
	group := "(" + strings.TrimSuffix(strings.Repeat("?,", columns), ",") + ")"
	head := single[:strings.LastIndex(single, "VALUES")+len("VALUES")]
	var b strings.Builder
	b.WriteString(head)
	b.WriteByte(' ')
	for i := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(group)
	}
	out := make([]any, 0, rows*columns)
	for i := range rows {
		out = append(out, args(i)...)
	}
	return b.String(), out
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
	return node.Replicated(), w
}

func benchWriterT(t *testing.T) (*store.DB, *store.Writer) {
	t.Helper()
	node, w := benchNodeT(t)
	return node.Replicated(), w
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

// openApplierStore opens a node with one declared pin and the applier-shaped
// tables in its REPLICATED estate, which is where an applier writes.
func openApplierStore(tb testing.TB, path string) (*store.DB, *store.Writer) {
	tb.Helper()
	ctx := tb.Context()
	db, err := store.Open(ctx, path, store.Options{PinnedWriters: 1})
	if err != nil {
		tb.Fatalf("open: %v", err)
	}
	rep := db.Replicated()
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

func resetBench(b *testing.B, w *store.Writer, ctx context.Context) {
	b.Helper()
	if err := clearBench(ctx, w); err != nil {
		b.Fatalf("reset: %v", err)
	}
}

func resetBenchT(t *testing.T, w *store.Writer, ctx context.Context) {
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
