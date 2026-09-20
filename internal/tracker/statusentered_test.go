package tracker_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// `status_entered_at` IS STAMPED, AND IT NAMES THE NEWEST STATUS CHANGE.
//
// # Why this case exists
//
// The column had no test at all, and it is the one thing that survived the
// table it used to be written beside. `recomputeSpans` rebuilt
// `tracker_status_spans` and stamped this column in the same pass; migration
// 0014 dropped the table, and the pass shrank to the stamp alone
// ([Applier.stampStatusEntered]).
//
// Deleting the function along with its table would have been the obvious move
// and is the one this guards against. `upsertTask` binds a literal 0 for this
// column on insert and OMITS it from the conflict update — so with no writer
// the column would read 0 on every task in the company, `sort=status_entered`
// would order by nothing, the `status_entered` date filter would match every
// task or none, and NOTHING in the tree would have failed.
//
// # What is asserted
//
// THE INSTANT, not merely that something was written. A stamp that named the
// FIRST status change rather than the newest is the shape the old code could
// have degraded into — it walked every status row and took the last — and a
// non-zero check passes on it happily.
func TestTheEnteredInstantNamesTheNewestStatusChange(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	// TWO MOVES, because one cannot tell "the newest" from "the first".
	for i, status := range []tracker.Status{
		tracker.StatusInProgress, tracker.StatusInReview,
	} {
		moved := status
		if _, err := r.writer.UpdateTask(t.Context(), "op-move-"+string(status),
			"t-1", "ENG", tracker.NoIfMatch,
			tracker.TaskPatch{Status: &moved}, tracker.ChangeStatus, nil); err != nil {
			t.Fatalf("move %d to %s: %v", i+1, status, err)
		}
		r.drain()
	}

	// THE COLUMN AGAINST THE HISTORY IT IS DERIVED FROM, rather than
	// against a literal: the instant is the broker's own and a case that
	// hard-coded one would be asserting the harness's clock.
	want := r.strings(`SELECT CAST(h.effective_at AS TEXT)
		FROM tracker_history h
		WHERE h.subject_id = ?
		  AND json_extract(h.fields_json, '$.status.to') IS NOT NULL
		ORDER BY h.log_seq DESC LIMIT 1`, "t-1")
	if len(want) != 1 {
		t.Fatalf("the history holds %d status rows for t-1, want the newest "+
			"of three — the derivation has nothing to read", len(want))
	}
	got := r.strings(
		`SELECT CAST(status_entered_at AS TEXT) FROM tracker_tasks WHERE id = ?`, "t-1")
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("status_entered_at is %v and the newest status row is at %v "+
			"— the column is the `status_entered` sort key and date filter, "+
			"and nothing else writes it", got, want)
	}
	if len(got) == 1 && got[0] == "0" {
		t.Error("status_entered_at is 0, which is what `upsertTask` binds when " +
			"nothing stamps it: the sort orders by nothing and the date " +
			"filter matches every task")
	}
}

// AND THE SORT ACTUALLY ORDERS BY IT.
//
// The column check above passes on a build whose QUERY side stopped reading
// it. This is the other end: two tasks whose statuses moved in a known order
// come back in that order, and reversed under `-status_entered`.
func TestTheEnteredInstantIsWhatStatusEnteredSortsOn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-early")
	filedTask(t, r, "t-late")

	// MOVED IN A KNOWN ORDER, one drain apart, so the log's own positions
	// separate them — the effective instant is the broker's and two writes
	// inside one drain can share it.
	for _, id := range []string{"t-early", "t-late"} {
		moved := tracker.StatusInProgress
		if _, err := r.writer.UpdateTask(t.Context(), "op-move-"+id, id, "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Status: &moved},
			tracker.ChangeStatus, nil); err != nil {
			t.Fatalf("move %s: %v", id, err)
		}
		r.drain()
	}

	order := func(sort string) []string {
		t.Helper()
		q, err := tracker.ParseQuery(tracker.MapParams(map[string]any{
			"container": "project:ENG", "sort": sort,
		}), wednesday, berlin)
		if err != nil {
			t.Fatalf("ParseQuery(sort=%s): %v", sort, err)
		}
		q.Level = statelog.ReadStale
		answer, err := r.reader.Tasks(t.Context(), q, wednesday)
		if err != nil {
			t.Fatalf("Tasks(sort=%s): %v", sort, err)
		}
		out := make([]string, 0, len(answer.Rows))
		for _, row := range answer.Rows {
			out = append(out, row.ID)
		}
		return out
	}

	if got := order("status_entered"); len(got) < 2 || got[0] != "t-early" {
		t.Errorf("sort=status_entered answered %v, want the task whose status "+
			"moved first at the front", got)
	}
	if got := order("-status_entered"); len(got) < 2 || got[0] != "t-late" {
		t.Errorf("sort=-status_entered answered %v, want the task whose status "+
			"moved last at the front", got)
	}
}
