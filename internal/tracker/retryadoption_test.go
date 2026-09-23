package tracker_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A WRITE RETRIED AFTER THIS NODE ADOPTED A DONATED SNAPSHOT IS NOT APPLIED A
// SECOND TIME, through the real writer, the real publisher and the real
// ledger.
//
// The adoption installs a replicated estate whose operation ledger was
// scrubbed, so the retry of an operation minted before it — a turn re-run
// under its derived operation id, a caller repeating an `unknown` — finds no
// row. Its fresh snapshot already holds the first application, and deciding
// again on top of it publishes the operation a second time; the broker's
// duplicate window is two minutes wide and is not what may stand between the
// two. What does is the operation id's OWN mint instant, read against the
// adoption record: every writer used to stamp its own clock beside the id,
// which on a retry is after the adoption by construction.
func TestAWriteRetriedAfterAnAdoptionIsNotAppliedTwice(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	created := r.createTask("who owns the rollback")

	// THE OPERATION, minted before the adoption — an hour ago, which is
	// the instant its id carries whichever call makes it.
	op := statelog.NewOpID(time.Now().Add(-time.Hour), "comment-"+created.ID)
	comment := func() (tracker.WriteResult, error) {
		return r.writer.UpdateTask(t.Context(), op, created.ID, "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
				ID: "cm-1", Task: created.ID, Author: "ana",
				AuthorKind: tracker.AuthorHuman, Body: "I do.",
				CreatedAt: wednesday,
			}}, tracker.ChangeComment, nil)
	}
	if first, err := comment(); err != nil || first.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the first application = (%+v, %v), want applied", first.Result, err)
	}
	r.drain()

	// THE ADOPTION: recorded in the node estate after the operation was
	// minted, and the ledger scrubbed out of the replicated one exactly as
	// a donated artefact arrives.
	adoptASnapshot(t, r, "tracker_ops")
	end, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	// THE RETRY COMES AFTER THE ADOPTION, by more than the millisecond an
	// operation id resolves its instant to — so a retry whose own clock
	// stood in for the mint would read as minted after it, which is the
	// defect.
	time.Sleep(10 * time.Millisecond)

	retry, err := comment()
	if err != nil {
		t.Fatalf("the retry: %v — an operation this node cannot vouch for is "+
			"answered, not refused as a broken applier", err)
	}
	if retry.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("the retry answered %q, want unknown — the ledger that would "+
			"say whether the first application landed was scrubbed, and "+
			"deciding again applies the operation twice", retry.Outcome)
	}
	after, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if after != end {
		t.Fatalf("the retry put %d record(s) on the log — every one of them is "+
			"applied by every node, a second application of one operation",
			after-end)
	}
	detail, err := r.reader.Task(t.Context(), created.ID,
		tracker.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the thread: %v", err)
	}
	if len(detail.Comments) != 1 {
		t.Fatalf("the thread holds %d comments, want the one", len(detail.Comments))
	}
}

// adoptASnapshot does to this node what installing a donated artefact does to
// its operation ledger: the adoption is recorded, now, in the node estate, and
// the ledger table arrives empty because every donor scrubs it.
func adoptASnapshot(t *testing.T, r *roundTrip, ledger string) {
	t.Helper()
	if err := statelog.RecordAdoption(t.Context(), r.db, time.Now(), "node-b",
		statelog.Manifest{SHA256: "artefact"}, statelog.AdoptionComplete); err != nil {
		t.Fatalf("record the adoption: %v", err)
	}
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM `+ledger)
		return err
	}); err != nil {
		t.Fatalf("scrub the ledger: %v", err)
	}
}

// A WRITE RETRIED AFTER ITS LEDGER ROW WAS SWEPT IS NOT APPLIED A SECOND TIME,
// through the real writer, the real publisher, the real ledger and the real
// sweep.
//
// The ledger keeps a row for thirty days, and a retry is judged by it — so a
// retry of an operation whose row the sweep deleted used to find the same
// silence as an operation that never ran, decide again on rows that already
// held the first application, and publish a second copy. The sweep now
// records how far back it forgot, and the publisher reads that before it
// trusts the silence.
func TestAWriteRetriedAfterItsLedgerRowWasSweptIsNotAppliedTwice(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	created := r.createTask("who owns the rollback")

	comment := func(op, id string) (tracker.WriteResult, error) {
		return r.writer.UpdateTask(t.Context(), op, created.ID, "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
				ID: id, Task: created.ID, Author: "ana",
				AuthorKind: tracker.AuthorHuman, Body: "I do.",
				CreatedAt: wednesday,
			}}, tracker.ChangeComment, nil)
	}
	op := statelog.NewOpID(time.Now().Add(-time.Hour), "comment-"+created.ID)
	if first, err := comment(op, "cm-1"); err != nil || first.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the first application = (%+v, %v), want applied", first.Result, err)
	}
	r.drain()

	// THE SWEEP, with a cutoff past the row — the arithmetic of a month
	// passing, done by the job that runs it.
	sweep, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: tracker.Domain{}, Applier: r.applier, Fetch: noFetch{},
		DB: r.db.Replicated(),
	})
	if err != nil {
		t.Fatalf("build the ledger's owner: %v", err)
	}
	if n, err := sweep.PurgeOps(t.Context(), time.Now()); err != nil || n == 0 {
		t.Fatalf("the sweep = (%d, %v), want the ledger's rows", n, err)
	}
	end := r.logEnd(t)

	retry, err := comment(op, "cm-1")
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if retry.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("the retry answered %q, want unknown — the row that would say "+
			"whether the first application landed was swept", retry.Outcome)
	}
	if got := r.logEnd(t); got != end {
		t.Fatalf("the retry put %d record(s) on the log — a second application "+
			"of one operation, applied by every node", got-end)
	}

	// AND THE CONTROL: an operation minted after the sweep's cutoff is
	// judged by its row as ever, or the sweep would refuse every write.
	fresh, err := comment(statelog.NewOpID(time.Now(), "comment-"+created.ID), "cm-2")
	if err != nil || fresh.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a fresh operation after the sweep = (%+v, %v), want applied",
			fresh.Result, err)
	}
}

// noFetch is a log nothing is pulled from: the sweep's owner is built only to
// sweep.
type noFetch struct{}

func (noFetch) Fetch(context.Context, int, int, time.Duration) ([]statelog.Message, error) {
	return nil, nil
}

func (noFetch) Pending(context.Context) (uint64, error) { return 0, nil }
