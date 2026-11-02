package statelog

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// EVERY ANCHOR STATEMENT SEEKS ITS KEY — the plan gate behind a write's
// snapshot costing one seek however many subjects its log has written.
//
// Every write a domain decides reads its subject's anchor, and the table has
// two indexes that both lead with the stream: its primary key (stream,
// subject) and the sweep's (stream, anchor). The pinned driver's planner,
// with no statistics to go by, chose the sweep's for the exact-key read on
// the stream alone and walked every anchor of the stream for the one it
// wanted, so a write cost more the more subjects its log had written inside
// the trim's window — a reconcile creating ten thousand repositories got
// slower with every one it created. So each statement is explained over a
// table holding two logs' anchors, and each must SEARCH its own index on
// every column that index has for it: the read on the stream AND the subject,
// the sweep and its count on the stream AND the anchor's range.
//
// The plan rather than a wall-clock ratio, because the plan is what decides
// the cost and it is the same at any size: the walk it caused shows at a
// production table, and the plan that causes it shows at any.
//
// Mutation: drop anchorSQL's INDEXED BY and the read searches the sweep's
// index on the stream alone.
func TestEveryAnchorStatementSeeksItsKey(t *testing.T) {
	t.Parallel()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	w, err := estate.Writer(t.Context())
	if err != nil {
		t.Fatalf("pin a writer: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	const stream, other = "CREWLET_PLAN_LOG", "CREWLET_PLAN_OTHER_LOG"
	logs := []tables{{stream: stream, prefix: "crewlet.plan.log"}, {stream: other, prefix: "crewlet.plan.other"}}
	if err := w.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range 2_000 {
			for _, l := range logs {
				subject := fmt.Sprintf("%s.task.%05d", l.prefix, i)
				if err := l.advanceAnchor(t.Context(), tx, subject,
					Position{Stream: l.stream, Generation: 1, Seq: uint64(i + 1)}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the anchors: %v", err)
	}

	floor := Position{Stream: stream, Generation: 1, Seq: 1_000}.Packed()
	for _, tc := range []struct {
		name, statement, seek string
		args                  []any
	}{
		{"a subject's anchor", anchorSQL,
			"INDEX sqlite_autoindex_statelog_anchor_1 (stream=? AND subject=?)",
			[]any{stream, "crewlet.plan.log.task.01234"}},
		{"the sweep", sweepAnchorsSQL,
			"INDEX statelog_anchor_swept_idx (stream=? AND anchor<?)",
			[]any{stream, floor, AnchorsPerTrimmedRecord}},
		{"the count below a floor", anchorsBelowSQL,
			"INDEX statelog_anchor_swept_idx (stream=? AND anchor<?)",
			[]any{stream, floor, 100}},
	} {
		plan := storetest.Plan(t, writerPlans{w}, tc.statement, tc.args)
		seeks := false
		for _, step := range plan {
			if strings.Contains(step, "SEARCH") && strings.Contains(step, tc.seek) {
				seeks = true
			}
			if strings.Contains(step, "SCAN statelog_anchor") {
				t.Errorf("%s scans the table:\n%s", tc.name, strings.Join(plan, "\n"))
			}
		}
		if !seeks {
			t.Errorf("%s does not seek its key %s:\n%s", tc.name, tc.seek, strings.Join(plan, "\n"))
		}
	}
}

// BenchmarkReadingASubjectsAnchor is one write's read of its subject's anchor
// ([tables.anchor]) in a read transaction of its own, at two table sizes —
// twenty thousand anchors on the read log, about what ten thousand
// repositories' creates leave on the code log, and three hundred thousand —
// with as many of another log's beside them, as the logs share the table:
//
//	go test ./internal/statelog -run '^$' -bench BenchmarkReadingASubjectsAnchor
//
// It is the number the plan gate above is about. Measured on a 4-core
// 2.1 GHz Xeon: by the key, 0.16 ms a read at twenty thousand and 0.09 ms at
// three hundred thousand — a read transaction's own cost, the same at both
// sizes; walking the stream's anchors, as the planner chose to unnamed,
// 4.0 ms and 84 ms.
func BenchmarkReadingASubjectsAnchor(b *testing.B) {
	for _, anchors := range []int{20_000, 300_000} {
		b.Run(fmt.Sprintf("anchors=%d", anchors), func(b *testing.B) {
			const stream, other = "CREWLET_BENCH_LOG", "CREWLET_BENCH_OTHER_LOG"
			db, estate := storetest.OpenEstate(b, filepath.Join(b.TempDir(), "node.db"), store.Options{}, 1)
			b.Cleanup(func() { _ = db.Close() })
			w, err := estate.Writer(b.Context())
			if err != nil {
				b.Fatalf("pin a writer: %v", err)
			}
			b.Cleanup(func() { _ = w.Close() })
			read := tables{stream: stream, prefix: "crewlet.bench.log"}
			beside := tables{stream: other, prefix: "crewlet.bench.other"}
			subject := func(i int) string { return fmt.Sprintf("%s.repo.%07d", read.prefix, i) }
			const perTx = 10_000
			for start := 0; start < anchors; start += perTx {
				if err := w.Tx(b.Context(), func(tx *sql.Tx) error {
					for i := start; i < min(start+perTx, anchors); i++ {
						at := Position{Stream: stream, Generation: 1, Seq: uint64(i + 1)}
						if err := read.advanceAnchor(b.Context(), tx, subject(i), at); err != nil {
							return err
						}
						if err := beside.advanceAnchor(b.Context(), tx,
							fmt.Sprintf("%s.page.%07d", beside.prefix, i),
							Position{Stream: other, Generation: 1, Seq: uint64(i + 1)}); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					b.Fatalf("seed the anchors: %v", err)
				}
			}
			i := 0
			for b.Loop() {
				i = (i + 7_919) % anchors
				if err := estate.Read(b.Context(), func(tx *sql.Tx) error {
					got, err := read.anchor(b.Context(), tx, subject(i), 1)
					if err == nil && got.Seq != uint64(i+1) {
						err = fmt.Errorf("read %s's anchor as %s, want seq %d", subject(i), got, i+1)
					}
					return err
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// writerPlans plans every statement in a transaction of the pinned writer's
// own — the sweep is a DELETE, and a read transaction refuses even to explain
// one — so [storetest.Plan] reads each through the connection the applier
// writes on.
type writerPlans struct{ w *store.Writer }

func (h writerPlans) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	return h.w.Tx(ctx, fn)
}

func (h writerPlans) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return h.w.Tx(ctx, fn)
}
