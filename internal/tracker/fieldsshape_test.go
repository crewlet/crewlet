package tracker_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY `fields_json` THIS BUILD WRITES IS A MAP OF FROM/TO PAIRS.
//
// It is the one thing that makes [Reader.Activity]'s decode safe. That read
// unmarshals the column into `map[string]Delta` and, on failure, drops the
// WHOLE map for that row (`record.Fields = nil`) so the feed renders the kind
// alone — while a task detail's own history decodes the same column into
// `map[string]any` and cannot fail. So the day a producer writes a value that
// is not a pair, `#/work/history` and an item's History tab start describing
// one record differently, silently, and only on the records that carry the new
// shape.
//
// THE INVARIANT IS PRODUCED RATHER THAN DECLARED, which is why this is a
// round trip over real writes instead of a table of literals: the column has
// exactly one writer (`Applier.stampHistory`) and exactly two sources, the
// applier's own `TaskDeltas` and the record's `Notify.Fields`, and BOTH are
// `map[string]Delta`. A third source, or either of those two changing type, is
// what this case is here to catch — in the place where it is cheap, rather
// than as a feed that quietly stops saying what moved.
//
// IT EXISTS BECAUSE THE OPPOSITE WAS WRITTEN DOWN AND BELIEVED. A comment in
// `dashboard/src/lib/work.ts` asserted for a long time that the engine writes
// "the notification's own fields where it cannot compare two documents (a
// comment, a mention, an ask)" — and `apply_history.go` refutes exactly that
// sentence in its own words, but nothing held the two against each other, so
// the false half was read as a defect report against the feed. A claim about
// what a column holds is worth what checks it.
func TestEveryHistoryRowRecordsFromToPairsAndNothingElse(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	created := r.createTask("who owns the rollback")
	filedInto(t, r, "ENG", "t-blocker")

	// THE RECORD THE FALSE CLAIM NAMED: a comment carrying mentions and an
	// ask, which is the one the engine was supposed to describe by value
	// rather than by delta.
	if _, err := r.writer.UpdateTask(t.Context(), "op-comment", created.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-1", Task: created.ID, Author: "bo",
			AuthorKind: tracker.AuthorHuman, Body: "@ana please look",
			Mentions: []string{"ana"}, Ask: "ana", CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-status", created.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done},
		tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("status: %v", err)
	}
	r.drain()

	parent := "t-blocker"
	if _, err := r.writer.UpdateTask(t.Context(), "op-adopt", created.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Parent: &parent},
		tracker.ChangeReparented, nil); err != nil {
		t.Fatalf("re-parent: %v", err)
	}
	r.drain()

	// AND THE OTHER PRODUCER FAMILY: a document write rather than a task
	// commit, which reaches the same column through the same frame.
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations"})

	rows := r.strings(`SELECT kind || ' ' || fields_json FROM tracker_history
		ORDER BY log_seq`)
	if len(rows) < 6 {
		t.Fatalf("only %d history rows were written, so this case is not "+
			"exercising the producers it names: %v", len(rows), rows)
	}
	carried := 0
	for _, row := range rows {
		kind, body, _ := strings.Cut(row, " ")
		// THE TWO SPELLINGS OF "NOTHING MOVED" are not values and are not
		// what this is about — see `Applier.stampHistory` on why the
		// column holds `{}` rather than `null`.
		if body == "" || body == "{}" || body == "null" {
			continue
		}
		var pairs map[string]tracker.Delta
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
