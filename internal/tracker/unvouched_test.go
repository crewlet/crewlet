package tracker_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A RETRY OF A MOVE WHOSE LEDGER ROW WAS SWEPT IS UNKNOWN, NEVER REFUSED AS
// SOMEBODY ELSE'S.
//
// A move re-run finds its root already in the target and asks the ledger
// whether THIS operation put it there: the root step's row answers it, and a
// decision that runs at all means somebody else did. But a ledger that lost
// the row — its retention sweep, or an adoption from a donor that scrubbed its
// ledger — cannot say that. The refusal was returned before the ledger's
// watermark was ever asked, so the retry was told "already in OPS, and no
// record of this move putting it there" about the project its own first copy
// moved it into.
func TestARetryOfAMoveTheLedgerLostIsUnknownRatherThanRefused(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid")
	minted := time.Now().Add(-time.Hour)
	op := statelog.NewOpID(minted, "move")
	if _, err := r.writer.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil); err != nil {
		t.Fatalf("the move: %v", err)
	}
	r.drain()

	r.sweepLedger(minted.Add(time.Minute))
	end := r.logEnd(t)
	retry, err := r.writer.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil)
	if !errors.Is(err, tracker.ErrStepUnresolved) || !strings.Contains(err.Error(), op) {
		t.Fatalf("a retry of a move whose ledger row was swept = (%q, %v), want "+
			"an error wrapping ErrStepUnresolved naming operation %s — the "+
			"ledger that would say whether this operation moved the root lost "+
			"its row, so \"somebody else moved it\" is its silence read as an "+
			"answer", retry.Outcome, err, op)
	}
	if retry.Outcome != statelog.OutcomeUnknown || retry.OpID == "" {
		t.Errorf("the retry answered %q under %q, want unknown under the root "+
			"step's id", retry.Outcome, retry.OpID)
	}
	if got := r.logEnd(t); got != end {
		t.Fatalf("the retry put %d record(s) on the log", got-end)
	}
}

// A RETRIED CREATE WHOSE TASK STEP THE LEDGER CANNOT VOUCH FOR FILES NOTHING.
//
// The counter step landed and its row answers it, so the retry resumes and
// asks whether the task step applied. Where the ledger may have lost that row
// its silence says nothing, and the retry used to read it as "not applied":
// it minted a fresh number — a counter record, and a key nobody holds — and
// only then was stopped by the gate in front of the task's own publish. Nor is
// the silence a landing: answered as one, the retry reported a task "filed by
// an earlier copy of this operation" that nothing here says exists.
func TestARetriedCreateTheLedgerCannotVouchForFilesNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	lossy, log := r.lossyWriter(t)
	minted := time.Now().Add(-time.Hour)
	op := statelog.NewOpID(minted, "create")

	log.refuse("t-stuck")
	if _, err := lossy.CreateTask(t.Context(), op, newTask("t-stuck"), nil); err == nil {
		t.Fatal("the premise: a create whose task step is refused reported success")
	}
	log.refuse("")
	r.drain()
	// A LOSS THE OPERATION PREDATES that took none of these rows — they
	// were applied after its cutoff — so the counter step's row still
	// answers it and the task step, which never applied, has none.
	r.markLedgerLost(minted.Add(time.Minute))
	end := r.logEnd(t)

	retry, err := lossy.CreateTask(t.Context(), op, newTask("t-stuck"), nil)
	if err != nil || retry.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("the retry = (%q, %v), want unknown — the ledger cannot say "+
			"whether the task step applied", retry.Outcome, err)
	}
	for _, warning := range retry.Warnings {
		if strings.Contains(warning, "filed by an earlier copy") {
			t.Errorf("the retry claims a landing nothing vouches for: %s", warning)
		}
	}
	if got := r.logEnd(t); got != end {
		t.Fatalf("the retry put %d record(s) on the log — a number minted for "+
			"a create this node cannot decide", got-end)
	}
}

// sweepLedger is what the operation ledger's retention sweep does to this
// node: every row goes, and the watermark records that rows applied before
// cutoff may have.
func (r *roundTrip) sweepLedger(cutoff time.Time) {
	r.t.Helper()
	if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `DELETE FROM tracker_ops`)
		return err
	}); err != nil {
		r.t.Fatalf("sweep the operation ledger: %v", err)
	}
	r.markLedgerLost(cutoff)
}

// markLedgerLost moves the operation ledger's watermark to cutoff and deletes
// nothing — a loss whose rows were not these.
func (r *roundTrip) markLedgerLost(cutoff time.Time) {
	r.t.Helper()
	if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO statelog_ops_lost (ops_table, lost_before) VALUES (?, ?)
			ON CONFLICT (ops_table) DO UPDATE SET
				lost_before = MAX(lost_before, excluded.lost_before)`,
			tracker.Domain{}.OpsTable(), store.EncodeTime(cutoff))
		return err
	}); err != nil {
		r.t.Fatalf("move the operation ledger's watermark: %v", err)
	}
}
