package tracker_test

import (
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
