package worklet_test

import (
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// `status_entered_at` is stamped, and it names the newest status change.
//
// Nothing else writes it ([Applier.stampStatusEntered]): `upsertTask` binds
// 0 on insert and omits it from the conflict update, so without the stamp
// every task reads 0, `sort=status_entered` orders by nothing, the
// `status_entered` date filter matches all or none, and nothing else fails.
// The instant is asserted, not merely a non-zero value: a stamp naming the
// first status change rather than the newest passes a non-zero check.
func TestTheEnteredInstantNamesTheNewestStatusChange(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	// TWO MOVES, because one cannot tell "the newest" from "the first".
	for i, status := range []worklet.Status{
		worklet.StatusInProgress, worklet.StatusInReview,
	} {
		moved := status
		if _, err := r.writer.UpdateTask(t.Context(), "op-move-"+string(status),
			"t-1", "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Status: &moved}, worklet.ChangeStatus, nil); err != nil {
			t.Fatalf("move %d to %s: %v", i+1, status, err)
		}
		r.drain()
	}

	// Against the history it derives from, not a literal: the instant is
	// the broker's, and a literal would assert the harness's clock.
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

// AND THE SORT ACTUALLY ORDERS BY IT: the column check above passes on a
// build whose query side stopped reading it, so two tasks moved in a known
// order come back in that order, reversed under `-status_entered`.
func TestTheEnteredInstantIsWhatStatusEnteredSortsOn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-early")
	filedTask(t, r, "t-late")

	// One drain apart, so the log's positions separate them: two writes in
	// one drain can share the broker's effective instant.
	for _, id := range []string{"t-early", "t-late"} {
		moved := worklet.StatusInProgress
		if _, err := r.writer.UpdateTask(t.Context(), "op-move-"+id, id, "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Status: &moved},
			worklet.ChangeStatus, nil); err != nil {
			t.Fatalf("move %s: %v", id, err)
		}
		r.drain()
	}

	order := func(sort string) []string {
		t.Helper()
		q, err := worklet.ParseQuery(worklet.MapParams(map[string]any{
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

// AND THE PUBLISHED FIELD CARRIES IT, not just the column.
//
// [Applier.stampStatusEntered] writes the column, but a task is served by
// decoding its stored `document`, which the applier re-marshals before the
// stamp runs (the stamp needs the history row written after the upsert). So
// the document holds a zero, `omitzero` drops it, and `GET /worklet/item`,
// `get_work_item` and the dashboard's "In status since" row get nothing. It
// is the one derived column that is no cache of the document, so it is
// joined on read rather than written into a document no writer authored.
func TestTheEnteredInstantReachesThePublishedField(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	moved := worklet.StatusInProgress
	if _, err := r.writer.UpdateTask(t.Context(), "op-move", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &moved},
		worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("move the status: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), "t-1", worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the task back: %v", err)
	}
	if detail.Task.StatusEnteredAt.IsZero() {
		t.Fatal("the served task carries no status_entered_at — `omitzero` " +
			"drops it from the JSON, so GET /worklet/item, the operator MCP and " +
			"the dashboard's \"In status since\" row all get nothing, while " +
			"sort=status_entered orders by a column that IS maintained")
	}

	// Against the column, not merely non-zero: a field filled from the
	// record's own clock would pass IsZero and disagree with the sort.
	want := r.strings(
		`SELECT CAST(status_entered_at AS TEXT) FROM tracker_tasks WHERE id = ?`,
		"t-1")
	if len(want) != 1 {
		t.Fatalf("the task has %d rows", len(want))
	}
	if got := detail.Task.StatusEnteredAt.UnixMicro(); got != parseMicros(t, want[0]) {
		t.Errorf("the published field is %d and the column is %s — the sort "+
			"key and the field a reader renders must be the same instant",
			got, want[0])
	}
}

func parseMicros(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return n
}
