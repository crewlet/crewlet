package worklet_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// moveTask sets one ENG task's status as a person's edit would.
func moveTask(t *testing.T, r *roundTrip, id string, status worklet.Status) {
	t.Helper()
	if _, err := r.writer.UpdateTask(t.Context(), "op-"+id+"-"+string(status), id, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &status}, worklet.ChangeStatus,
		&worklet.Notify{Kind: worklet.ChangeStatus}); err != nil {
		t.Fatalf("move %s to %s: %v", id, status, err)
	}
	r.drain()
}

// dependOn writes one dependency gesture in ENG.
func dependOn(t *testing.T, r *roundTrip, op string, change worklet.DependencyChange) {
	t.Helper()
	change.Project = "ENG"
	if _, err := r.writer.Depend(t.Context(), op, change, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend %s: %v", op, err)
	}
	r.drain()
}

// edgeOpen is whether dep's row says its edge to blk is open, and the row's
// `blocked`.
func edgeOpen(t *testing.T, r *roundTrip, dep, blk string) (open, blocked bool) {
	t.Helper()
	got := row(t, r, dep)
	for _, edge := range got.WaitingOn {
		if edge.ID == blk {
			return edge.Open, got.Blocked
		}
	}
	t.Fatalf("%s's row has no edge to %s: %+v", dep, blk, got.WaitingOn)
	return false, false
}

// A FINISHED BLOCKER SAYS SO ON THE WIRE: `open` is stated both ways, and `blocked` is exactly
// "some edge states open". Mutation: tag `open` omitempty again, and the finished edge states
// nothing.
func TestEveryEdgeStatesWhetherItsBlockerIsOpen(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"dep", "finished", "working"} {
		filedTask(t, r, id)
	}
	dependOn(t, r, "op-depend", worklet.DependencyChange{Task: "dep",
		WaitingOnAdd: []string{"finished", "working"}})
	moveTask(t, r, "finished", worklet.StatusDone)

	raw, err := json.Marshal(row(t, r, "dep"))
	if err != nil {
		t.Fatalf("encode the row: %v", err)
	}
	var wire struct {
		Blocked   bool             `json:"blocked"`
		WaitingOn []map[string]any `json:"waiting_on"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode the row: %v", err)
	}
	want := map[string]bool{"finished": false, "working": true}
	if len(wire.WaitingOn) != len(want) {
		t.Fatalf("the row carries %d edges, want %d: %s", len(wire.WaitingOn), len(want), raw)
	}
	anyOpen := false
	for _, edge := range wire.WaitingOn {
		id, _ := edge["id"].(string)
		open, stated := edge["open"].(bool)
		if !stated {
			t.Errorf("the edge to %s states no `open`, so a reader guesses: %s", id, raw)
			continue
		}
		if open != want[id] {
			t.Errorf("the edge to %s says open=%v, want %v", id, open, want[id])
		}
		anyOpen = anyOpen || open
	}
	if wire.Blocked != anyOpen {
		t.Errorf("the row says blocked=%v while its edges say %v open", wire.Blocked, anyOpen)
	}
}

// AN EDGE FOLLOWS ITS BLOCKER'S STATUS ON EVERY PATH THAT MOVES EITHER END, in any order and on
// replay. Mutation: drop the blocker-side update from maintainDeps, and every case that finishes
// the blocker after the edge reads it open.
func TestADependentsEdgeFollowsItsBlockersStatus(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		steps func(t *testing.T, r *roundTrip)
		open  bool
	}{
		"authored on the dependent, the blocker done": {func(t *testing.T, r *roundTrip) {
			dependOn(t, r, "op-1", worklet.DependencyChange{Task: "dep", WaitingOnAdd: []string{"blk"}})
			moveTask(t, r, "blk", worklet.StatusDone)
		}, false},
		"authored on the blocker, the blocker cancelled": {func(t *testing.T, r *roundTrip) {
			dependOn(t, r, "op-1", worklet.DependencyChange{Task: "blk", BlockingAdd: []string{"dep"}})
			moveTask(t, r, "blk", worklet.StatusCancelled)
		}, false},
		"the blocker finished before the edge": {func(t *testing.T, r *roundTrip) {
			moveTask(t, r, "blk", worklet.StatusDone)
			dependOn(t, r, "op-1", worklet.DependencyChange{Task: "dep", WaitingOnAdd: []string{"blk"}})
		}, false},
		"through work and review": {func(t *testing.T, r *roundTrip) {
			dependOn(t, r, "op-1", worklet.DependencyChange{Task: "dep", WaitingOnAdd: []string{"blk"}})
			moveTask(t, r, "blk", worklet.StatusInProgress)
			moveTask(t, r, "blk", worklet.StatusInReview)
			moveTask(t, r, "blk", worklet.StatusDone)
		}, false},
		"reopened": {func(t *testing.T, r *roundTrip) {
			dependOn(t, r, "op-1", worklet.DependencyChange{Task: "dep", WaitingOnAdd: []string{"blk"}})
			moveTask(t, r, "blk", worklet.StatusDone)
			moveTask(t, r, "blk", worklet.StatusInProgress)
		}, true},
		"the dependent edited after": {func(t *testing.T, r *roundTrip) {
			dependOn(t, r, "op-1", worklet.DependencyChange{Task: "dep", WaitingOnAdd: []string{"blk"}})
			moveTask(t, r, "blk", worklet.StatusDone)
			moveTask(t, r, "dep", worklet.StatusInProgress)
		}, false},
		"a blocker waiting on its own": {func(t *testing.T, r *roundTrip) {
			filedTask(t, r, "up")
			dependOn(t, r, "op-1", worklet.DependencyChange{Task: "blk", WaitingOnAdd: []string{"up"}})
			dependOn(t, r, "op-2", worklet.DependencyChange{Task: "dep", WaitingOnAdd: []string{"blk"}})
			moveTask(t, r, "up", worklet.StatusDone)
			moveTask(t, r, "blk", worklet.StatusDone)
		}, false},
		"the mirror repaired after the blocker finished": {func(t *testing.T, r *roundTrip) {
			if _, err := r.writer.UpdateTask(t.Context(), "op-half", "dep", "ENG",
				worklet.NoIfMatch, worklet.TaskPatch{Relate: &worklet.RelationIntent{
					Add: []worklet.Relation{{Kind: worklet.RelationWaitingOn, Other: "blk"}},
				}}, worklet.ChangeRelations, nil); err != nil {
				t.Fatalf("write the authored half: %v", err)
			}
			r.drain()
			moveTask(t, r, "blk", worklet.StatusDone)
			edges, err := worklet.ScanOneSided(t.Context(), r.db.Reader(), wednesday.AddDate(1, 0, 0), 16)
			if err != nil || len(edges) != 1 {
				t.Fatalf("ScanOneSided = %v, %v; want the one edge", edges, err)
			}
			if _, err := r.writer.RepairOneSided(t.Context(), "op-repair", edges[0],
				fixedLeads{project: "eng-lead"}); err != nil {
				t.Fatalf("RepairOneSided: %v", err)
			}
			r.drain()
		}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			filedTask(t, r, "dep")
			filedTask(t, r, "blk")
			tc.steps(t, r)
			if open, blocked := edgeOpen(t, r, "dep", "blk"); open != tc.open || blocked != tc.open {
				t.Fatalf("the edge says open=%v and the row blocked=%v, want both %v", open, blocked, tc.open)
			}
			// EVERY RECORD AGAIN, as a redelivery or a replay applies it.
			for seq := uint64(1); seq <= r.consumed; seq++ {
				r.redeliver(seq)
			}
			if open, blocked := edgeOpen(t, r, "dep", "blk"); open != tc.open || blocked != tc.open {
				t.Fatalf("replayed, the edge says open=%v and the row blocked=%v, want both %v",
					open, blocked, tc.open)
			}
		})
	}
}
