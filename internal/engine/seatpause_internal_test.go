package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// pausingEngine is an engine with a queue, a fleet store and its pause watch
// running — the three things the pause is made of.
func pausingEngine(t *testing.T) (*Engine, *memory.Queue, *coordmem.Fleet) {
	t.Helper()
	e, q := engineOn(t)
	fleet := coordmem.NewFleet()
	e.backends.Fleet = fleet
	e.startSeatPauses(t.Context())
	t.Cleanup(e.stopSeatPauses)
	if _, _, known := e.pauseOf("ceo"); !known {
		t.Fatal("the pause watch gave no first answer within its budget on a healthy store")
	}
	return e, q.(*memory.Queue), fleet
}

func pauseSeat(t *testing.T, f coord.SeatPauses, handle string, stop bool) coord.SeatPause {
	t.Helper()
	return pauseSeatCtx(t.Context(), t, f, handle, stop)
}

func pauseSeatCtx(ctx context.Context, t *testing.T, f coord.SeatPauses, handle string, stop bool) coord.SeatPause {
	t.Helper()
	p, created, err := f.CreateSeatPause(ctx, coord.SeatPause{
		Handle: handle, By: "jane-token", Seat: "jane", Reason: "looping",
		StopRunning: stop, At: time.Now(),
	})
	if err != nil || !created {
		t.Fatalf("CreateSeatPause(%s) = (%v, %v)", handle, created, err)
	}
	return p
}

func held(q *memory.Queue, handle string) bool {
	return slices.Contains(q.PauseHolds(topics.AgentInbox(handle),
		topics.AgentInboxGroup(handle)), seatPauseHold)
}

// seatTurns runs a seat's inbox through a dispatcher reading THIS engine's
// pause copy, and records the turns it starts.
type seatTurns struct {
	mu   sync.Mutex
	work []string
}

func (s *seatTurns) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.work)
}

func attachSeat(t *testing.T, e *Engine, q *memory.Queue, handle string) *seatTurns {
	t.Helper()
	turns := &seatTurns{}
	d := &Dispatcher{
		Conditions: func(h string) inbox.Conditions {
			_, paused, known := e.pauseOf(h)
			return inbox.Conditions{
				Owned: true, TurnEngineReady: true, AdmitsTriggers: true,
				Paused: paused, PauseUnknown: !known,
			}
		},
		Ledgered: inbox.Ledgered,
		Park:     e.park,
		Pause:    e.holdInbox,
		Turn: func(_ context.Context, req Request) (turn.Result, error) {
			turns.mu.Lock()
			defer turns.mu.Unlock()
			for _, ev := range req.Events {
				if task, ok := events.DataAs[*types.TaskAssigned](ev); ok {
					turns.work = append(turns.work, task.TaskID)
				}
			}
			return turn.Result{}, nil
		},
	}
	if err := q.SubscribeBatch(t.Context(), topics.AgentInbox(handle),
		topics.AgentInboxGroup(handle),
		func(ctx context.Context, evs []*events.Event) queue.Result {
			return d.Dispatch(ctx, handle, evs)
		},
		func(*events.Event) string { return "one-conversation" }, nil); err != nil {
		t.Fatalf("SubscribeBatch: %v", err)
	}
	return turns
}

func sendTask(t *testing.T, q queue.EventQueue, handle, id string) {
	t.Helper()
	if err := q.Publish(t.Context(), topics.AgentInbox(handle),
		events.New(types.TaskAssigned{TaskID: id, RoleName: handle}, events.TraceContext{})); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

// A PAUSED SEAT STARTS NO NEW TURN AND LOSES NO DELIVERY — and the resume
// delivers what waited, in order.
//
// Both routes a delivery can take onto a paused seat: the ordinary one, where
// the watch has already taken the hold and the mail simply waits on the
// broker, and the RACE, where a delivery was already on its way when the pause
// landed and the screening holds and parks it. Neither may run a turn, and
// neither may drop the mail.
func TestAPausedSeatStartsNoNewTurnAndLosesNoDelivery(t *testing.T) {
	t.Parallel()
	e, q, fleet := pausingEngine(t)
	turns := attachSeat(t, e, q, "ceo")

	sendTask(t, q, "ceo", "before")
	waitFor(t, "the unpaused seat to work", func() bool { return len(turns.seen()) == 1 })

	p := pauseSeat(t, fleet, "ceo", false)
	waitFor(t, "the watch to hold the inbox", func() bool { return held(q, "ceo") })
	sendTask(t, q, "ceo", "during-1")
	sendTask(t, q, "ceo", "during-2")
	time.Sleep(50 * time.Millisecond)
	if got := turns.seen(); len(got) != 1 {
		t.Fatalf("a paused seat ran %v", got)
	}

	if gone, err := fleet.DeleteSeatPause(t.Context(), "ceo", p.Version); err != nil || !gone {
		t.Fatalf("DeleteSeatPause = (%v, %v)", gone, err)
	}
	waitFor(t, "the resume to deliver what waited", func() bool { return len(turns.seen()) == 3 })
	if got := turns.seen(); !slices.Equal(got, []string{"before", "during-1", "during-2"}) {
		t.Errorf("turns = %v, want the held mail in the order it was sent", got)
	}
	if held(q, "ceo") {
		t.Error("the resume left the pause hold on the inbox")
	}
}

// THE RACE, from the delivery's side: the copy says paused and no hold is on
// the inbox yet. The screening takes the pause's own hold and parks the mail,
// so nothing runs and nothing is lost — and a hold the resume then lifts.
func TestAPausedSeatParksAndHoldsItsInboxWhenADeliveryRacesTheHold(t *testing.T) {
	t.Parallel()
	e, q, fleet := pausingEngine(t)
	p := pauseSeat(t, fleet, "ceo", false)
	waitFor(t, "the copy to see the pause", func() bool {
		_, paused, _ := e.pauseOf("ceo")
		return paused
	})
	turns := attachSeat(t, e, q, "ceo")
	// The watch took the hold already. Dropping it here is the state a
	// delivery races into: the pause has reached this node's copy and the
	// inbox is not held yet.
	if err := q.ResumeTopic(t.Context(), topics.AgentInbox("ceo"),
		topics.AgentInboxGroup("ceo"), seatPauseHold); err != nil {
		t.Fatalf("ResumeTopic: %v", err)
	}
	sendTask(t, q, "ceo", "raced")
	waitFor(t, "the screening to take the hold", func() bool { return held(q, "ceo") })
	time.Sleep(50 * time.Millisecond)
	if got := turns.seen(); len(got) != 0 {
		t.Fatalf("a delivery that raced the pause ran %v", got)
	}

	if gone, err := fleet.DeleteSeatPause(t.Context(), "ceo", p.Version); err != nil || !gone {
		t.Fatalf("DeleteSeatPause = (%v, %v)", gone, err)
	}
	waitFor(t, "the parked mail after the resume", func() bool {
		return slices.Equal(turns.seen(), []string{"raced"})
	})
}

// A PAUSED SEAT ACQUIRED HERE IS ATTACHED ALREADY HELD, and one that is not
// paused is attached with nothing — the node's pre-attach question.
func TestAPausedSeatIsAttachedUnderItsHold(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	if got := e.attachHolds("ceo"); len(got) != 0 {
		t.Fatalf("attachHolds(unpaused) = %v, want none", got)
	}
	pauseSeat(t, fleet, "ceo", false)
	waitFor(t, "the copy to see the pause", func() bool {
		_, paused, _ := e.pauseOf("ceo")
		return paused
	})
	if got := e.attachHolds("ceo"); !slices.Equal(got, []string{string(inbox.HoldSeatPaused)}) {
		t.Errorf("attachHolds(paused) = %v, want the pause's hold", got)
	}
}

// A STOP ENDS THE TURN AT ITS NEXT ROUND, AND THE TRIGGER IS NOT RUN AGAIN.
//
// The fence is checked at the top of every round; a pause asking for the
// running turn to stop closes it there, with [turn.ErrStoppedByPerson], so the
// round in flight finishes and the next never starts. The dispatcher then
// SPENDS the trigger — records it worked and acks it — because a NAK would run
// the turn a person stopped again the moment they resumed the seat, and says
// on the record who stopped it.
func TestStopEndsTheTurnAtTheNextRoundAndDoesNotReRunIt(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	var (
		mu       sync.Mutex
		rounds   int
		observed []*events.Event
	)
	d := &Dispatcher{
		Ledgered:    inbox.Ledgered,
		Completions: ledgerstore.NewFleetCompletions(fleet),
		Identify:    func(string) (string, string) { return "CEO", "agent-1" },
		Observe: func(_ context.Context, ev *events.Event) {
			mu.Lock()
			defer mu.Unlock()
			observed = append(observed, ev)
		},
		Turn: func(ctx context.Context, req Request) (turn.Result, error) {
			fence := e.seatFence(req.Handle)
			for round := range 5 {
				if err := fence(); err != nil {
					return turn.Result{}, err
				}
				mu.Lock()
				rounds++
				mu.Unlock()
				if round == 0 {
					// Mid-turn, the person pauses with a stop.
					pauseSeatCtx(ctx, t, fleet, req.Handle, true)
					waitFor(t, "the copy to see the stop", func() bool {
						p, paused, _ := e.pauseOf(req.Handle)
						return paused && p.StopRunning
					})
				}
			}
			return turn.Result{}, nil
		},
	}
	trigger := events.New(types.TaskAssigned{TaskID: "t-1"}, events.TraceContext{})

	got := d.Dispatch(t.Context(), "ceo", []*events.Event{trigger})
	if got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v (%v), want an ack: a stopped turn's delivery is spent, "+
			"not handed back to run again", got.Outcome, got.Err)
	}
	if rounds != 1 {
		t.Errorf("the turn ran %d rounds, want 1 — the stop lands at the next round", rounds)
	}
	var stopped *types.AgentTurnStopped
	for _, ev := range observed {
		if s, ok := events.DataAs[*types.AgentTurnStopped](ev); ok {
			stopped = s
		}
	}
	if stopped == nil || stopped.StoppedBy != "jane-token" || stopped.StoppedBySeat != "jane" ||
		stopped.AgentHandle != "ceo" || stopped.Reason != "looping" {
		t.Errorf("agent_turn_stopped = %+v, want who stopped ceo's turn and why", stopped)
	}

	// The redelivery a NAK would have caused finds the trigger worked.
	again := d.Dispatch(t.Context(), "ceo", []*events.Event{trigger})
	if again.Outcome != queue.OutcomeAck || rounds != 1 {
		t.Errorf("the stopped trigger ran again (outcome %v, %d rounds)", again.Outcome, rounds)
	}
}

// THE FENCE CARRIES WHO, AND ONLY A STOP CLOSES IT: a pause without one lets
// the running turn finish, which is what the person was told.
func TestOnlyAPauseThatAsksToStopClosesTheFence(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	fence := e.seatFence("ceo")
	p := pauseSeat(t, fleet, "ceo", false)
	waitFor(t, "the copy to see the pause", func() bool {
		_, paused, _ := e.pauseOf("ceo")
		return paused
	})
	if err := fence(); err != nil {
		t.Fatalf("a pause without a stop closed the fence: %v", err)
	}
	p.StopRunning = true
	if _, ok, err := fleet.UpdateSeatPause(t.Context(), p); err != nil || !ok {
		t.Fatalf("UpdateSeatPause = (%v, %v)", ok, err)
	}
	waitFor(t, "the fence to close", func() bool { return fence() != nil })
	err := fence()
	if !turn.Stopped(err) {
		t.Fatalf("fence = %v, want ErrStoppedByPerson", err)
	}
	if who, ok := stopOf(err); !ok || who.Seat != "jane" {
		t.Errorf("the stop names %+v, want the pause that asked for it", who)
	}
}

// A REMOVED SEAT LEAVES NO PAUSE BEHIND. The record has no age, so without the
// apply clearing it a seat later added under the same handle would arrive
// paused by somebody who paused a different role.
func TestARemovedSeatLeavesNoPauseBehind(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	pauseSeat(t, fleet, "ceo", false)
	pauseSeat(t, fleet, "ghost", true)
	waitFor(t, "the copy to see both", func() bool {
		_, a, _ := e.pauseOf("ceo")
		_, b, _ := e.pauseOf("ghost")
		return a && b
	})
	e.clearRemovedSeatPauses(t.Context(), companyFor(t, `
name: Acme
roles:
  - name: CEO
    handle: ceo
`))
	all, err := fleet.ListSeatPauses(t.Context())
	if err != nil {
		t.Fatalf("ListSeatPauses: %v", err)
	}
	if len(all) != 1 || all[0].Handle != "ceo" {
		t.Fatalf("pauses = %+v, want only the seat the company still has", all)
	}
	waitFor(t, "the copy to forget the removed seat", func() bool {
		_, paused, _ := e.pauseOf("ghost")
		return !paused
	})
}

// endingFleet serves one watch that delivers a pause and then ends, and fails
// every watch after it — a store that went away.
type endingFleet struct {
	fleetBase
	mu      sync.Mutex
	watched bool
}

func (f *endingFleet) WatchSeatPauses(context.Context) (<-chan coord.SeatPauseUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watched {
		return nil, coord.ErrUnavailable
	}
	f.watched = true
	out := make(chan coord.SeatPauseUpdate, 2)
	out <- coord.SeatPauseUpdate{Handle: "ceo", Pause: &coord.SeatPause{
		Handle: "ceo", By: "jane-token", At: time.Now(),
	}}
	out <- coord.SeatPauseUpdate{Current: true}
	close(out)
	return out, nil
}

// AN UNREACHABLE STORE IS NOT A RESUME. A watch that ends, and every re-watch
// that fails after it, leave this node's copy as it last was: the seat a person
// paused stays paused, and its hold stays on its inbox.
func TestAnUnreachableStoreIsNotUnpaused(t *testing.T) {
	t.Parallel()
	e, q := engineOn(t)
	e.backends.Fleet = &endingFleet{fleetBase: coordmem.NewFleet()}
	e.startSeatPauses(t.Context())
	t.Cleanup(e.stopSeatPauses)
	time.Sleep(2 * seatPauseRewatchBase)
	if _, paused, known := e.pauseOf("ceo"); !known || !paused {
		t.Fatalf("paused=%v known=%v after the store went away, want the pause kept", paused, known)
	}
	if !held(q.(*memory.Queue), "ceo") {
		t.Error("the hold was lifted when the watch ended")
	}
	if paused, err := e.seatPaused("ceo"); err != nil || !paused {
		t.Errorf("seatPaused = (%v, %v), want the kept pause", paused, err)
	}
}

// A NODE THAT HAS NOT READ THE PAUSES SAYS SO, and never "not paused".
func TestUnreadPausesAreUnknownNotUnpaused(t *testing.T) {
	t.Parallel()
	e, _ := engineOn(t)
	if _, err := e.seatPaused("ceo"); !errors.Is(err, errPausesUnread) {
		t.Errorf("seatPaused before any answer = %v, want errPausesUnread", err)
	}
	if _, _, known := e.pauseOf("ceo"); known {
		t.Error("a node that never read the pauses reported it knew them")
	}
}
