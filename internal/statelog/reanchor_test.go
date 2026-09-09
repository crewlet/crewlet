package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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
