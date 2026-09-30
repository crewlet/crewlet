package pages_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TURN'S COMMENT RE-POSTED AFTER THIS NODE'S LEDGER LOST ITS ROW IS NOT
// POSTED A SECOND TIME, through the real store, the real publisher and the
// real ledger.
//
// A re-run turn derives the SAME operation for the same comment — that is what
// its turn key is for — and the row that would say the first run's copy landed
// is gone: here because this node adopted a snapshot from a donor that
// scrubbed its ledger, which the join records exactly as staged below (the
// ledger empty, the join's start as its watermark — see
// [statelog.RecordLedgerLoss]). Decided again on rows that already hold the
// comment, it is a second record every node applies: a second history entry
// and a second wake for one remark. What stops it is the instant the
// operation id carries, which is the one the turn's work BEGAN at and so the
// same on every re-run — never the re-run's own clock, which is after the loss
// by construction.
func TestATurnsCommentRepostedAfterAnAdoptionIsNotPostedTwice(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})

	in := pages.NewComment{
		Body:      "does this still hold?",
		TurnKey:   "wk-1",
		TurnSince: time.Now().Add(-time.Hour),
	}
	if _, _, err := r.store.Comment(t.Context(), agent("bob"), page.Page.ID, in); err != nil {
		t.Fatalf("the first run's comment: %v", err)
	}
	r.drain()

	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM pages_ops`)
		return err
	}); err != nil {
		t.Fatalf("scrub the ledger: %v", err)
	}
	if err := statelog.RecordLedgerLoss(t.Context(), r.db.Replicated(),
		pages.Domain{}, time.Now()); err != nil {
		t.Fatalf("record the join's watermark: %v", err)
	}
	end, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	// THE RE-RUN COMES AFTER THE LOSS, by more than the millisecond an
	// operation id resolves its instant to — so a re-run whose own clock had
	// leaked into the id would read as minted after it, which is the defect.
	time.Sleep(10 * time.Millisecond)

	_, written, err := r.store.Comment(t.Context(), agent("bob"), page.Page.ID, in)
	if err != nil {
		t.Fatalf("the re-run's comment: %v", err)
	}
	if written.Outcome.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("the re-run's comment answered %q, want unknown — the ledger "+
			"row that would say the first run's copy landed is gone, and "+
			"posting it again is a second remark nobody made twice",
			written.Outcome.Outcome)
	}
	after, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if after != end {
		t.Fatalf("the re-run put %d record(s) on the log, want none", after-end)
	}
	r.drain()
	thread, err := r.store.Thread(t.Context(), page.Page.ID)
	if err != nil {
		t.Fatalf("read the thread: %v", err)
	}
	if len(thread) != 1 {
		t.Fatalf("the thread holds %d comments, want the one", len(thread))
	}
}
