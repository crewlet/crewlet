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

// appendOwnGeneration appends node A's own record opening the next generation of
// the tracker's log, exactly as a reanchor's append would, and nothing else —
// what an attempt that failed after its append leaves behind — and answers the
// sequence it landed at.
func appendOwnGeneration(t *testing.T, e *Engine) uint64 {
	t.Helper()
	running := e.native.Load().log.Domain(tracker.Domain{}.Name())
	in, _, err := e.reanchorInputs(t.Context(), running)
	if err != nil {
		t.Fatalf("reanchorInputs: %v", err)
	}
	gen := max(in.Generation, in.Abandoned) + 1
	record, keeps, err := tracker.GenerationRecord{}.GenerationRecord(statelog.GenerationFacts{
		Generation: gen, Case: statelog.ReanchorRestored, Inputs: in,
		By: "ops-1", Writer: e.native.Load().nodeID, At: time.Now().UTC(),
	})
	if err != nil || !keeps {
		t.Fatalf("encode the generation record: (%v, %v)", keeps, err)
	}
	zero := uint64(0)
	seq, _, err := running.log.Append(t.Context(),
		tracker.Domain{}.Stream().SubjectPrefix+"."+record.Subject.String(),
		record.OpID, &zero, record.Payload)
	if err != nil {
		t.Fatalf("append the first attempt's generation record: %v", err)
	}
	return seq
}

// A RE-RUN OF A RESTORED REANCHOR THAT FAILED AFTER ITS APPEND FINISHES.
//
// The first attempt's generation record is on the log and nothing else moved —
// a CLI that gave up after ten seconds while the consumer was being rebuilt is
// enough. The re-run reads the log's end after that record, and its walk for
// records written after the restore met the node's own record first: it
// refused naming it, hiding every real one beneath it, and with the discard
// flag it put the checkpoint above the record, which was then applied nowhere.
// Now the walk and the checkpoint stop one below the node's own record: with
// nothing written after the restore the re-run just finishes, and over records
// node B wrote it names B's — and, told to discard them, the resumed applier
// applies the generation record first.
func TestARerunOfARestoredReanchorThatFailedAfterItsAppendFinishes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		stage func(t *testing.T) divergedBroker
		// discards is whether node B's writes are below the record.
		discards bool
	}{
		{"nothing written after the restore", stageRestoredBroker, false},
		{"the log written past this node's rows", stageDivergedBroker, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := c.stage(t)
			e, _ := bootNode(t, &d.a, d.cfg)
			stream := tracker.Domain{}.Stream().Name
			running := e.native.Load().log.Domain(tracker.Domain{}.Name())
			opened := appendOwnGeneration(t, e)

			view, err := e.ReanchorStatus(t.Context(), stream)
			if err != nil {
				t.Fatalf("ReanchorStatus: %v", err)
			}
			if view.Case != statelog.ReanchorRestored || view.Cursor != opened-1 {
				t.Fatalf("the re-run's status names the %q case at %d, want restored "+
					"at %d, one below its own record at %d", view.Case, view.Cursor,
					opened-1, opened)
			}
			switch {
			case !c.discards && view.Discards != nil:
				t.Fatalf("the re-run would discard %+v — its own generation record "+
					"is not a record written after the restore", view.Discards)
			case c.discards && (view.Discards == nil || view.Discards.Writer != d.b.Node.ID):
				t.Fatalf("the re-run would discard %+v, want node B's newest record "+
					"— the one its own record hid", view.Discards)
			}
			req := ReanchorRequest{Stream: stream,
				Confirm: statelog.ConfirmationOf(view.CreatedAt), By: "ops-1"}
			if c.discards {
				if _, err := e.Reanchor(t.Context(), req); !errors.Is(err,
					statelog.ErrReanchorRefused) || !strings.Contains(err.Error(), d.b.Node.ID) {
					t.Fatalf("the re-run without -discard = %v, want the refusal naming "+
						"node B's record", err)
				}
				req.Discard = true
			}
			plan, err := e.Reanchor(t.Context(), req)
			if err != nil {
				t.Fatalf("the re-run: %v", err)
			}
			if plan.Cursor != opened-1 {
				t.Fatalf("the re-run's checkpoint is at %d, want %d — at the end it "+
					"read, its own record at %d is applied nowhere", plan.Cursor,
					opened-1, opened)
			}
			waitUntil(t, 10*time.Second, "the tracker to apply its own generation record", func() bool {
				return countRows(t, e, "SELECT COUNT(*) FROM tracker_log_generations "+
					"WHERE generation = ?", plan.Generation) == 1
			})
			if got := taskState(t, e, "t-1"); got != d.before {
				t.Fatalf("the task is %+v after the re-run, want A's own %+v", got, d.before)
			}
			if running.runner.StreamIdentity() != nil {
				t.Fatalf("the tracker still refuses after the re-run: %v",
					running.runner.StreamIdentity())
			}
		})
	}
}
