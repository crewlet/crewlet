package worklet_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A RETRY OF A MOVE WHOSE LEDGER ROW WAS SWEPT IS UNKNOWN, NEVER REFUSED AS
// SOMEBODY ELSE'S.
//
// A re-run that finds its root already in the target asks the ledger whether
// THIS operation put it there; a ledger whose retention sweep took the row
// cannot say, so its watermark is asked before any refusal, or the retry is
// refused over the move its own first copy made.
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
	if !errors.Is(err, worklet.ErrStepUnresolved) || !strings.Contains(err.Error(), op) {
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
	// AND IT SAYS THIS NODE CANNOT VOUCH: the re-run an ordinary unknown
	// asks for stops at the same step here every time.
	if !errors.Is(err, worklet.ErrStepUnvouched) {
		t.Errorf("the retry's error does not wrap ErrStepUnvouched: %v", err)
	}
	if got := r.logEnd(t); got != end {
		t.Fatalf("the retry put %d record(s) on the log", got-end)
	}
}

// A RETRIED CREATE WHOSE TASK STEP THE LEDGER CANNOT VOUCH FOR FILES NOTHING.
//
// The counter step's row answers it, so the retry asks whether the task step
// applied. Where the ledger may have lost that row its silence is neither "not
// applied" (which would mint a counter record and a key nobody holds) nor a
// landing (which would report a task nothing here says exists).
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
	// A LOSS THE OPERATION PREDATES that took none of these rows (applied
	// after its cutoff): the counter step's row still answers, and the task
	// step, which never applied, has none.
	r.markLedgerLost(minted.Add(time.Minute))
	end := r.logEnd(t)

	retry, err := lossy.CreateTask(t.Context(), op, newTask("t-stuck"), nil)
	if err != nil || retry.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("the retry = (%q, %v), want unknown — the ledger cannot say "+
			"whether the task step applied", retry.Outcome, err)
	}
	for _, said := range retry.Caveats {
		if said.Code() == caveat.CodeFiledEarlierUnread || said.Code() == caveat.CodeFiledEarlierRemoved {
			t.Errorf("the retry claims a landing nothing vouches for: %s", said.Agent())
		}
	}
	if got := r.logEnd(t); got != end {
		t.Fatalf("the retry put %d record(s) on the log — a number minted for "+
			"a create this node cannot decide", got-end)
	}
}

// A CREATE THIS NODE'S LEDGER CANNOT VOUCH FOR IS ANSWERED FROM ITS OWN TASK
// ROW, AND NEVER AS "NOT MADE".
//
// A seat re-running a turn older than the ledger's thirty-day sweep carries an
// operation minted before the loss; told "NOT made", it rephrases (a new
// operation) and files a duplicate. The task's id is a function of the
// operation, so its row says whether this operation filed it.
func TestACreateTheLedgerCannotVouchForIsAnsweredFromItsTaskRow(t *testing.T) {
	t.Parallel()

	t.Run("filed by an earlier run", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		r.applyWhileWriting()
		minted := time.Now().Add(-time.Hour)
		op := statelog.NewOpID(minted, "create")
		first, err := r.writer.CreateTask(t.Context(), op, newTask("t-once"), nil)
		if err != nil || first.Key == "" {
			t.Fatalf("the first run = (%+v, %v)", first, err)
		}
		r.drain()
		r.sweepLedger(minted.Add(time.Minute))
		end := r.logEnd(t)

		retry, err := r.writer.CreateTask(t.Context(), op, newTask("t-once"), nil)
		if err != nil {
			t.Fatalf("the retry: %v — an operation this node cannot vouch for is "+
				"not a failure, and its task is right here", err)
		}
		if retry.Outcome != statelog.OutcomeApplied || retry.Key != first.Key {
			t.Fatalf("the retry answered %q with key %q, want applied with the "+
				"first run's %q", retry.Outcome, retry.Key, first.Key)
		}
		if got := r.logEnd(t); got != end {
			t.Fatalf("the retry put %d record(s) on the log", got-end)
		}
	})

	t.Run("filed nowhere this node can see", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		r.applyWhileWriting()
		minted := time.Now().Add(-time.Hour)
		r.sweepLedger(minted.Add(time.Minute))
		end := r.logEnd(t)

		got, err := r.writer.CreateTask(t.Context(), statelog.NewOpID(minted, "create"),
			newTask("t-maybe"), nil)
		if err != nil {
			t.Fatalf("the create = %v, want no error — neither made nor not made "+
				"is not a failure to report as one", err)
		}
		if got.Outcome != statelog.OutcomeUnknown || !got.Unvouched || got.Key != "" {
			t.Fatalf("the create answered %+v, want an unvouched unknown with no key",
				got.Result)
		}
		if n := r.logEnd(t); n != end {
			t.Fatalf("the create put %d record(s) on the log", n-end)
		}
	})
}

// sweepLedger does what the ledger's retention sweep does to this node: every
// row goes, and the watermark records that rows applied before cutoff may
// have.
func (r *roundTrip) sweepLedger(cutoff time.Time) {
	r.t.Helper()
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `DELETE FROM tracker_ops`)
		return err
	}); err != nil {
		r.t.Fatalf("sweep the operation ledger: %v", err)
	}
	r.markLedgerLost(cutoff)
}

// markLedgerLost moves the operation ledger's watermark to cutoff and deletes
// nothing: a loss whose rows were not these.
func (r *roundTrip) markLedgerLost(cutoff time.Time) {
	r.t.Helper()
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO statelog_ops_lost (ops_table, lost_before) VALUES (?, ?)
			ON CONFLICT (ops_table) DO UPDATE SET
				lost_before = MAX(lost_before, excluded.lost_before)`,
			worklet.Domain{}.OpsTable(), store.EncodeTime(cutoff))
		return err
	}); err != nil {
		r.t.Fatalf("move the operation ledger's watermark: %v", err)
	}
}
