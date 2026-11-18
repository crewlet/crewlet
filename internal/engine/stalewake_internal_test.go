package engine

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// staleTasks answers one task read per item, and records each read's floors.
type staleTasks struct {
	tasks  map[string]worklet.Task
	err    error
	floors [][]statelog.Position
}

func (s *staleTasks) Task(_ context.Context, id string, _ worklet.DetailWants,
	fresh statelog.Freshness) (worklet.TaskDetail, error) {
	s.floors = append(s.floors, fresh.Floors)
	if s.err != nil {
		return worklet.TaskDetail{}, s.err
	}
	t, ok := s.tasks[id]
	if !ok {
		return worklet.TaskDetail{}, worklet.ErrNoTask
	}
	return worklet.TaskDetail{Task: t}, nil
}

// staleWakeAt is the position a test wake's change was committed at.
var staleWakeAt = statelog.Position{Stream: "tracker", Generation: 1, Seq: 40}

// workletWake is a Worklet wake to seat cy about item id, under reason, for a
// change of kind, committed at [staleWakeAt].
func workletWake(id string, reason worklet.Reason, kind worklet.ChangeKind) *events.Event {
	ev := events.New(types.ExternalNotification{NotificationSource: worklet.Source,
		SourceEventType: string(kind), Metadata: map[string]string{
			worklet.MetaVia: string(reason), worklet.MetaTaskID: id}}, events.TraceContext{})
	ev.Payload = map[string]any{notify.TriggerField: staleWakeAt.String()}
	return ev
}

// A WAKE ABOUT WORK THE SEAT NO LONGER HOLDS STARTS NO TURN: an assignment, a
// new dependent on the seat's item (`blocking`) or its blocker's close
// (`unblocked`), once the item was finished, removed or handed on. A comment,
// another reason and an unread item still run. Mutation: drop by the reason
// alone, and a comment on a finished item goes unanswered; check `assignee`
// alone, and a day-old relation notice on finished work costs a turn; skip the
// floor, and it fails.
func TestAWakeAboutWorkTheSeatNoLongerHoldsStartsNoTurn(t *testing.T) {
	t.Parallel()
	tasks := &staleTasks{tasks: map[string]worklet.Task{
		"open":     {Assignee: "cy", Status: worklet.StatusTodo},
		"done":     {Assignee: "cy", Status: worklet.StatusDone},
		"handed":   {Assignee: "dee", Status: worklet.StatusTodo},
		"archived": {Assignee: "cy", Status: worklet.StatusTodo, Removed: &worklet.Tombstone{}},
	}}
	keep := map[string]*events.Event{
		"an open item of the seat's":         workletWake("open", worklet.ReasonAssignee, worklet.ChangeStatus),
		"a new dependent on an open item":    workletWake("open", worklet.ReasonBlocking, worklet.ChangeRelations),
		"an open item unblocked":             workletWake("open", worklet.ReasonUnblocked, worklet.ChangeRelations),
		"a comment on a finished item":       workletWake("done", worklet.ReasonAssignee, worklet.ChangeComment),
		"a mention on a finished item":       workletWake("done", worklet.ReasonMention, worklet.ChangeComment),
		"a watched item handed to others":    workletWake("handed", worklet.ReasonWatcher, worklet.ChangeAssignee),
		"the reporter of a finished item":    workletWake("done", worklet.ReasonReporter, worklet.ChangeStatus),
		"an answer on an item handed to dee": workletWake("handed", worklet.ReasonAnswered, worklet.ChangeComment),
	}
	drop := map[string]*events.Event{
		"a finished item":                  workletWake("done", worklet.ReasonAssignee, worklet.ChangeStatus),
		"an item handed to dee":            workletWake("handed", worklet.ReasonAssignee, worklet.ChangeCreated),
		"a removed item":                   workletWake("archived", worklet.ReasonAssignee, worklet.ChangeFields),
		"an item gone from here":           workletWake("purged", worklet.ReasonAssignee, worklet.ChangeStatus),
		"a new dependent on finished work": workletWake("done", worklet.ReasonBlocking, worklet.ChangeRelations),
		"a new dependent on handed work":   workletWake("handed", worklet.ReasonBlocking, worklet.ChangeRelations),
		"finished work unblocked":          workletWake("done", worklet.ReasonUnblocked, worklet.ChangeRelations),
		"handed work unblocked":            workletWake("handed", worklet.ReasonUnblocked, worklet.ChangeRelations),
	}
	var evs []*events.Event
	for _, ev := range keep {
		evs = append(evs, ev)
	}
	for _, ev := range drop {
		evs = append(evs, ev)
	}
	got := staleWakes(t.Context(), tasks, "cy", evs)
	for name, ev := range keep {
		if !slices.Contains(got, ev) {
			t.Errorf("%s: the wake was dropped, want it to run", name)
		}
	}
	for name, ev := range drop {
		if slices.Contains(got, ev) {
			t.Errorf("%s: the wake runs a turn, want it dropped", name)
		}
	}
	for _, floors := range tasks.floors {
		if len(floors) != 1 || floors[0] != staleWakeAt {
			t.Fatalf("a read was floored at %v, want the wake's own change %v", floors, staleWakeAt)
		}
	}

	// A READ THAT FAILS, OR A WAKE THAT NAMES NO CHANGE, KEEPS THE WAKE.
	unread := &staleTasks{err: errors.New("the estate did not answer")}
	if got := staleWakes(t.Context(), unread, "cy", []*events.Event{drop["a finished item"]}); len(got) != 1 {
		t.Error("a wake whose item could not be read was dropped")
	}
	bare := workletWake("done", worklet.ReasonAssignee, worklet.ChangeStatus)
	bare.Payload = nil
	if got := staleWakes(t.Context(), tasks, "cy", []*events.Event{bare}); len(got) != 1 {
		t.Error("a wake with no committing position was dropped")
	}
}
