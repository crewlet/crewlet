package statelog_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// ---- what a restored log holds that the rows do not ------------------- //

// unheld walks the harness's log back from last, as a restored reanchor does.
func unheld(t *testing.T, h *applyHarness, last uint64) *statelog.TailRecord {
	t.Helper()
	got, err := statelog.UnheldTail(t.Context(), probeDomain{}, h.db.Replicated(), h.fetch,
		h.runner.Committed().Generation, 1, last)
	if err != nil {
		t.Fatalf("UnheldTail: %v", err)
	}
	return got
}

// barrier is a linearizable read's append at seq, which writes no row.
func barrier(h *applyHarness, seq uint64, storedAt time.Time) {
	e := env(seq, "barrier", "b", "", 1)
	e.Subject = statelog.Subject{Kind: statelog.BarrierKind, ID: "b"}
	h.fetch.offerStored(seq, storedAt, e)
}

// A RESTORED LOG WHOSE NEWEST RECORD THAT WRITES ROWS IS THIS NODE'S OWN HOLDS
// NOTHING WRITTEN AFTER THE RESTORE.
//
// Every record the node applied is named in its ledger by operation, position
// and broker instant, and the walk steps over barriers — a node whose rows are
// the copy's age goes on appending them for every linearizable read — to the
// newest record that writes rows.
func TestARestoredLogOfThisNodesOwnRecordsHoldsNothingUnheld(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	if got := unheld(t, h, 3); got != nil {
		t.Fatalf("the node's own history reads as holding %v it did not apply", got)
	}
	barrier(h, 4, otherHistory)
	barrier(h, 5, otherHistory.Add(time.Second))
	if got := unheld(t, h, 5); got != nil {
		t.Fatalf("barriers written after the restore read as unheld: %v", got)
	}
}

// A RECORD WRITTEN AFTER THE RESTORE IS NAMED — THE NEWEST THAT WRITES ROWS.
//
// Followed from its end, it would be applied on no node; the walk finds it
// beneath the barriers above it and names it whole, so the refusal can say
// what would be lost.
func TestARecordWrittenAfterTheRestoreIsNamed(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	written := env(4, "edit", "4", "op-after-the-restore", 1)
	written.Writer = "node-b"
	h.fetch.offerStored(4, otherHistory, written)
	barrier(h, 5, otherHistory.Add(time.Second))

	got := unheld(t, h, 5)
	if got == nil {
		t.Fatal("a record written after the restore reads as held — a reanchor " +
			"would apply it on no node and say nothing")
	}
	if got.Seq != 4 || got.OpID != "op-after-the-restore" || got.Writer != "node-b" ||
		got.Subject != "object.4" || !got.StoredAt.Equal(otherHistory) {
		t.Fatalf("the walk named %+v, want the record at 4 by node-b", got)
	}
}

// THE SAME OPERATION AT THE SAME POSITION IS NOT THE SAME RECORD.
//
// A caller retrying an operation the restore lost carries its id into a record
// written after the restore, and on a log reissuing sequences it can land at the
// very position the original held. The ledger names the original's broker
// instant, and only a record stored at that instant is the one these rows
// applied.
func TestTheSameOperationAtTheSamePositionIsNotTheSameRecord(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	h.fetch.offerStored(3, otherHistory, env(3, "edit", "3", "op-3", 1))
	got := unheld(t, h, 3)
	if got == nil || got.Seq != 3 {
		t.Fatalf("a retry of op-3 at sequence 3, stored after the restore, reads as "+
			"%v — the operation and the position agree and the record is another", got)
	}
}

// A LEDGER ROW THAT NAMES NO INSTANT VOUCHES FOR NOTHING.
//
// A row written before the ledger kept the instant says an operation applied at
// a position, which a retry at that position also satisfies; weighed as held,
// it would let a reanchor skip a record it cannot tell from its own.
func TestALedgerRowThatNamesNoInstantVouchesForNothing(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE probe_ops SET stored_at = 0`)
		return err
	}); err != nil {
		t.Fatalf("clear the ledger's instants: %v", err)
	}
	if got := unheld(t, h, 3); got == nil || got.Seq != 3 {
		t.Fatalf("a ledger row naming no instant read as holding the record: %v", got)
	}
}

// A RECORD THIS NODE RETAINED IS ITS HISTORY AS MUCH AS ONE IT APPLIED.
func TestARecordThisNodeRetainedIsHeld(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	h.fetch.offer(4, env(4, "edit", "4", "op-4", 2)) // a version this build cannot read
	if err := h.run(4); err != nil {
		t.Fatalf("run: %v", err)
	}
	if h.retainedCount() != 1 {
		t.Fatalf("%d record(s) retained, want the one", h.retainedCount())
	}
	if got := unheld(t, h, 4); got != nil {
		t.Fatalf("a record this node retained reads as unheld: %v", got)
	}
}

// A RECORD WHOSE ENVELOPE DOES NOT DECODE IS AN ERROR, NEVER A GUESS.
//
// Whether it writes rows cannot be told, so neither "held" nor "not held" is
// an answer the walk may give.
func TestARecordWhoseEnvelopeDoesNotDecodeIsAnError(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	h.fetch.mu.Lock()
	h.fetch.log[4] = statelog.Message{Seq: 4, StoredAt: otherHistory, Payload: []byte("{")}
	h.fetch.mu.Unlock()
	_, err := statelog.UnheldTail(t.Context(), probeDomain{}, h.db.Replicated(), h.fetch,
		h.runner.Committed().Generation, 1, 4)
	if err == nil || !strings.Contains(err.Error(), "does not decode") {
		t.Fatalf("UnheldTail over an undecodable record = %v, want an error", err)
	}
}

// A RESTORED REANCHOR OVER RECORDS THE ROWS DO NOT HOLD RUNS ONLY ON THE
// OPERATOR'S WORD — AND NO OTHER CASE ASKS FOR IT.
//
// Refused, it names the record and both ways out; with Discard the plan carries
// what it discards. A recreated log is followed from its first record and an
// abandoned one from the rows' own checkpoint, so neither skips anything, and a
// stray Unheld on either discards nothing and refuses nothing.
func TestARestoredReanchorOverUnheldRecordsRunsOnlyOnTheOperatorsWord(t *testing.T) {
	t.Parallel()
	written := &statelog.TailRecord{Seq: 7_100, Kind: "edit",
		Subject: "object.o", Writer: "node-b",
		OpID: "op-b-1", StoredAt: otherHistory}
	restored := reanchorInputs()
	restored.KeyedTo = reanchorCreated.Truncate(time.Microsecond)
	restored.FirstSeq, restored.LastSeq, restored.Position = 1, 7_100, 9_000
	restored.Unheld = written

	_, err := statelog.PermitReanchor(restored, confirmed())
	if !errors.Is(err, statelog.ErrReanchorRefused) {
		t.Fatalf("PermitReanchor over an unheld record = %v, want a refusal", err)
	}
	for _, want := range []string{"sequence 7100", "node-b", "op-b-1", "discard",
		"replace its rows"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if restored.Discarding() == nil {
		t.Fatal("the status would not say what the transition refuses")
	}

	guard := confirmed()
	guard.Discard = true
	plan, err := statelog.PermitReanchor(restored, guard)
	if err != nil {
		t.Fatalf("PermitReanchor with the operator's word: %v", err)
	}
	if plan.Case != statelog.ReanchorRestored || plan.Cursor != 7_100 ||
		plan.Discarded == nil || plan.Discarded.Seq != 7_100 {
		t.Fatalf("the plan is %+v, want the restored case at the end naming what "+
			"it discards", plan)
	}

	// FORCE IS NOT THE OPERATOR'S WORD FOR THIS.
	forced := confirmed()
	forced.Force = true
	if _, err := statelog.PermitReanchor(restored, forced); !errors.Is(err, statelog.ErrReanchorRefused) {
		t.Fatalf("PermitReanchor forced = %v — forcing past the most-caught-up rule "+
			"discarded writes", err)
	}

	// AND A RESTORED LOG HOLDING NOTHING UNHELD DISCARDS NOTHING.
	clean := restored
	clean.Unheld = nil
	if plan, err := statelog.PermitReanchor(clean, confirmed()); err != nil || plan.Discarded != nil {
		t.Fatalf("a restored log with nothing unheld = (%+v, %v)", plan, err)
	}

	for name, in := range map[string]statelog.ReanchorInputs{
		"recreated": func() statelog.ReanchorInputs {
			in := reanchorInputs()
			in.FirstSeq, in.LastSeq, in.Unheld = 42, 60, written
			return in
		}(),
		"abandoned": func() statelog.ReanchorInputs {
			in := restored
			in.LastSeq, in.Abandoned = in.Position+50, in.Generation+1
			return in
		}(),
	} {
		plan, err := statelog.PermitReanchor(in, confirmed())
		if err != nil || plan.Discarded != nil || in.Discarding() != nil {
			t.Errorf("the %s case with an unheld record = (%+v, %v) — it skips nothing, "+
				"so it asks nothing", name, plan, err)
		}
	}
}

// A RECORD THESE ROWS HOLD FROM A PEER'S SNAPSHOT IS HELD — THROUGH A REAL
// SNAPSHOT, TRANSFER AND ADOPTION.
//
// The operation ledger travels inside every snapshot, instant and all, so the
// row a donor wrote when it applied a record names that record on the node that
// adopted it exactly as it did on the donor: a restored reanchor there finds
// nothing written after the restore and runs. A donor on an older build scrubbed
// its ledger, and there the walk cannot vouch — but it says why, with the
// watermark the adoption wrote in place of the scrubbed rows, so the refusal
// can say the rows may hold the record after all.
func TestARecordHeldFromAPeersSnapshotIsHeld(t *testing.T) {
	t.Parallel()
	held := otherHistory.Add(-time.Hour).Truncate(time.Microsecond)
	at := statelog.Position{Stream: probeStream, Generation: 1, Seq: 4_200}
	// THE DONOR APPLIED THE RECORD AT ITS CHECKPOINT: its ledger names it by
	// operation, position and the broker's instant.
	seed := func(t *testing.T, db *store.DB) {
		t.Helper()
		if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `
				INSERT INTO probe_ops (op_id, subject, position, applied_at, stored_at)
				VALUES ('op-held', 'object.held', ?, 0, ?)`,
				at.Packed(), store.EncodeTime(held))
			return err
		}); err != nil {
			t.Fatalf("seed the donor's ledger: %v", err)
		}
	}
	log := newProbeFetch()
	log.offerStored(at.Seq, held, env(at.Seq, "edit", "held", "op-held", 1))
	walk := func(t *testing.T, h *joinHarness) *statelog.TailRecord {
		t.Helper()
		got, err := statelog.UnheldTail(t.Context(), probeDomain{}, h.joiner.Replicated(),
			log, at.Generation, 1, at.Seq)
		if err != nil {
			t.Fatalf("UnheldTail over the adopted estate: %v", err)
		}
		return got
	}

	t.Run("from a donor whose ledger travels", func(t *testing.T) {
		t.Parallel()
		h := newJoinHarnessFrom(t, joinDonor{domain: probeDomain{}, seed: seed})
		if _, err := h.adopter(t).Join(t.Context()); err != nil {
			t.Fatalf("Join: %v", err)
		}
		if got := walk(t, h); got != nil {
			t.Fatalf("the record the donor applied reads as unheld on the node that "+
				"adopted its snapshot: %v — a restored reanchor there refuses over "+
				"its own history", got)
		}
	})

	t.Run("from a donor on an older build, which scrubbed it", func(t *testing.T) {
		t.Parallel()
		h := newJoinHarnessFrom(t, joinDonor{domain: scrubbingProbe{}, seed: seed})
		if _, err := h.adopter(t).Join(t.Context()); err != nil {
			t.Fatalf("Join: %v", err)
		}
		got := walk(t, h)
		if got == nil || got.Seq != at.Seq {
			t.Fatalf("the walk over a scrubbed ledger answered %v — it cannot vouch "+
				"for a record whose row never arrived", got)
		}
		bound, lost := h.joinerLostBefore(t)
		if !lost || !got.LedgerLostBefore.Equal(bound) {
			t.Fatalf("the record names the ledger's watermark as %s, want the "+
				"adoption's %s (lost %v) — the refusal cannot say the rows may "+
				"hold it after all", got.LedgerLostBefore, bound, lost)
		}
	})
}

// A LEDGER'S SILENCE IS CONCLUSIVE ONLY FOR AN OPERATION MINTED SINCE IT LAST
// LOST A ROW.
//
// The walk names an unheld record with the ledger's watermark exactly when the
// record's operation was minted before it — the one case in which its row may
// have been swept, or scrubbed from the snapshot this node adopted, and the
// rows may hold the record after all. An operation id that carries no instant
// reads as minted before every loss, as the publisher reads it.
func TestALedgersSilenceIsConclusiveOnlySinceItLastLostARow(t *testing.T) {
	t.Parallel()
	minted := otherHistory.Add(-24 * time.Hour).Truncate(time.Millisecond)
	for _, c := range []struct {
		name  string
		opID  string
		lost  time.Time // zero: the ledger has lost nothing
		names bool
	}{
		{"a ledger that has lost nothing", statelog.NewOpID(minted, "edit"), time.Time{}, false},
		{"an operation minted after the loss", statelog.NewOpID(minted, "edit"),
			minted.Add(-time.Hour), false},
		{"an operation minted before the loss", statelog.NewOpID(minted, "edit"),
			minted.Add(time.Hour), true},
		{"an operation id with no instant", "op-after-the-restore",
			minted.Add(-time.Hour), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := appliedThrough(t, 3)
			if !c.lost.IsZero() {
				if err := statelog.RecordLedgerLoss(t.Context(), h.db.Replicated(),
					probeDomain{}, c.lost); err != nil {
					t.Fatalf("record the ledger's loss: %v", err)
				}
			}
			h.fetch.offerStored(4, otherHistory, env(4, "edit", "4", c.opID, 1))
			got := unheld(t, h, 4)
			if got == nil || got.Seq != 4 {
				t.Fatalf("the record written after the restore reads as %v", got)
			}
			switch {
			case c.names && !got.LedgerLostBefore.Equal(c.lost.UTC()):
				t.Fatalf("the record names the watermark %s, want %s — its row may "+
					"have been lost, and the refusal has to say so",
					got.LedgerLostBefore, c.lost)
			case !c.names && !got.LedgerLostBefore.IsZero():
				t.Fatalf("the record names the watermark %s, and the ledger's "+
					"silence about it is conclusive", got.LedgerLostBefore)
			}
		})
	}
}

// THE REFUSAL SAYS WHICH KIND OF "NOT HELD" IT IS.
//
// Where the ledger may have lost the record's row the refusal says the rows may
// hold it after all, naming how far back; where the ledger's silence is
// conclusive it says so, the snapshot a record came from no longer being a
// reason — only a gate that dropped it is.
func TestARestoredReanchorsRefusalSaysHowSureItIs(t *testing.T) {
	t.Parallel()
	lost := otherHistory.Add(time.Hour)
	for _, c := range []struct {
		name      string
		record    statelog.TailRecord
		want, not []string
	}{
		{"the ledger may have lost its row", statelog.TailRecord{Seq: 7_100, Kind: "edit",
			Subject: "object.o", OpID: "op-b-1", StoredAt: otherHistory, LedgerLostBefore: lost},
			[]string{"may have lost rows from before " + lost.Format(time.RFC3339Nano),
				"may hold the record after all"},
			[]string{"unless a gate dropped it"}},
		{"the ledger's silence is conclusive", statelog.TailRecord{Seq: 7_100, Kind: "edit",
			Subject: "object.o", OpID: "op-b-1", StoredAt: otherHistory},
			[]string{"held from a peer's snapshot included", "unless a gate dropped it"},
			[]string{"may have lost rows"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := reanchorInputs()
			in.KeyedTo = reanchorCreated.Truncate(time.Microsecond)
			in.FirstSeq, in.LastSeq, in.Position = 1, 7_100, 9_000
			in.Unheld = &c.record
			err := in.Discarding()
			if err == nil {
				t.Fatal("a restored log holding an unheld record refuses nothing")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
			for _, not := range c.not {
				if strings.Contains(err.Error(), not) {
					t.Errorf("the refusal %q says %q", err, not)
				}
			}
		})
	}
}
