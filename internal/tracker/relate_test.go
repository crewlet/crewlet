package tracker_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// A WHOLE RELATION SET KEEPS WHAT ITS CALLER DID NOT RESTATE.
//
// A `set` is the caller's whole statement of which edges the task holds and of
// the note on each. An edge it restates — the same kind to the same task,
// which is an edge's identity — is one the task already holds, so who authored
// it and when, the repair duty's decision that its mirror will never be
// written, and a member a newer build wrote on it are the stored edge's: the
// caller holds no honest value for any of them. An edge the set adds is one
// this write authors, and the writer stamps it as it stamps an added edge.
//
// Mutation: return the deduplicated set as it arrived in
// RelationIntent.resolve and the restated edge loses its author, its flag and
// the newer build's member; leave the set out of settleRelations' stamping and
// the new edge names no author.
func TestAWholeRelationSetKeepsWhatItsCallerDidNotRestate(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2", "t-3"} {
		filedTask(t, r, id)
	}
	authored := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)
	stored := []tracker.Relation{{
		Kind: tracker.RelationLinked, Other: "t-2", Note: "first",
		CreatedBy: "bo", CreatedAt: authored, OneSidedFinal: true,
		Extra: map[string]json.RawMessage{"lane": json.RawMessage(`"newer"`)},
	}}
	if _, err := r.writer.UpdateTask(t.Context(), "op-seed", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Relations: &stored},
		tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("seed the edge: %v", err)
	}
	r.drain()

	if _, err := r.writer.UpdateTask(t.Context(), "op-set", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Relate: &tracker.RelationIntent{
			Set: []tracker.Relation{
				{Kind: tracker.RelationLinked, Other: "t-2", Note: "second"},
				{Kind: tracker.RelationLinked, Other: "t-3"},
			},
		}}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("restate the set: %v", err)
	}
	r.drain()
	for path, want := range map[string]string{
		"$.relations[0].other":           "t-2",
		"$.relations[0].note":            "second",
		"$.relations[0].created_by":      "bo",
		"$.relations[0].created_at":      authored.Format(time.RFC3339),
		"$.relations[0].one_sided_final": "1",
		"$.relations[0].lane":            "newer",
		"$.relations[1].other":           "t-3",
		"$.relations[1].created_by":      "ana",
		"$.relations[1].lane":            "",
	} {
		got := r.strings(`SELECT COALESCE(json_extract(CAST(document AS TEXT), '` +
			path + `'), '') FROM tracker_tasks WHERE id = 't-1'`)
		if len(got) != 1 || got[0] != want {
			t.Errorf("after the set the task holds %s = %q, want %q", path, got, want)
		}
	}
}
