package statelog_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

var reanchorCreated = time.Unix(1_700_000_000, 0).UTC()

func reanchorInputs() statelog.ReanchorInputs {
	return statelog.ReanchorInputs{
		Stream:           probeStream,
		StreamCreatedAt:  reanchorCreated,
		FirstSeq:         0,
		PeersHydrated:    0,
		Position:         9_000,
		Highest:          9_000,
		RegisterReadable: true,
		Generation:       1,
	}
}

func confirmed() statelog.ReanchorGuard {
	return statelog.ReanchorGuard{Confirm: reanchorCreated.Format(time.RFC3339)}
}

// EVERY GUARD REFUSES FOR ITS OWN REASON, and each one names a mistake that
// cannot be undone.
func TestAReanchorRefusesEveryWayItCanBeWrong(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in    statelog.ReanchorInputs
		guard statelog.ReanchorGuard
		names string
	}{
		"no confirmation at all": {
			in: reanchorInputs(), guard: statelog.ReanchorGuard{},
			names: "no undo",
		},
		"a confirmation for another estate": {
			in:    reanchorInputs(),
			guard: statelog.ReanchorGuard{Confirm: "2020-01-01T00:00:00Z"},
			names: "wrong estate",
		},
		"a peer is hydrated on the live stream": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.PeersHydrated = 1
				return in
			}(),
			guard: confirmed(),
			names: "identity claim is violated",
		},
		"this is not the most caught-up node": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.Position = 4_000
				return in
			}(),
			guard: confirmed(),
			names: "what the reanchor discards",
		},
		"the register could not be read": {
			in: func() statelog.ReanchorInputs {
				in := reanchorInputs()
				in.RegisterReadable = false
				return in
			}(),
			guard: confirmed(),
			names: "force flag",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := statelog.PermitReanchor(tc.in, tc.guard)
			if !errors.Is(err, statelog.ErrReanchorRefused) {
				t.Fatalf("PermitReanchor = %v, want a refusal", err)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal does not say why: %v", err)
			}
		})
	}

	// A CLEAN ONE PROCEEDS, to one past the estate's own highest.
	gen, err := statelog.PermitReanchor(reanchorInputs(), confirmed())
	if err != nil {
		t.Fatalf("a clean reanchor was refused: %v", err)
	}
	if gen != 2 {
		t.Fatalf("the new generation is %d, want 2 — it is DERIVED LOCALLY from "+
			"the estate's own audit table, because this verb runs when the "+
			"broker estate is exactly what was lost", gen)
	}

	// AND A FORCE OVERRIDES THE POSITION RULE and nothing else: an
	// operator can know something the register does not say, and cannot
	// know that two hydrated peers will not diverge.
	behind := reanchorInputs()
	behind.Position = 4_000
	if _, err := statelog.PermitReanchor(behind, statelog.ReanchorGuard{
		Confirm: confirmed().Confirm, Force: true,
	}); err != nil {
		t.Fatalf("a forced reanchor was refused: %v", err)
	}
	hydrated := reanchorInputs()
	hydrated.PeersHydrated = 1
	if _, err := statelog.PermitReanchor(hydrated, statelog.ReanchorGuard{
		Confirm: confirmed().Confirm, Force: true,
	}); err == nil {
		t.Fatal("a forced reanchor ran with a hydrated peer — that is not a " +
			"judgement an operator can make, because the divergence it produces " +
			"is silent and there is no log left to reconcile from")
	}
}

// THE TRANSITION MOVES EVERY CURSOR AND THE AUDIT ROW IN ONE TRANSACTION.
//
// A crash between them would leave the fleet on a generation nothing recorded
// — so the re-run would derive the same number, and the cursors that DID move
// would be a generation ahead of the audit table that decides it.
func TestAReanchorMovesEveryCursorAndItsAuditRowTogether(t *testing.T) {
	t.Parallel()
	db := reanchorStore(t)
	var reset, published, recorded atomic.Int64

	gen, err := statelog.Reanchor(t.Context(), statelog.ReanchorDeps{
		Domains: map[string]statelog.Registered{"probe": {Domain: probeDomain{}}},
		DB:      db,
		ResetVersions: func(context.Context, uint32) error {
			reset.Add(1)
			return nil
		},
		PublishGeneration: func(context.Context, uint32, statelog.ReanchorInputs) error {
			published.Add(1)
			return nil
		},
		RecordGeneration: func(_ context.Context, _ *sql.Tx, _ uint32, _ statelog.ReanchorInputs) error {
			recorded.Add(1)
			return nil
		},
	}, reanchorInputs(), confirmed())
	if err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	if gen != 2 {
		t.Fatalf("reanchored to generation %d, want 2", gen)
	}
	if reset.Load() != 1 || published.Load() != 1 || recorded.Load() != 1 {
		t.Fatalf("reset=%d published=%d recorded=%d, want one of each",
			reset.Load(), published.Load(), recorded.Load())
	}

	var g, seq int64
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT generation, seq FROM statelog_cursor WHERE stream = ?`,
			probeStream).Scan(&g, &seq)
	}); err != nil {
		t.Fatalf("read the cursor back: %v", err)
	}
	if g != 2 || seq != 0 {
		t.Fatalf("the cursor is at generation %d sequence %d, want 2/0 — on a "+
			"fresh stream the new cursor is one below its first surviving "+
			"sequence, which is zero", g, seq)
	}
}

// A REANCHOR IS REPORTED ONCE, AND THE COMPLETION NAMES WHAT IT DISCARDED.
//
// Its lines went nowhere — the engine handed it no logger and an absent one
// was a discarding one — so the engine wrote its own `statelog_reanchored`
// beside the call. With both reaching the log one transition read as two, and
// the engine's copy is gone: this one is the record, so it carries what that
// one did. `prev_last_seq_seen` is the high-water mark of the history the
// reanchor walked away from, which is the one fact nothing after it can
// reconstruct.
//
// AND IT NAMES THE STREAM THAT NUMBER BELONGS TO. The engine's line carried
// the stream the operator named; this one carried only `streams`, every cursor
// the call moved — so with more than one domain registered, which is every
// production node, it paired one domain's high-water mark with a list of logs
// and nothing to say which. Two domains here, because one hides exactly that.
func TestAReanchorIsReportedOnceNamingWhatItDiscarded(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	db := reanchorStore(t)
	in := reanchorInputs()
	in.Highest = 9_000

	gen, err := statelog.Reanchor(t.Context(), statelog.ReanchorDeps{
		// KEYED BY DOMAIN NAME, as the engine keys it. The line names
		// streams from each domain's spec, and keyed by the stream this
		// fixture made the key and the spec the same string — so a
		// regression reading the keys instead passed.
		Domains: map[string]statelog.Registered{
			probeDomain{}.Name():       {Domain: probeDomain{}},
			secondProbeDomain{}.Name(): {Domain: secondProbeDomain{}},
		},
		DB:                db,
		ResetVersions:     func(context.Context, uint32) error { return nil },
		PublishGeneration: func(context.Context, uint32, statelog.ReanchorInputs) error { return nil },
		RecordGeneration: func(context.Context, *sql.Tx, uint32, statelog.ReanchorInputs) error {
			return nil
		},
		Logger: slog.New(slog.NewJSONHandler(&buf, nil)),
	}, in, confirmed())
	if err != nil {
		t.Fatalf("Reanchor: %v", err)
	}

	started := logRecords(t, buf.Bytes(), "statelog_reanchor_started")
	if len(started) != 1 {
		t.Fatalf("%d statelog_reanchor_started lines, want one", len(started))
	}
	done := logRecords(t, buf.Bytes(), "statelog_reanchored")
	if len(done) != 1 {
		t.Fatalf("%d statelog_reanchored lines, want one: %s", len(done), buf.String())
	}
	for key, want := range map[string]any{
		"generation":         float64(2),
		"cursor":             float64(0),
		"prev_last_seq_seen": float64(9_000),
		"stream":             probeStream,
	} {
		if done[0][key] != want {
			t.Errorf("statelog_reanchored %s = %v, want %v", key, done[0][key], want)
		}
	}
	if started[0]["stream"] != probeStream {
		t.Errorf("statelog_reanchor_started names stream %v, want %s — its creation "+
			"instant and position are that stream's", started[0]["stream"], probeStream)
	}

	// `streams` IS WHAT MOVED, read back from the cursors themselves rather
	// than restated: the line's promise is the logs whose positions this
	// call rewrote, and an operator reading it after the fact is asking
	// exactly that.
	var moved []any
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT stream FROM statelog_cursor WHERE generation = ? ORDER BY stream`, gen)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var stream string
			if err := rows.Scan(&stream); err != nil {
				return err
			}
			moved = append(moved, stream)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the cursors back: %v", err)
	}
	if got, _ := done[0]["streams"].([]any); len(moved) == 0 || !slices.Equal(got, moved) {
		t.Errorf("statelog_reanchored names streams %v, and the cursors that moved "+
			"are on %v", done[0]["streams"], moved)
	}
	if detail, _ := done[0]["detail"].(string); !strings.Contains(detail, "not recovered") {
		t.Errorf("statelog_reanchored does not say what is lost: %q", detail)
	}
}

// secondProbeDomain is a second domain on its own log, for the one case a
// single registered domain cannot show: which of several streams a line's
// numbers belong to.
type secondProbeDomain struct{ probeDomain }

func (secondProbeDomain) Name() string { return "second_probe" }

func (secondProbeDomain) Stream() statelog.StreamSpec {
	spec := probeDomain{}.Stream()
	spec.Name = "CREWLET_SECOND_PROBE_LOG"
	spec.Subjects = []string{"crewlet.secondprobe.log.>"}
	spec.SubjectPrefix = "crewlet.secondprobe.log"
	return spec
}

// A FAILED AUDIT ROW ROLLS THE CURSORS BACK WITH IT.
//
// The two commit together or not at all: cursors a generation ahead of the
// table that decides the generation is a fleet that reanchors to the same
// number twice.
func TestAReanchorWhoseAuditRowFailsMovesNoCursor(t *testing.T) {
	t.Parallel()
	db := reanchorStore(t)

	_, err := statelog.Reanchor(t.Context(), statelog.ReanchorDeps{
		Domains:       map[string]statelog.Registered{"probe": {Domain: probeDomain{}}},
		DB:            db,
		ResetVersions: func(context.Context, uint32) error { return nil },
		PublishGeneration: func(context.Context, uint32, statelog.ReanchorInputs) error {
			return nil
		},
		RecordGeneration: func(context.Context, *sql.Tx, uint32, statelog.ReanchorInputs) error {
			return errors.New("the audit table refused")
		},
	}, reanchorInputs(), confirmed())
	if err == nil {
		t.Fatal("a reanchor whose audit row failed reported success")
	}

	var rows int64
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM statelog_cursor`).Scan(&rows)
	}); err != nil {
		t.Fatalf("read the cursors: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d cursor row(s) survive a rolled-back reanchor — a cursor a "+
			"generation ahead of the table that decides the generation is a "+
			"fleet that reanchors to the same number twice", rows)
	}
}

// THE RESET RUNS BEFORE THE RECORD, AND THE RECORD BEFORE THE CURSORS.
//
// The order IS the crash matrix. A reset interrupted needs no repairer,
// because every row it did not reach is covered by the lazy rule; a record
// published without its cursors is collapsed by first-writer-wins on the
// re-run. Reversed, neither of those is true.
func TestTheReanchorsStepsRunInTheOrderItsCrashMatrixAssumes(t *testing.T) {
	t.Parallel()
	db := reanchorStore(t)
	var order []string

	if _, err := statelog.Reanchor(t.Context(), statelog.ReanchorDeps{
		Domains: map[string]statelog.Registered{"probe": {Domain: probeDomain{}}},
		DB:      db,
		ResetVersions: func(context.Context, uint32) error {
			order = append(order, "reset")
			return nil
		},
		PublishGeneration: func(context.Context, uint32, statelog.ReanchorInputs) error {
			order = append(order, "publish")
			return nil
		},
		RecordGeneration: func(context.Context, *sql.Tx, uint32, statelog.ReanchorInputs) error {
			order = append(order, "record")
			return nil
		},
	}, reanchorInputs(), confirmed()); err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	want := []string{"reset", "publish", "record"}
	if len(order) != len(want) {
		t.Fatalf("the steps ran as %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("the steps ran as %v, want %v — the order IS the crash "+
				"matrix, and reversed neither residue has a repairer", order, want)
		}
	}
}

// reanchorStore is a node with a replicated estate and no cursor yet.
func reanchorStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return db
}

// seedAnchor writes one subject's arbitration anchor at a generation.
func seedAnchor(ctx context.Context, t *testing.T, db *store.DB, subject string, gen uint32, seq uint64) {
	t.Helper()
	packed := int64(gen)*statelog.GenerationStride + int64(seq)
	if err := db.Replicated().Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO statelog_anchor (stream, subject, anchor) VALUES (?, ?, ?)
			ON CONFLICT (stream, subject) DO UPDATE SET anchor = excluded.anchor`,
			probeStream, subject, packed)
		return err
	}); err != nil {
		t.Fatalf("seed the anchor on %s: %v", subject, err)
	}
}

// seedCursor writes the domain's committed cursor at a generation, which is
// what a node that has been running has.
func seedCursor(ctx context.Context, t *testing.T, db *store.DB, gen uint32, seq uint64) {
	t.Helper()
	if err := db.Replicated().Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (stream) DO UPDATE SET
				generation = excluded.generation, seq = excluded.seq`,
			probeStream, gen, seq, reanchorCreated.UnixMilli(), reanchorCreated.UnixMilli())
		return err
	}); err != nil {
		t.Fatalf("seed the cursor: %v", err)
	}
}

// anchorGeneration reads one subject's anchor back as a generation.
func anchorGeneration(t *testing.T, db *store.DB, subject string) uint32 {
	t.Helper()
	var packed int64
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT anchor FROM statelog_anchor WHERE stream = ? AND subject = ?`,
			probeStream, subject).Scan(&packed)
	}); err != nil {
		t.Fatalf("read the anchor on %s back: %v", subject, err)
	}
	return uint32(packed / statelog.GenerationStride)
}

// cursorGeneration reads the domain's committed cursor back as a generation.
func cursorGeneration(t *testing.T, db *store.DB) uint32 {
	t.Helper()
	var gen int64
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT generation FROM statelog_cursor WHERE stream = ?`,
			probeStream).Scan(&gen)
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0
		}
		t.Fatalf("read the cursor back: %v", err)
	}
	return uint32(gen)
}

// AN INTERRUPTED REANCHOR IS FINISHED BY RE-RUNNING IT, AND NEEDS NO REPAIRER.
//
// Step 4 rewrites every anchor in bounded transactions because half a million
// of them cannot be one, and a process that dies partway leaves a table where
// some rows carry the new generation and some carry the old. That state is
// CORRECT rather than damaged, and the reason is contract 1's lazy rule: a row
// below the current generation is "no anchor at this generation", so its next
// write forms an expectation of zero and the broker arbitrates it against a
// subject that genuinely holds nothing.
//
// Both halves are asserted here — the re-run, and the rule that makes the
// residue harmless — because either one alone is an argument rather than a
// check.
func TestAReanchorIsResumable(t *testing.T) {
	t.Parallel()

	t.Run("a crash inside the reset is finished by re-running", func(t *testing.T) {
		t.Parallel()
		db := reanchorStore(t)
		seedCursor(t.Context(), t, db, 1, 9_000)
		seedAnchor(t.Context(), t, db, "probe.reached", 1, 500)
		seedAnchor(t.Context(), t, db, "probe.missed", 1, 600)

		crash := errors.New("the process died between two bounded transactions")
		var attempts, published, recorded atomic.Int64
		deps := statelog.ReanchorDeps{
			Domains: map[string]statelog.Registered{"probe": {Domain: probeDomain{}}},
			DB:      db,
			ResetVersions: func(ctx context.Context, gen uint32) error {
				if attempts.Add(1) == 1 {
					// One bounded transaction committed, then the
					// process died before the next. ON THE CALLER'S
					// OWN CONTEXT: the seed stands in for a write
					// the reset itself would have made.
					seedAnchor(ctx, t, db, "probe.reached", gen, 0)
					return crash
				}
				return nil
			},
			PublishGeneration: func(context.Context, uint32, statelog.ReanchorInputs) error {
				published.Add(1)
				return nil
			},
			RecordGeneration: func(context.Context, *sql.Tx, uint32, statelog.ReanchorInputs) error {
				recorded.Add(1)
				return nil
			},
		}

		if _, err := statelog.Reanchor(t.Context(), deps, reanchorInputs(), confirmed()); !errors.Is(err, crash) {
			t.Fatalf("Reanchor returned %v, want the crash — a failed reset must "+
				"not be reported as a completed transition", err)
		}
		// NOTHING PAST STEP 4 RAN. The record and the cursors are what
		// make the transition fleet-visible, and a crash inside the
		// reset must leave the fleet on the old generation.
		if published.Load() != 0 || recorded.Load() != 0 {
			t.Fatalf("a crash inside the reset still published=%d recorded=%d",
				published.Load(), recorded.Load())
		}
		if got := cursorGeneration(t, db); got != 1 {
			t.Fatalf("the cursor moved to generation %d during a reset that "+
				"crashed", got)
		}

		// THE RE-RUN DERIVES THE SAME GENERATION. It is derived from
		// the estate's own audit table, which the crash did not move —
		// so a second attempt is a retry rather than a second
		// transition, and the record it publishes is the one the first
		// attempt would have.
		gen, err := statelog.Reanchor(t.Context(), deps, reanchorInputs(), confirmed())
		if err != nil {
			t.Fatalf("the re-run failed: %v", err)
		}
		if gen != 2 {
			t.Fatalf("the re-run reanchored to generation %d, want the same 2 the "+
				"crashed attempt derived", gen)
		}
		if got := cursorGeneration(t, db); got != 2 {
			t.Fatalf("the cursor is at generation %d after a completed re-run", got)
		}
		if got := anchorGeneration(t, db, "probe.reached"); got != 2 {
			t.Errorf("the anchor the crashed reset DID reach is at generation %d", got)
		}
		// AND THE ONE IT DID NOT REACH IS STILL AT THE OLD GENERATION,
		// deliberately. Nothing repairs it, and the next subtest is why
		// that is safe.
		if got := anchorGeneration(t, db, "probe.missed"); got != 1 {
			t.Errorf("the anchor the reset never reached is at generation %d, "+
				"want 1 — the resumable design rests on that row being left "+
				"alone rather than on a repairer nobody wrote", got)
		}
	})

	t.Run("a row the reset never reached writes at an expectation of zero", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		subject := statelog.Subject{Kind: "task", ID: "left-behind"}
		// A durable row whose anchor is from BEFORE the reanchor: the
		// generation the reset would have raised, and did not.
		h.rows.stage(subject, statelog.Position{
			Stream: probeStream, Generation: 1, Seq: 500,
		})
		h.gen.Store(2)

		if _, err := h.write(subject, "op-after-reanchor", "hello"); err != nil {
			t.Fatalf("a write on a row the reset never reached failed: %v", err)
		}
		expects := h.appends.expectations()
		if len(expects) != 1 {
			t.Fatalf("the write took %d append(s), want one", len(expects))
		}
		if expects[0] == nil {
			t.Fatal("an arbitrated write formed no expectation at all")
		}
		if *expects[0] != 0 {
			t.Fatalf("the write formed an expectation of %d against a subject the "+
				"new stream has never held: an anchor below the current "+
				"generation names a sequence in a dead number space, and "+
				"publishing at it is refused for ever", *expects[0])
		}
	})
}
