package builtin_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// moveSpy is the board-drag seam, recording the move it was handed.
type moveSpy struct {
	got    *tracker.Move
	notify *tracker.Notify
	result tracker.MoveResult
	err    error
}

func (m *moveSpy) MoveTask(_ context.Context, _ string, move tracker.Move,
	notify *tracker.Notify) (tracker.MoveResult, error) {
	m.got, m.notify = &move, notify
	return m.result, m.err
}

// moveTool is move_work_item over the fake tracker and a spy, as the operator
// surface builds it.
func moveTool(t *testing.T, spy *moveSpy) tools.Callable {
	t.Helper()
	work := newFakeTracker()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: work, Writer: work.as,
			Mover: func(builtin.Actor) builtin.WorkMover { return spy },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator}, nil
			},
		},
	}) {
		if tool.Name() == tracker.MoveWorkItemTool {
			return tool
		}
	}
	t.Fatal("the operator surface does not serve move_work_item with a mover wired")
	return nil
}

// A DRAG NAMES ITS NEIGHBOUR BY ID, AND THE LANE ONLY WHEN IT CHANGES.
//
// A key typed by a caller is resolved before anything is written, because the
// tracker places beside a task id; and a drop into the lane the card is
// already in is not a status change, or a drag within a lane would write a
// status commit to the feed that moved nothing and wake the card's people.
func TestAMoveNamesItsNeighbourByIDAndItsLaneOnlyWhenItChanges(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		args   map[string]any
		want   tracker.Move
		wakes  bool
		status string
	}{
		{
			name: "within a lane",
			args: map[string]any{"item": "ENG-1", "before": "ENG-2", "status": "todo", "if_match": 7},
			want: tracker.Move{Task: "i1", Project: "ENG", Before: "id-2", IfMatch: 7},
		},
		{
			name:  "across lanes",
			args:  map[string]any{"item": "ENG-1", "after": "ENG-3", "status": "in_progress", "if_match": 7},
			want:  tracker.Move{Task: "i1", Project: "ENG", After: "id-3", IfMatch: 7},
			wakes: true, status: "in_progress",
		},
		{
			name:  "into an empty lane",
			args:  map[string]any{"item": "ENG-1", "status": "done", "if_match": 7},
			want:  tracker.Move{Task: "i1", Project: "ENG", IfMatch: 7},
			wakes: true, status: "done",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			spy := &moveSpy{}
			got, err := moveTool(t, spy).Call(t.Context(), c.args)
			if err != nil || got.Failed {
				t.Fatalf("move_work_item failed: %v %s", err, got.Output)
			}
			if spy.got == nil {
				t.Fatal("the tool never reached the mover")
			}
			move := *spy.got
			status := ""
			if move.Status != nil {
				status = string(*move.Status)
			}
			move.Status = nil
			if move != c.want || status != c.status {
				t.Fatalf("the tool handed the tracker %+v status %q, want %+v status %q",
					move, status, c.want, c.status)
			}
			if (spy.notify != nil) != c.wakes {
				t.Errorf("a lane change wakes the card's people and a reorder "+
					"wakes nobody — this move carried notify=%v", spy.notify != nil)
			}
		})
	}
}

// A MOVE THAT CANNOT MEAN ONE THING IS REFUSED BEFORE ANYTHING IS WRITTEN.
func TestAMoveThatMeansNothingIsRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"no if_match", map[string]any{"item": "ENG-1", "before": "ENG-2"}},
		{"both neighbours", map[string]any{"item": "ENG-1", "before": "ENG-2", "after": "ENG-3", "if_match": 7}},
		{"beside itself", map[string]any{"item": "ENG-1", "before": "ENG-1", "if_match": 7}},
		{"its own lane and nobody", map[string]any{"item": "ENG-1", "status": "todo", "if_match": 7}},
		{"a lane that is not one", map[string]any{"item": "ENG-1", "status": "shipped", "if_match": 7}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			spy := &moveSpy{}
			got, err := moveTool(t, spy).Call(t.Context(), c.args)
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if !got.Failed || got.Refusal != tools.RefusalInvalid {
				t.Fatalf("answered %+v, want an invalid refusal", got)
			}
			if spy.got != nil {
				t.Errorf("a refused move still reached the tracker: %+v", *spy.got)
			}
		})
	}
}

// A LANE THAT CHANGED AND A CARD THAT DID NOT TAKE ITS PLACE IS REPORTED, NOT
// FAILED — the status write landed, and a caller told "failed" would drag it
// back.
func TestAnUnplacedLaneChangeIsReportedNotFailed(t *testing.T) {
	t.Parallel()
	at := statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 9}
	spy := &moveSpy{result: tracker.MoveResult{
		Lane: tracker.WriteResult{Result: statelog.Result{
			Outcome: statelog.OutcomeApplied, Position: at, Version: at.Packed(),
		}},
		Unplaced: fmt.Errorf("%w: task i1 moved", tracker.ErrStaleVersion),
	}}
	got, err := moveTool(t, spy).Call(t.Context(), map[string]any{
		"item": "ENG-1", "before": "ENG-2", "status": "in_progress", "if_match": 7,
	})
	if err != nil || got.Failed {
		t.Fatalf("a lane change that landed answered failed: %v %s", err, got.Output)
	}
	answer := decodeAnswer(t, got)
	if answer["placed"] != false || !strings.Contains(fmt.Sprint(answer["unplaced"]), "changed the item") {
		t.Errorf("the answer does not say the card missed its place: %v", answer)
	}
	if answer["status"] != "in_progress" || answer["position"] != at.String() {
		t.Errorf("the answer does not report the lane change it made: %v", answer)
	}
	if answer["version"] != float64(at.Packed()) {
		t.Errorf("the answer's version is %v, want the lane change's %d — the "+
			"board's next if_match on the card", answer["version"], at.Packed())
	}
}
