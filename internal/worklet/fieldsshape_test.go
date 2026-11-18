package worklet_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY `fields_json` THIS BUILD WRITES IS A MAP OF FROM/TO PAIRS.
//
// [Reader.Activity] decodes the column into `map[string]Delta` and drops the
// whole map on failure, while a task's own history decodes `map[string]any`:
// a value that is not a pair makes the feed and the History tab silently
// disagree. A round trip over real writes, because the invariant is produced:
// the one writer (`Applier.stampHistory`) has two sources, `TaskDeltas` and
// `Notify.Fields`, both `map[string]Delta`; a third source or a type change
// is what this catches.
func TestEveryHistoryRowRecordsFromToPairsAndNothingElse(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	created := r.createTask("who owns the rollback")
	filedInto(t, r, "ENG", "t-blocker")

	// A comment carrying mentions and an ask: still recorded as deltas,
	// not by value.
	if _, err := r.writer.UpdateTask(t.Context(), "op-comment", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "cm-1", Task: created.ID, Author: "bo",
			AuthorKind: worklet.AuthorHuman, Body: "@ana please look",
			Mentions: []string{"ana"}, Ask: "ana", CreatedAt: wednesday,
		}}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-status", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done},
		worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("status: %v", err)
	}
	r.drain()

	parent := "t-blocker"
	if _, err := r.writer.UpdateTask(t.Context(), "op-adopt", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Parent: &parent},
		worklet.ChangeReparented, nil); err != nil {
		t.Fatalf("re-parent: %v", err)
	}
	r.drain()

	// The other producer family: a document write, not a task commit.
	seedProject(t, r, worklet.Project{Key: "OPS", Name: "Operations"})

	rows := r.strings(`SELECT kind || ' ' || fields_json FROM tracker_history
		ORDER BY log_seq`)
	if len(rows) < 6 {
		t.Fatalf("only %d history rows were written, so this case is not "+
			"exercising the producers it names: %v", len(rows), rows)
	}
	carried := 0
	for _, row := range rows {
		kind, body, _ := strings.Cut(row, " ")
		// "Nothing moved" is not a value (`Applier.stampHistory` on why `{}`
		// rather than `null`).
		if body == "" || body == "{}" || body == "null" {
			continue
		}
		var pairs map[string]worklet.Delta
		if err := json.Unmarshal([]byte(body), &pairs); err != nil {
			t.Errorf("a %s row records %s, which is not a map of from/to "+
				"pairs — the activity feed drops the whole map on that and "+
				"renders the kind alone, where the item's own History tab "+
				"renders it: %v", kind, body, err)
			continue
		}
		carried++
	}
	if carried == 0 {
		t.Fatal("no row carried any deltas at all, so nothing above was decoded")
	}
}
