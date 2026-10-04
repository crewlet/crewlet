package statelog_test

import (
	"database/sql"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// appliedVersion is the probe log's checkpoint row's applied record version,
// and false where the row holds none — unknown.
func appliedVersion(t *testing.T, h *applyHarness) (int64, bool) {
	t.Helper()
	var v sql.NullInt64
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT applied_version FROM statelog_cursor WHERE stream = ?`,
			probeStream).Scan(&v)
	}); err != nil {
		t.Fatalf("read the checkpoint row: %v", err)
	}
	return v.Int64, v.Valid
}

// THE CHECKPOINT KEEPS THE HIGHEST RECORD VERSION ITS ROWS WERE APPLIED FROM —
// and not one a record it could not read, or one a gate dropped, never wrote.
//
// A snapshot states it, and a joiner whose build reads less refuses the
// artefact: its rows would sit past a record the joiner could never apply. So
// it must rise with every record applied — the live loop's, and a later build's
// reprocess of what an earlier one retained, which moves no checkpoint — and
// with nothing else, or a rolling upgrade's older nodes refuse every upgraded
// donor over records that do not exist.
func TestTheCheckpointKeepsTheRecordVersionItsRowsWereAppliedFrom(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	// ABOVE THIS BUILD, so retained: in no row yet.
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 9))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}
	if v, known := appliedVersion(t, h); !known || v != 1 {
		t.Fatalf("the checkpoint says its rows were applied from version %d "+
			"(known %v), want 1 — the record at 2 was retained, not applied", v, known)
	}

	// THE UPGRADE applies it, without moving the checkpoint.
	h.upgrade(upgradedDomain{reads: 9})
	if err := h.boot(0); err != nil {
		t.Fatalf("the upgraded build's boot: %v", err)
	}
	if v, known := appliedVersion(t, h); !known || v != 9 {
		t.Fatalf("after the reprocess the checkpoint says version %d (known %v), "+
			"want 9 — the rows now hold a record only a build reading 9 can apply",
			v, known)
	}
}

// A GATED RECORD WROTE NO ROW, SO IT RAISES NOTHING.
func TestARecordAGateDroppedRaisesNoAppliedVersion(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, upgradedDomain{reads: 4})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	// A VERSION THIS BUILD READS, whose writer the domain's gate holds:
	// consumed, and written into no row.
	h.applier.mu.Lock()
	h.applier.gate, h.applier.gated[2] = statelog.ReasonEvicted, true
	h.applier.mu.Unlock()
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 4))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}
	if v, known := appliedVersion(t, h); !known || v != 1 {
		t.Fatalf("the checkpoint says version %d (known %v), want 1 — the record "+
			"at version 4 was gated and wrote nothing", v, known)
	}
}

// A ROW NOTHING RECORDED STAYS UNKNOWN.
//
// A checkpoint from before the rows kept this held rows nothing recorded, and
// a maximum over the records applied since is no bound on the ones before: a
// row that took a later record's version as its own would let a joiner reading
// less adopt rows applied from more.
func TestAnAppliedVersionNobodyRecordedStaysUnknown(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	if err := h.run(1); err != nil {
		t.Fatalf("run: %v", err)
	}
	// THE ROW AS MIGRATION 0035 LEFT EVERY CHECKPOINT THAT PREDATES IT.
	if err := h.estate.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE statelog_cursor SET applied_version = NULL`)
		return err
	}); err != nil {
		t.Fatalf("forget the applied version: %v", err)
	}
	h.upgrade(probeDomain{})
	h.fetch.offer(2, env(2, "edit", "b", "op-2", 1))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}
	if v, known := appliedVersion(t, h); known {
		t.Fatalf("the checkpoint says version %d after a record applied over a row "+
			"nothing recorded, want unknown", v)
	}
}

// A SNAPSHOT STATES WHAT ITS ROWS HOLD, NOT WHAT ITS DONOR'S BUILD COULD READ —
// and the build's own bound only where the file cannot say.
func TestASnapshotStatesTheRecordVersionItsRowsHold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		applied any
		want    int
	}{
		{"rows applied from nothing newer than version 0", int64(0), 0},
		{"a checkpoint nothing recorded", nil, probeDomain{}.RecordVersion()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSnapHarness(t)
			if err := h.estate.Tx(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(),
					`UPDATE statelog_cursor SET applied_version = ?`, tc.applied)
				return err
			}); err != nil {
				t.Fatalf("stage the applied version: %v", err)
			}
			m, err := h.snap.Take(t.Context())
			if err != nil {
				t.Fatalf("Take: %v", err)
			}
			if got := m.Domains["probe"].RecordVersion; got != tc.want {
				t.Fatalf("the manifest states record version %d, want %d", got, tc.want)
			}
		})
	}
}
