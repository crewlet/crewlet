package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// staleTasks answers one task read per item, and records each read's floors.
// An item in blocked reads with an open blocker, one in failing fails, and
// each reads its flow in flows, live as live says.
type staleTasks struct {
	tasks   map[string]worklet.Task
	blocked map[string]bool
	failing map[string]bool
	flows   map[string]*worklet.Flow
	live    bool
	err     error
	floors  [][]statelog.Position
}

func (s *staleTasks) Task(_ context.Context, id string, _ worklet.DetailWants,
	fresh statelog.Freshness) (worklet.TaskDetail, error) {
	s.floors = append(s.floors, fresh.Floors)
	if s.err != nil {
		return worklet.TaskDetail{}, s.err
	}
	if s.failing[id] {
		return worklet.TaskDetail{}, errors.New("the estate did not answer")
	}
	t, ok := s.tasks[id]
	if !ok {
		return worklet.TaskDetail{}, worklet.ErrNoTask
	}
	return worklet.TaskDetail{Task: t, Blocked: s.blocked[id], Flow: s.flows[id], FlowLive: s.live}, nil
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

// trackerAt is the position seq of the tracker's log.
func trackerAt(seq uint64) statelog.Position {
	return statelog.Position{Stream: "tracker", Generation: 1, Seq: seq}
}

// trackerNotice is the one wake a change to item, written by actor as kind and
// committed at seq of the tracker's log, puts on recipient's inbox: relayed as
// the tracker's feed relays it, routed by its parser and woken by the
// notification service.
func trackerNotice(t *testing.T, reg *notify.Registry, op, item, actor string, kind worklet.AuthorKind,
	n *worklet.Notify, seq uint64, recipient string) *events.Event {

	t.Helper()
	record := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			OpID: op, Subject: worklet.TaskSubject(item), Op: worklet.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
			Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: json.RawMessage(`{}`), Actor: actor, ActorKind: kind, Notify: n,
	}
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode %s: %v", op, err)
	}
	return noticeAt(t, reg, worklet.NewParser(worklet.ParserOptions{}), worklet.Prompt{}, payload,
		recipient, trackerAt(seq).String())
}

// blockingNotice is the mirror commit that tells blocker's assignee swe it now
// blocks dependents.
func blockingNotice(blocker string, dependents ...string) *worklet.Notify {
	task := worklet.Task{ID: blocker, Key: "ENG-1", Project: "ENG", Title: "ship the API",
		Status: worklet.StatusInProgress, Assignee: "swe"}
	parties := make([]worklet.TaskParty, len(dependents))
	for i, id := range dependents {
		parties[i] = worklet.TaskParty{Task: id, Key: "ENG-" + id, Assignee: "qa"}
	}
	return worklet.Wake{Kind: worklet.ChangeRelations, Before: task, After: task,
		Dependents: parties}.Notify(nil)
}

// unblockedNotice is the repair that tells item's assignee swe its blockers
// closed.
func unblockedNotice(item string) *worklet.Notify {
	return &worklet.Notify{Kind: worklet.ChangeRelations, Late: true,
		Snapshot: worklet.Snapshot{Key: "ENG-9", Project: "ENG",
			Unblocked: []worklet.TaskParty{{Task: item, Key: "ENG-9", Assignee: "swe"}}}}
}

// A WAKE WHOSE RELATION ENDED STARTS NO TURN (ADR-0162): a `blocking` wake
// carries the dependents its record names, and one whose every dependent was
// finished, removed or no longer waits on the item asks the seat nothing; nor
// does an `unblocked` wake on an item blocked again. A dependent still waiting,
// one a read could not settle and a wake naming none keep it, and every read is
// floored at the wake's own change. Mutation: judge a blocking wake by any one
// dependent, or treat one naming none as moot, or skip the blocked-again
// check, and it fails.
func TestAWakeWhoseRelationEndedStartsNoTurn(t *testing.T) {
	t.Parallel()
	reg := notify.NewRegistry(audienceChart(t), nil)
	waiting := []worklet.Relation{{Kind: worklet.RelationWaitingOn, Other: "b-1"}}
	tasks := &staleTasks{
		tasks: map[string]worklet.Task{
			"b-1":       {Assignee: "swe", Status: worklet.StatusInProgress},
			"d-done":    {Assignee: "qa", Status: worklet.StatusDone, Relations: waiting},
			"d-gone":    {Assignee: "qa", Status: worklet.StatusTodo, Removed: &worklet.Tombstone{}},
			"d-free":    {Assignee: "qa", Status: worklet.StatusTodo},
			"d-waiting": {Assignee: "qa", Status: worklet.StatusTodo, Relations: waiting},
			"d-unread":  {Assignee: "qa", Status: worklet.StatusTodo},
			"u-free":    {Assignee: "swe", Status: worklet.StatusTodo},
			"u-again":   {Assignee: "swe", Status: worklet.StatusTodo},
		},
		blocked: map[string]bool{"u-again": true},
		failing: map[string]bool{"d-unread": true},
	}
	blocking := func(op string, dependents ...string) *events.Event {
		return trackerNotice(t, reg, op, "b-1", "cto", worklet.AuthorAgent,
			blockingNotice("b-1", dependents...), 40, "swe")
	}
	unblocked := func(op, item string) *events.Event {
		return trackerNotice(t, reg, op, item, "crewlet", worklet.AuthorSystem, unblockedNotice(item), 40, "swe")
	}

	// THE WAKE CARRIES WHOM IT IS ABOUT, beside its own item.
	named := blocking("op-named", "d-done", "d-waiting")
	n, _ := events.DataAs[*types.ExternalNotification](named)
	if got := worklet.WakeDependents(n.Metadata); !slices.Equal(got, []string{"d-done", "d-waiting"}) {
		t.Fatalf("the blocking wake names dependents %v, want its record's d-done and d-waiting", got)
	}
	if reason, state := worklet.StateNotice(n); !state || reason != worklet.ReasonBlocking {
		t.Fatalf("the blocking wake reads as %q (state notice %v), want a blocking state notice", reason, state)
	}

	drop := map[string]*events.Event{
		"a dependent that finished":                blocking("op-1", "d-done"),
		"a dependent that was removed":             blocking("op-2", "d-gone"),
		"a dependent gone from here":               blocking("op-3", "d-purged"),
		"a dependent that no longer waits":         blocking("op-4", "d-free"),
		"every dependent ended, each its own way":  blocking("op-5", "d-done", "d-gone", "d-free"),
		"an item unblocked and then blocked again": unblocked("op-6", "u-again"),
	}
	keep := map[string]*events.Event{
		"a dependent still waiting":                 blocking("op-7", "d-waiting"),
		"one dependent ended and one still waiting": named,
		"a dependent that could not be read":        blocking("op-8", "d-unread", "d-done"),
		"a blocking wake that names no dependent":   blocking("op-9", "d-done"),
		"an item unblocked and still free":          unblocked("op-10", "u-free"),
	}
	// AN OLDER BUILD'S BLOCKING WAKE names none, and is no wake whose
	// dependents all ended.
	older, _ := events.DataAs[*types.ExternalNotification](keep["a blocking wake that names no dependent"])
	delete(older.Metadata, worklet.MetaDependents)
	turns := 0
	d := &Dispatcher{
		Conditions: func(string) inbox.Conditions {
			return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
		},
		Ledgered: inbox.Ledgered,
		StaleWakes: func(ctx context.Context, handle string, evs []*events.Event) []*events.Event {
			return staleWakes(ctx, tasks, handle, evs)
		},
		Turn: func(context.Context, Request) (turn.Result, error) {
			turns++
			return turn.Result{}, nil
		},
	}
	for what, wake := range drop {
		before := turns
		if got := d.Dispatch(t.Context(), "swe", []*events.Event{wake}); got.Outcome != queue.OutcomeAck ||
			turns != before {
			t.Errorf("%s: the wake answered %v after %d turns, want acked unrun", what, got.Outcome, turns-before)
		}
	}
	for what, wake := range keep {
		before := turns
		if d.Dispatch(t.Context(), "swe", []*events.Event{wake}); turns != before+1 {
			t.Errorf("%s: the wake ran %d turns, want one", what, turns-before)
		}
	}
	for _, floors := range tasks.floors {
		if len(floors) != 1 || floors[0] != trackerAt(40) {
			t.Fatalf("a read was floored at %v, want the wake's own change %v", floors, trackerAt(40))
		}
	}
}

// flowNotice is the one wake the flow duty's notice that item has been in
// state puts on its lead cto's inbox, committed at seq of the tracker's log:
// relayed, routed by the parser and woken by the notification service.
func flowNotice(t *testing.T, reg *notify.Registry, op, item string, state worklet.FlowState,
	seq uint64) *events.Event {

	t.Helper()
	since := time.Unix(1_700_000_000, 0).UTC()
	flow := worklet.FlowRecord{ID: "key-" + op, Task: item, State: state, Since: &since, Seat: "swe",
		Notice: worklet.FlowToldLead, Told: "cto"}
	body, err := json.Marshal(flow)
	if err != nil {
		t.Fatalf("encode %s: %v", op, err)
	}
	record := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			OpID: op, Subject: worklet.FlowSubject(item), Op: worklet.OpFlow,
			CreatedAt: since, Writer: "node-a",
			Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: body, Actor: "node-a", ActorKind: worklet.AuthorSystem,
		Notify: &worklet.Notify{Kind: worklet.ChangeFlow, Snapshot: worklet.Snapshot{
			Key: "ENG-" + item, Project: "ENG", Assignee: "swe", Task: item,
			Flow: &worklet.FlowNotice{ID: flow.ID, Told: "cto", Role: worklet.FlowToldLead,
				State: state, Since: &since},
		}},
	}
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode %s: %v", op, err)
	}
	return noticeAt(t, reg, worklet.NewParser(worklet.ParserOptions{}), worklet.Prompt{}, payload,
		"cto", trackerAt(seq).String())
}

// A FLOW NOTICE WHOSE ITEM LEFT ITS STATE STARTS NO TURN (ADR-0162): a lead
// told an item is stuck, due or held is asked to look at it, which an item
// finished, removed or moving again no longer asks. Judged only where the read
// can tell: a read with no live facts keeps a notice of a hold it cannot see,
// and an item still in the state it told of keeps it. Mutation: drop a notice
// whatever its item reads, or keep one whose item moved on, and it fails.
func TestAFlowNoticeWhoseItemLeftItsStateStartsNoTurn(t *testing.T) {
	t.Parallel()
	reg := notify.NewRegistry(audienceChart(t), nil)
	open := worklet.Task{Assignee: "swe", Status: worklet.StatusInProgress}
	flowOf := func(state worklet.FlowState) *worklet.Flow { return &worklet.Flow{State: state} }
	tasks := func(live bool) *staleTasks {
		return &staleTasks{live: live,
			tasks: map[string]worklet.Task{
				"stuck": open, "moving": open, "held": open, "blocked": open,
				"done": {Assignee: "swe", Status: worklet.StatusDone},
				"todo": {Assignee: "swe", Status: worklet.StatusTodo},
			},
			flows: map[string]*worklet.Flow{"stuck": flowOf(worklet.FlowStuck),
				"moving": flowOf(worklet.FlowMoving), "held": flowOf(worklet.FlowDue),
				"blocked": flowOf(worklet.FlowBlocked)},
		}
	}

	// THE WAKE SAYS WHAT IT TELLS OF, and is a state notice its item's turn absorbs.
	told := flowNotice(t, reg, "op-told", "stuck", worklet.FlowStuck, 40)
	n, _ := events.DataAs[*types.ExternalNotification](told)
	if state, ok := worklet.WakeFlow(n.Metadata); !ok || state != worklet.FlowStuck {
		t.Fatalf("the flow notice tells of %q (%v), want stuck", state, ok)
	}
	if reason, state := worklet.StateNotice(n); !state || reason != worklet.ReasonFlow ||
		!reason.Absorbable() {
		t.Fatalf("the flow notice reads as %q (state notice %v), want an absorbable flow notice",
			reason, state)
	}

	for _, c := range []struct {
		what  string
		item  string
		state worklet.FlowState
		live  bool
		moot  bool
	}{
		{"an item still stuck", "stuck", worklet.FlowStuck, true, false},
		{"an item moving again", "moving", worklet.FlowStuck, true, true},
		{"an item moving again, read without live facts", "moving", worklet.FlowStuck, false, true},
		{"an item finished", "done", worklet.FlowDue, true, true},
		{"an item back to do", "todo", worklet.FlowDue, false, true},
		{"a hold a read without live facts cannot see", "held", worklet.FlowHeld, false, false},
		{"a hold that ended", "held", worklet.FlowHeld, true, true},
		{"a hold behind a blocker", "blocked", worklet.FlowHeld, false, true},
	} {
		wake := flowNotice(t, reg, "op-"+c.item, c.item, c.state, 40)
		got := staleWakes(t.Context(), tasks(c.live), "cto", []*events.Event{wake})
		if moot := len(got) == 0; moot != c.moot {
			t.Errorf("%s: the notice is moot %v, want %v", c.what, moot, c.moot)
		}
	}
}
