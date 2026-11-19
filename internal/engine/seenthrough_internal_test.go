package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/pickup"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/worklet"
)

// markedSeat is a dispatcher for seat swe with a completion ledger and a
// seen-through mark, counting the turns it runs and the triggers it absorbed.
type markedSeat struct {
	d        *Dispatcher
	turns    int
	absorbed []string
}

func newMarkedSeat() *markedSeat {
	s := &markedSeat{}
	s.d = &Dispatcher{
		Conditions: func(string) inbox.Conditions {
			return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
		},
		Ledgered:    inbox.Ledgered,
		Completions: ledgerstore.NewMemoryCompletions(),
		SeenThrough: ledgerstore.NewMemorySeenThrough(),
		Observe: func(_ context.Context, ev *events.Event) {
			if k, ok := events.DataAs[*types.TurnTriggerSkipped](ev); ok &&
				k.Disposition == types.DispositionAbsorbed {
				s.absorbed = append(s.absorbed, k.TriggerID)
			}
		},
		Turn: func(context.Context, Request) (turn.Result, error) {
			s.turns++
			return turn.Result{}, nil
		},
	}
	return s
}

// drain dispatches evs as the seat's inbox does, one partition per stamped
// key and writer, and answers how many turns they ran.
func (s *markedSeat) drain(t *testing.T, evs ...*events.Event) int {
	t.Helper()
	before := s.turns
	parts := queue.PartitionByKey(evs, notify.KeyOf, func(ev *events.Event) *events.Event { return ev })
	for _, part := range parts {
		if got := s.d.Dispatch(t.Context(), "swe", part.Items); got.Outcome != queue.OutcomeAck {
			t.Fatalf("a partition answered %v, want an ack", got.Outcome)
		}
	}
	return s.turns - before
}

// itemTask is item t-1 as its notices name it, assigned to swe.
func itemTask(id string, status worklet.Status) worklet.Task {
	return worklet.Task{ID: id, Key: "ENG-1", Project: "ENG", Title: "ship the API",
		Status: status, Assignee: "swe"}
}

// statusNotice is a status change on item by somebody other than its assignee
// swe, carrying comment when one is given.
func statusNotice(item string, comment *worklet.Comment) *worklet.Notify {
	return worklet.Wake{Kind: worklet.ChangeStatus, Before: itemTask(item, worklet.StatusTodo),
		After: itemTask(item, worklet.StatusInProgress), Comment: comment}.Notify(nil)
}

// personMention is ada's comment on item asking its assignee swe.
func personMention(item, id string) *worklet.Notify {
	task := itemTask(item, worklet.StatusInProgress)
	return worklet.Wake{Kind: worklet.ChangeComment, Before: task, After: task,
		Comment:  &worklet.Comment{ID: id, Body: "@swe can you take the API first", AuthorKind: worklet.AuthorHuman},
		Mentions: []string{"swe"}}.Notify(nil)
}

// A STATE NOTICE A TURN ON ITS ITEM READ PAST STARTS NO TURN (ADR-0162): once a
// turn woken at position 40 of item t-1's conversation completes, the queued
// assignment, new dependent and unblock on t-1 at or below 40, each from
// another writer, are absorbed and counted so. Words still run (a person's
// mention, a status change carrying a comment), as do a reporter's notice, a
// notice past the mark and one on another item. Mutation: keep every addressed
// notice an obligation, and the state notices each run a turn; absorb by the
// reason alone, and the comment on a status change goes unread.
func TestAStateNoticeATurnOnItsItemReadPastStartsNoTurn(t *testing.T) {
	t.Parallel()
	reg := notify.NewRegistry(audienceChart(t), nil)
	seat := newMarkedSeat()

	if got := seat.drain(t, trackerNotice(t, reg, "op-ask", "t-1", "ada", worklet.AuthorHuman,
		personMention("t-1", "c-1"), 40, "swe")); got != 1 {
		t.Fatalf("the person's mention ran %d turns, want one", got)
	}
	if mark := seat.d.SeenThrough.Mark(t.Context(), "swe", worklet.ItemConversation("t-1"),
		"tracker"); mark != trackerAt(40) {
		t.Fatalf("the completed turn marked t-1's conversation at %v, want %v", mark, trackerAt(40))
	}

	queued := []*events.Event{
		trackerNotice(t, reg, "op-start", "t-1", "cto", worklet.AuthorAgent, statusNotice("t-1", nil), 35, "swe"),
		trackerNotice(t, reg, "op-dep", "t-1", "qa", worklet.AuthorAgent, blockingNotice("t-1", "d-1"), 38, "swe"),
		trackerNotice(t, reg, "op-free", "t-1", "crewlet", worklet.AuthorSystem, unblockedNotice("t-1"), 39, "swe"),
		trackerNotice(t, reg, "op-again", "t-1", "cto", worklet.AuthorAgent, statusNotice("t-1", nil), 40, "swe"),
	}
	for _, ev := range queued {
		n, _ := events.DataAs[*types.ExternalNotification](ev)
		if _, state := worklet.StateNotice(n); !state {
			t.Fatalf("the fixture's %s is no state notice: %v", n.Metadata[worklet.MetaVia], n.Metadata)
		}
	}
	if got := seat.drain(t, queued...); got != 0 {
		t.Errorf("the state notices the turn read past ran %d turns, want none", got)
	}
	want := make([]string, len(queued))
	for i, ev := range queued {
		want[i] = ev.ID.String()
	}
	slices.Sort(want)
	slices.Sort(seat.absorbed)
	if !slices.Equal(seat.absorbed, want) {
		t.Errorf("absorbed %v, want each queued state notice counted absorbed %v", seat.absorbed, want)
	}

	reporter := worklet.Wake{Kind: worklet.ChangeStatus,
		Before: worklet.Task{ID: "t-1", Key: "ENG-1", Project: "ENG", Status: worklet.StatusInProgress, StatusGroup: worklet.GroupActive,
			Assignee: "qa", Reporter: "swe"},
		After: worklet.Task{ID: "t-1", Key: "ENG-1", Project: "ENG", Status: worklet.StatusDone,
			StatusGroup: worklet.GroupDone, Assignee: "qa", Reporter: "swe"}}.Notify(nil)
	for what, wake := range map[string]*events.Event{
		"a person's mention the turn read past": trackerNotice(t, reg, "op-ask-2", "t-1", "ada",
			worklet.AuthorHuman, personMention("t-1", "c-2"), 37, "swe"),
		"a status change carrying a comment": trackerNotice(t, reg, "op-said", "t-1", "cto",
			worklet.AuthorAgent, statusNotice("t-1", &worklet.Comment{ID: "c-3", Body: "started, see notes",
				AuthorKind: worklet.AuthorAgent}), 36, "swe"),
		"the reporter's notice of a finish": trackerNotice(t, reg, "op-done", "t-1", "qa",
			worklet.AuthorAgent, reporter, 34, "swe"),
		"a state notice past the mark": trackerNotice(t, reg, "op-later", "t-1", "cto",
			worklet.AuthorAgent, statusNotice("t-1", nil), 41, "swe"),
		"a state notice on another item": trackerNotice(t, reg, "op-other", "t-2", "cto",
			worklet.AuthorAgent, statusNotice("t-2", nil), 30, "swe"),
	} {
		if got := seat.drain(t, wake); got != 1 {
			t.Errorf("%s ran %d turns, want one", what, got)
		}
	}
}

// A PICKUP MOVES ITS ITEM'S SEEN-THROUGH MARK (ADR-0162): a pickup is filed
// under the item's own Worklet conversation and carries the position of the
// change its key names, so its completed turn marks the item read through
// there, and the state notices queued on the item at or below it start no
// turn. A pickup whose key names no change moves nothing. Mutation: stamp the
// pickup `task:<id>`, or drop its position, and the queued notices each run.
func TestAPickupMovesItsItemsSeenThroughMark(t *testing.T) {
	t.Parallel()
	reg := notify.NewRegistry(audienceChart(t), nil)
	seat := newMarkedSeat()
	now := time.Unix(1_700_000_600, 0).UTC()
	handed := func(id string, moved uint64) *events.Event {
		item := worklet.ReadyItem{TaskRow: worklet.TaskRow{ID: id, Key: "ENG-1",
			Status: worklet.StatusInProgress}, Why: types.PickupQueue, MovedAt: now.Add(-time.Hour)}
		if moved != 0 {
			item.Moved, item.MovedOn = moved, trackerAt(moved)
		}
		seatID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("swe"))
		return pickup.Event(seatID, "SWE", item, pickup.KeyOf(seatID, item, now), 1, false,
			events.NewTrace())
	}
	queued := func(item string) []*events.Event {
		return []*events.Event{
			trackerNotice(t, reg, "op-start-"+item, item, "cto", worklet.AuthorAgent,
				statusNotice(item, nil), 40, "swe"),
			trackerNotice(t, reg, "op-dep-"+item, item, "qa", worklet.AuthorAgent,
				blockingNotice(item, "d-1"), 33, "swe"),
		}
	}

	picked := handed("t-1", 40)
	wakes := queued("t-1")
	if got, want := notify.ConversationIdentityOf(picked), notify.ConversationIdentityOf(wakes[0]); got != want {
		t.Fatalf("the pickup continues conversation %q, want its item's own %q", got, want)
	}
	if got := seat.drain(t, picked); got != 1 {
		t.Fatalf("the pickup ran %d turns, want one", got)
	}
	if mark := seat.d.SeenThrough.Mark(t.Context(), "swe", worklet.ItemConversation("t-1"),
		"tracker"); mark != trackerAt(40) {
		t.Fatalf("the pickup's turn marked t-1 at %v, want the change its key names %v", mark, trackerAt(40))
	}
	if got := seat.drain(t, wakes...); got != 0 {
		t.Errorf("the notices queued on the item the pickup's turn read past ran %d turns, want none", got)
	}

	if got := seat.drain(t, handed("t-2", 0)); got != 1 {
		t.Fatalf("the pickup naming no change ran %d turns, want one", got)
	}
	if got := seat.drain(t, queued("t-2")[1]); got != 1 {
		t.Errorf("after a pickup naming no change, a notice on its item ran %d turns, want one", got)
	}
}
