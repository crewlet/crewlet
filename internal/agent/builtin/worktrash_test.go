package builtin_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A SUBTREE GESTURE THAT STOPPED PART OF THE WAY THROUGH IS REPORTED AS WHAT
// IT IS, AND FINISHED FROM THE TOOL.
//
// Both answered the root's receipt whatever the walk did — "restored: true"
// over descendants still in the trash — or, when the walk returned an error,
// the default failure's "The change was NOT made" about a root that had moved.
// And the restore could not be finished from the tool at all: the root was
// back, so the next call was refused as "not in the trash" before the tracker
// was asked, and everything that went with it stayed there.

// trashSpy is the trash's write side, answering what a case sets.
type trashSpy struct {
	answers  []error
	restores int
	removes  int
}

func (s *trashSpy) next() error {
	if len(s.answers) == 0 {
		return nil
	}
	err := s.answers[0]
	s.answers = s.answers[1:]
	return err
}

func (s *trashSpy) RemoveTask(context.Context, string, string, string, bool,
	*tracker.Notify) (tracker.WriteResult, error) {

	s.removes++
	return trashReceipt(), s.next()
}

func (s *trashSpy) RestoreTask(context.Context, string, string, string,
	*tracker.Notify) (tracker.WriteResult, error) {

	s.restores++
	return trashReceipt(), s.next()
}

func trashReceipt() tracker.WriteResult {
	return tracker.WriteResult{Result: statelog.Result{
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 61},
		Version:  61,
	}}
}

// trashSurface is the operator catalogue — the one these tools are served on.
func trashSurface(t *testing.T, spy *trashSpy) *tools.Registry {
	t.Helper()
	trk := newFakeTracker()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Actor: operatorActor,
			TrashWriter: func(builtin.Actor) builtin.TrashWriter { return spy },
		},
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

func stoppedWalk(verb string, err error) error {
	return &tracker.SubtreeStopped{Verb: verb, Root: "i1", Followed: 1, Of: 3,
		OpID: "op", Err: err}
}

func TestAStoppedRestoreIsReportedAndFinishedFromTheTool(t *testing.T) {
	t.Parallel()
	spy := &trashSpy{answers: []error{
		stoppedWalk("restore", fmt.Errorf("stopped: %w", tracker.ErrStepUnresolved)),
	}}
	reg := trashSurface(t, spy)

	// ENG-1 IS LIVE in the fake — the state a stopped restore leaves its
	// root in, and the one the tool used to refuse before asking.
	got := callNoTurn(t, reg, tracker.RestoreWorkItemTool, map[string]any{"item": "ENG-1"})
	if got.Failed {
		t.Fatalf("a restore whose root landed was reported as a failure: %s", got.Output)
	}
	answer := answerOf(t, got)
	stopped, _ := answer["subtree_stopped"].(string)
	if answer["restored"] != true || answer["subtree_followed"] != float64(1) ||
		answer["subtree_total"] != float64(3) {
		t.Errorf("the answer is %v, want the root restored with 1 of 3 followed", answer)
	}
	for _, want := range []string{"Do not report it as done",
		"restore_work_item on ENG-1 again"} {
		if !strings.Contains(stopped, want) {
			t.Errorf("subtree_stopped lacks %q: %s", want, stopped)
		}
	}
	if strings.Contains(got.Output, "NOT made") {
		t.Errorf("the answer says the change was not made: %s", got.Output)
	}

	// AND THE NEXT CALL REACHES THE TRACKER, on a root that is back.
	if got := callNoTurn(t, reg, tracker.RestoreWorkItemTool,
		map[string]any{"item": "ENG-1"}); got.Failed {
		t.Fatalf("finishing the restore from the tool failed: %s", got.Output)
	}
	if spy.restores != 2 {
		t.Errorf("the tracker was asked %d time(s), want the second call to "+
			"reach it rather than be refused as not in the trash", spy.restores)
	}
}

func TestARestoreWithNothingToBringBackIsRefusedAsSuch(t *testing.T) {
	t.Parallel()
	spy := &trashSpy{answers: []error{
		fmt.Errorf("tracker: task i1 is not in the trash: %w", tracker.ErrNothingToRestore),
	}}
	got := callNoTurn(t, trashSurface(t, spy), tracker.RestoreWorkItemTool,
		map[string]any{"item": "ENG-1"})
	if !got.Failed || !strings.Contains(got.Output, "nothing to restore") ||
		strings.Contains(got.Output, "NOT made") {
		t.Errorf("a restore with nothing to bring back gave %q", got.Output)
	}
}

func TestAStoppedRemovalIsReportedWithWhatFinishesIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"unknown": {fmt.Errorf("stopped: %w", tracker.ErrStepUnresolved),
			"remove_work_item on ENG-1 again, with the same arguments"},
		// A STEP THIS NODE CANNOT VOUCH FOR is not finished by the same
		// operation, which stops at the same task here every time — only
		// a new one decides it afresh.
		"unvouched": {fmt.Errorf("stopped: %w", tracker.ErrStepUnvouched),
			"WITHOUT an op_id"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spy := &trashSpy{answers: []error{stoppedWalk("removal", tc.err)}}
			got := callNoTurn(t, trashSurface(t, spy), tracker.RemoveWorkItemTool,
				map[string]any{"item": "ENG-1", "subtree": true})
			if got.Failed {
				t.Fatalf("a removal whose root landed was reported as a failure: %s",
					got.Output)
			}
			answer := answerOf(t, got)
			stopped, _ := answer["subtree_stopped"].(string)
			if answer["removed"] != true || !strings.Contains(stopped, tc.want) {
				t.Errorf("the answer is %v, want the root removed and %q", answer, tc.want)
			}
			if _, held := answer["op_id"]; !held {
				t.Errorf("the answer carries no op_id to finish it under: %v", answer)
			}
		})
	}
}
