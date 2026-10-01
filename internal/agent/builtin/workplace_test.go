package builtin_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// placeSpy is the board-drag seam, recording every drop it was handed.
type placeSpy struct {
	mu     sync.Mutex
	places []tracker.Place
	ops    []string
	notify *tracker.Notify
	result tracker.PlaceResult
	err    error
}

func (p *placeSpy) PlaceTask(_ context.Context, opID string, place tracker.Place,
	notify *tracker.Notify) (tracker.PlaceResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.places, p.ops, p.notify = append(p.places, place), append(p.ops, opID), notify
	return p.result, p.err
}

// last is the drop the spy was handed most recently, or nil.
func (p *placeSpy) last() *tracker.Place {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.places) == 0 {
		return nil
	}
	place := p.places[len(p.places)-1]
	return &place
}

// placeTool is place_work_item over the fake tracker and a spy, as the
// operator surface builds it, with the actor the given resolver answers.
func placeTool(t *testing.T, spy *placeSpy,
	actor func(context.Context, *turnctx.Turn) (builtin.Actor, error)) tools.Callable {

	t.Helper()
	work := newFakeTracker()
	if actor == nil {
		actor = func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
			return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator}, nil
		}
	}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: work, Writer: work.as,
			Placer: func(builtin.Actor) builtin.WorkPlacer { return spy },
			Actor:  actor,
		},
	}) {
		if tool.Name() == tracker.PlaceWorkItemTool {
			return tool
		}
	}
	t.Fatal("the operator surface does not serve place_work_item with a placer wired")
	return nil
}

// A DROP NAMES ITS NEIGHBOUR BY ID, AND ITS LANE AS DROPPED.
//
// A key typed by a caller is resolved before anything is written, because the
// tracker places beside a task id. The lane is handed over AS SENT, even when
// it is the card's own: the tracker — not the tool — decides that a drop into
// the card's own lane writes nothing on the task, because a tool that dropped
// the status there refused the retry of a drag whose lane change had already
// landed (the card read as in its new lane, and the placement was conditioned
// on the version read before that change). Only a lane the card LEAVES wakes
// anybody.
func TestADropNamesItsNeighbourByIDAndItsLaneAsDropped(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		args   map[string]any
		want   tracker.Place
		wakes  bool
		status string
	}{
		{
			name:   "within a lane",
			args:   map[string]any{"item": "ENG-1", "before": "ENG-2", "status": "todo", "if_match": 7},
			want:   tracker.Place{Task: "i1", Project: "ENG", Before: "id-2", IfMatch: 7},
			status: "todo",
		},
		{
			name:  "across lanes",
			args:  map[string]any{"item": "ENG-1", "after": "ENG-3", "status": "in_progress", "if_match": 7},
			want:  tracker.Place{Task: "i1", Project: "ENG", After: "id-3", IfMatch: 7},
			wakes: true, status: "in_progress",
		},
		{
			name:  "into an empty lane",
			args:  map[string]any{"item": "ENG-1", "status": "done", "if_match": 7},
			want:  tracker.Place{Task: "i1", Project: "ENG", IfMatch: 7},
			wakes: true, status: "done",
		},
		{
			name:   "into its own lane, beside nobody",
			args:   map[string]any{"item": "ENG-1", "status": "todo", "if_match": 7},
			want:   tracker.Place{Task: "i1", Project: "ENG", IfMatch: 7},
			status: "todo",
		},
		{
			name: "a reorder naming no lane",
			args: map[string]any{"item": "ENG-1", "before": "ENG-2", "if_match": 7},
			want: tracker.Place{Task: "i1", Project: "ENG", Before: "id-2", IfMatch: 7},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			spy := &placeSpy{}
			got, err := placeTool(t, spy, nil).Call(t.Context(), c.args)
			if err != nil || got.Failed {
				t.Fatalf("place_work_item failed: %v %s", err, got.Output)
			}
			place := spy.last()
			if place == nil {
				t.Fatal("the tool never reached the placer")
			}
			status := ""
			if place.Status != nil {
				status = string(*place.Status)
			}
			bare := *place
			bare.Status = nil
			if bare != c.want || status != c.status {
				t.Fatalf("the tool handed the tracker %+v status %q, want %+v status %q",
					bare, status, c.want, c.status)
			}
			if (spy.notify != nil) != c.wakes {
				t.Errorf("a lane change wakes the card's people and a drop that "+
					"leaves its lane alone wakes nobody — this drop carried "+
					"notify=%v", spy.notify != nil)
			}
		})
	}
}

// A DROP THAT CANNOT MEAN ONE THING IS REFUSED BEFORE ANYTHING IS WRITTEN.
func TestADropThatMeansNothingIsRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"no if_match", map[string]any{"item": "ENG-1", "before": "ENG-2"}},
		{"both neighbours", map[string]any{"item": "ENG-1", "before": "ENG-2", "after": "ENG-3", "if_match": 7}},
		{"beside itself", map[string]any{"item": "ENG-1", "before": "ENG-1", "if_match": 7}},
		{"no neighbour and no lane", map[string]any{"item": "ENG-1", "if_match": 7}},
		{"a lane that is not one", map[string]any{"item": "ENG-1", "status": "shipped", "if_match": 7}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			spy := &placeSpy{}
			got, err := placeTool(t, spy, nil).Call(t.Context(), c.args)
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if !got.Failed || got.Refusal != tools.RefusalInvalid {
				t.Fatalf("answered %+v, want an invalid refusal", got)
			}
			if place := spy.last(); place != nil {
				t.Errorf("a refused drop still reached the tracker: %+v", *place)
			}
		})
	}
}

// A LANE THAT CHANGED AND A CARD THAT DID NOT TAKE ITS PLACE IS REPORTED, NOT
// FAILED — the status write landed, and a caller told "failed" would drag it
// back. The version answered is the tracker's own, which is what the board's
// next `if_match` on the card states.
func TestAnUnplacedLaneChangeIsReportedNotFailed(t *testing.T) {
	t.Parallel()
	at := statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 9}
	spy := &placeSpy{result: tracker.PlaceResult{
		Lane: tracker.WriteResult{Result: statelog.Result{
			Outcome: statelog.OutcomeApplied, Position: at, Version: 8,
		}},
		Version:  8,
		Unplaced: fmt.Errorf("%w: task i1 moved", tracker.ErrStaleVersion),
	}}
	got, err := placeTool(t, spy, nil).Call(t.Context(), map[string]any{
		"item": "ENG-1", "before": "ENG-2", "status": "in_progress", "if_match": 7,
	})
	if err != nil || got.Failed {
		t.Fatalf("a lane change that landed answered failed: %v %s", err, got.Output)
	}
	answer := decodeAnswer(t, got)
	if answer["placed"] != false || !strings.Contains(fmt.Sprint(answer["unplaced"]), "changed the item") {
		t.Errorf("the answer does not say the card missed its place: %v", answer)
	}
	if answer["status"] != "in_progress" || answer["position"] != at.String() ||
		answer["outcome"] != string(statelog.OutcomeApplied) {
		t.Errorf("the answer does not report the lane change it made: %v", answer)
	}
	if answer["version"] != float64(8) {
		t.Errorf("the answer's version is %v, want the tracker's 8 — the "+
			"board's next if_match on the card", answer["version"])
	}
}

// A DROP WHOSE STEP ENDED UNKNOWN IS UNKNOWN, NEVER PLACED.
//
// Answered as a success carrying `outcome: unknown`, the rank and the lane
// beside it read as where the card now sits — a person's board would draw it
// there — when the step may never have landed. It is the failed answer every
// tracker write gives an unknown, naming the operation the retry repeats.
func TestAnUnknownDropStepIsAnsweredUnknown(t *testing.T) {
	t.Parallel()
	unknown := tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeUnknown}}
	applied := tracker.WriteResult{Result: statelog.Result{
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 4},
		Version:  8,
	}}
	for name, result := range map[string]tracker.PlaceResult{
		"the lane":  {Lane: unknown, Unplaced: fmt.Errorf("the lane's outcome is unknown")},
		"the order": {Lane: applied, Order: unknown, Rank: "a0V", Version: 8},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spy := &placeSpy{result: result}
			got, err := placeTool(t, spy, nil).Call(t.Context(), map[string]any{
				"item": "ENG-1", "before": "ENG-2", "status": "in_progress", "if_match": 7,
			})
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if !got.Failed || got.Refusal != tools.RefusalUnavailable {
				t.Fatalf("an unknown %s step answered %+v, want the failed unknown", name, got)
			}
			spy.mu.Lock()
			op := spy.ops[0]
			spy.mu.Unlock()
			for _, want := range []string{"is unknown", "do not report it as done",
				"ENG-1 dropped before ENG-2 into in_progress"} {
				if !strings.Contains(got.Output, want) {
					t.Errorf("the answer lacks %q: %s", want, got.Output)
				}
			}
			// THE OPERATION IS THE CALL'S, which the operator brings back.
			operation, _, _ := strings.Cut(op, ".place-")
			if !strings.Contains(got.Output, operation) {
				t.Errorf("the answer does not name the call's operation %q: %s",
					operation, got.Output)
			}
		})
	}
}

// THE SAME DROP MADE AGAIN IS THE SAME DROP — the same place handed to the
// tracker, status included, and the same operation — whichever way the call
// is made again: an operator's assistant bringing back its `op_id`, or a
// person's retry of one request at the dashboard. A different drop under the
// request is another operation.
func TestTheSameDropMadeAgainIsTheSameOperation(t *testing.T) {
	t.Parallel()
	args := func() map[string]any {
		return map[string]any{"item": "ENG-1", "before": "ENG-2", "status": "in_progress", "if_match": 7}
	}

	t.Run("an op_id brought back", func(t *testing.T) {
		t.Parallel()
		spy := &placeSpy{}
		tool := placeTool(t, spy, nil)
		first, err := tool.Call(t.Context(), args())
		if err != nil || first.Failed {
			t.Fatalf("the first drop failed: %v %s", err, first.Output)
		}
		again := args()
		again["op_id"] = decodeAnswer(t, first)["op_id"]
		second, err := tool.Call(t.Context(), again)
		if err != nil || second.Failed {
			t.Fatalf("the drop brought back failed: %v %s", err, second.Output)
		}
		sameDrop(t, spy)
	})

	t.Run("a request sent again", func(t *testing.T) {
		t.Parallel()
		request := statelog.NewOpID(time.Now(), "")
		spy := &placeSpy{}
		tool := placeTool(t, spy, func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
			op, err := builtin.RequestOperation("ops", request, tracker.PlaceWorkItemTool, args())
			return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator,
				Operation: op}, err
		})
		for range 2 {
			if got, err := tool.Call(t.Context(), args()); err != nil || got.Failed {
				t.Fatalf("the drop failed: %v %s", err, got.Output)
			}
		}
		sameDrop(t, spy)
	})
}

// sameDrop holds a spy's two drops to one place and one operation.
func sameDrop(t *testing.T, spy *placeSpy) {
	t.Helper()
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.places) != 2 {
		t.Fatalf("the placer was reached %d times, want 2", len(spy.places))
	}
	first, second := spy.places[0], spy.places[1]
	if first.Status == nil || second.Status == nil || *first.Status != *second.Status {
		t.Fatalf("the two drops sent lanes %v and %v — a retry sends the lane "+
			"it was dropped into, as the first attempt did", first.Status, second.Status)
	}
	first.Status, second.Status = nil, nil
	if first != second {
		t.Errorf("the same drop made again handed the tracker %+v, then %+v", first, second)
	}
	if spy.ops[0] != spy.ops[1] {
		t.Errorf("the same drop made again wrote under %q, then %q — a retry "+
			"is the first attempt's operation", spy.ops[0], spy.ops[1])
	}
}
