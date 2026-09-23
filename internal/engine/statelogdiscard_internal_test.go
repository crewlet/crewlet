package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A RESTORED REANCHOR OVER RECORDS WRITTEN AFTER THE RESTORE ASKS THE OPERATOR.
//
// Followed from its end, the log's records node B wrote after the restore would
// be applied on no node, and the reanchor used to do exactly that without a
// word. The status now names the newest of them — B's own last edit — the
// transition refuses without the operator's word and moves nothing, and with it
// runs: node A keeps its rows, B's edits are what it discarded, and the
// generation record says so.
func TestARestoredReanchorOverRecordsWrittenAfterTheRestoreAsksTheOperator(t *testing.T) {
	t.Parallel()
	d := stageDivergedBroker(t)
	e, _ := bootNode(t, &d.a, d.cfg)
	stream := tracker.Domain{}.Stream().Name
	running := e.native.Load().log.Domain(tracker.Domain{}.Name())

	view, err := e.ReanchorStatus(t.Context(), stream)
	if err != nil {
		t.Fatalf("ReanchorStatus: %v", err)
	}
	if view.Case != statelog.ReanchorRestored || view.Cursor != d.end {
		t.Fatalf("ReanchorStatus names the %q case at %d, want restored at %d",
			view.Case, view.Cursor, d.end)
	}
	newest := fmt.Sprintf("op-b-%d", d.written)
	if view.Discards == nil || view.Discards.Writer != d.b.Node.ID ||
		!strings.Contains(view.Discards.OpID, newest) || view.Discarding == "" {
		t.Fatalf("ReanchorStatus names %+v as discarded, want node B's %s", view.Discards, newest)
	}

	// REFUSED WITHOUT THE OPERATOR'S WORD, and nothing moved.
	_, err = e.Reanchor(t.Context(), ReanchorRequest{
		Stream: stream, Confirm: statelog.ConfirmationOf(view.CreatedAt), By: "ops-1",
	})
	if !errors.Is(err, statelog.ErrReanchorRefused) || !strings.Contains(err.Error(), "discard") {
		t.Fatalf("Reanchor without -discard = %v, want the refusal naming it", err)
	}
	if got := readCursorRow(t, e, tracker.Domain{}.Name()); got.at != d.checkpoint.at {
		t.Fatalf("a refused reanchor moved the checkpoint to %s", got.at)
	}

	// WITH IT, A KEEPS ITS ROWS.
	plan, err := e.Reanchor(t.Context(), ReanchorRequest{
		Stream: stream, Confirm: statelog.ConfirmationOf(view.CreatedAt), By: "ops-1",
		Discard: true,
	})
	if err != nil {
		t.Fatalf("Reanchor with -discard: %v", err)
	}
	if plan.Case != statelog.ReanchorRestored || plan.Cursor != d.end || plan.Discarded == nil ||
		plan.Discarded.Seq != view.Discards.Seq {
		t.Fatalf("reanchored as %+v, want the restored case at %d naming what it "+
			"discarded", plan, d.end)
	}
	waitUntil(t, 10*time.Second, "the tracker to resume in the new generation", func() bool {
		return running.runner.StreamIdentity() == nil &&
			running.runner.Committed().Generation == plan.Generation &&
			running.runner.Committed().Seq > plan.Cursor
	})
	if got := taskState(t, e, "t-1"); got != d.before {
		t.Fatalf("the task is %+v after the reanchor, want A's own %+v — B's edits "+
			"were the ones discarded", got, d.before)
	}
	waitUntil(t, 10*time.Second, "the generation record to say what it discarded", func() bool {
		return countRows(t, e, "SELECT COUNT(*) FROM tracker_log_generations "+
			"WHERE generation = ? AND reason LIKE ?", plan.Generation, "%discarded%") == 1
	})
	res := mustApply(t, "an edit after the reanchor", func() (tracker.WriteResult, error) {
		title := "after the reanchor"
		return e.native.Load().writer.UpdateTask(t.Context(), "op-a-after", "t-1", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Title: &title}, tracker.ChangeFields, nil)
	})
	if res.Position.Generation != plan.Generation {
		t.Fatalf("the edit landed at %s, want generation %d", res.Position, plan.Generation)
	}
}
