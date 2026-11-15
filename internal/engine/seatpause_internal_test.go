package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
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
	if _, _, known := e.pauseOf(ceoSeat); !known {
		t.Fatal("the pause watch gave no first answer within its budget on a healthy store")
	}
	return e, q.(*memory.Queue), fleet
}

// seatOf is the id the engineOn company ("Acme") derives for a seat's handle —
// what a pause, a hold and a mailbox are keyed by.
func seatOf(handle string) uuid.UUID {
	id, _ := org.DeriveAgentID("Acme", handle)
	return id
}

// ceoSeat is the fixture's seat every case pauses.
var ceoSeat = seatOf("ceo")

func pauseSeat(t *testing.T, f coord.SeatPauses, seat uuid.UUID, stop bool) coord.SeatPause {
	t.Helper()
	return pauseSeatCtx(t.Context(), t, f, seat, stop)
}

// pauseSeatCtx pauses one seat as a person does: Jane, through her machine
// token — the three halves iam.ActorFor makes of a principal.
func pauseSeatCtx(ctx context.Context, t *testing.T, f coord.SeatPauses, seat uuid.UUID, stop bool) coord.SeatPause {
	t.Helper()
	p, created, err := f.CreateSeatPause(ctx, coord.SeatPause{
		Seat: seat, By: "jane", ByKind: iam.ActorHuman, OperatorID: "pat:jane-token",
		Reason: "looping", StopRunning: stop, At: time.Now(),
	})
	if err != nil || !created {
		t.Fatalf("CreateSeatPause(%s) = (%v, %v)", seat, created, err)
	}
	return p
}

// pauseHeld reports whether the pause's own hold is on seat's inbox — named by
// the seat's id, as every one of its mailboxes is.
func pauseHeld(q *memory.Queue, seat uuid.UUID) bool {
	return slices.Contains(q.PauseHolds(topics.AgentInbox(seat),
		topics.AgentInboxGroup(seat)), seatPauseHold)
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

// takeInbox records seat's inbox attachment on q with e, as a node's
// acquisition hands it to the seat's preparation before the attach: every hold
// e takes on the inbox names it.
func takeInbox(t *testing.T, e *Engine, q queue.EventQueue, seat uuid.UUID) {
	t.Helper()
	at, err := q.Attachment(t.Context(), topics.AgentInbox(seat), topics.AgentInboxGroup(seat))
	if err != nil {
		t.Fatalf("Attachment: %v", err)
	}
	e.attachInbox(seat, at)
}

func attachSeat(t *testing.T, e *Engine, q *memory.Queue, handle string) *seatTurns {
	t.Helper()
	turns := &seatTurns{}
	d := &Dispatcher{
		Conditions: func(h string) inbox.Conditions {
			paused, known := e.pauseOfHandle(h)
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
	takeInbox(t, e, q, seatOf(handle))
	if err := q.SubscribeBatch(t.Context(), topics.AgentInbox(seatOf(handle)),
		topics.AgentInboxGroup(seatOf(handle)),
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
	if err := q.Publish(t.Context(), topics.AgentInbox(seatOf(handle)),
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

	p := pauseSeat(t, fleet, ceoSeat, false)
	waitFor(t, "the watch to hold the inbox", func() bool { return pauseHeld(q, ceoSeat) })
	sendTask(t, q, "ceo", "during-1")
	sendTask(t, q, "ceo", "during-2")
	time.Sleep(50 * time.Millisecond)
	if got := turns.seen(); len(got) != 1 {
		t.Fatalf("a paused seat ran %v", got)
	}

	if gone, err := fleet.DeleteSeatPause(t.Context(), ceoSeat, p.Version); err != nil || !gone {
		t.Fatalf("DeleteSeatPause = (%v, %v)", gone, err)
	}
	waitFor(t, "the resume to deliver what waited", func() bool { return len(turns.seen()) == 3 })
	if got := turns.seen(); !slices.Equal(got, []string{"before", "during-1", "during-2"}) {
		t.Errorf("turns = %v, want the held mail in the order it was sent", got)
	}
	if pauseHeld(q, ceoSeat) {
		t.Error("the resume left the pause hold on the inbox")
	}
}

// THE RACE, from the delivery's side: the copy says paused and no hold is on
// the inbox yet. The screening takes the pause's own hold and parks the mail,
// so nothing runs and nothing is lost — and a hold the resume then lifts.
func TestAPausedSeatParksAndHoldsItsInboxWhenADeliveryRacesTheHold(t *testing.T) {
	t.Parallel()
	e, q, fleet := pausingEngine(t)
	p := pauseSeat(t, fleet, ceoSeat, false)
	waitFor(t, "the copy to see the pause", func() bool {
		_, paused, _ := e.pauseOf(ceoSeat)
		return paused
	})
	turns := attachSeat(t, e, q, "ceo")
	// The watch took the hold already. Dropping it here is the state a
	// delivery races into: the pause has reached this node's copy and the
	// inbox is not held yet.
	at, err := q.Attachment(t.Context(), topics.AgentInbox(ceoSeat), topics.AgentInboxGroup(ceoSeat))
	if err != nil {
		t.Fatalf("Attachment: %v", err)
	}
	if err := q.ResumeTopic(t.Context(), topics.AgentInbox(ceoSeat),
		topics.AgentInboxGroup(ceoSeat), at, seatPauseHold); err != nil {
		t.Fatalf("ResumeTopic: %v", err)
	}
	sendTask(t, q, "ceo", "raced")
	waitFor(t, "the screening to take the hold", func() bool { return pauseHeld(q, ceoSeat) })
	time.Sleep(50 * time.Millisecond)
	if got := turns.seen(); len(got) != 0 {
		t.Fatalf("a delivery that raced the pause ran %v", got)
	}

	if gone, err := fleet.DeleteSeatPause(t.Context(), ceoSeat, p.Version); err != nil || !gone {
		t.Fatalf("DeleteSeatPause = (%v, %v)", gone, err)
	}
	waitFor(t, "the parked mail after the resume", func() bool {
		return slices.Equal(turns.seen(), []string{"raced"})
	})
}

// A PAUSED SEAT ACQUIRED HERE IS ATTACHED ALREADY HELD, for the attachment its
// acquisition makes, and one that is not paused is attached with nothing: the
// first step of the seat's preparation, which the node runs before the attach.
// The seat is held elsewhere as the pause lands, so the pause itself takes
// nothing here and the hold on the inbox is the acquisition's.
//
// Mutation: skip the hold, or take it for a seat that is not paused, and this
// fails.
func TestAPausedSeatIsAttachedUnderItsHold(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, q, fleet := pausingEngine(t)
	subject, group := topics.AgentInbox(ceoSeat), topics.AgentInboxGroup(ceoSeat)
	acquire := func() queue.Attachment {
		t.Helper()
		if _, err := q.Detach(ctx, subject, group); err != nil {
			t.Fatalf("Detach: %v", err)
		}
		e.detachInbox(ceoSeat)
		at, err := q.Attachment(ctx, subject, group)
		if err != nil {
			t.Fatalf("Attachment: %v", err)
		}
		if err := e.acquireInbox(ctx, ceoSeat, at); err != nil {
			t.Fatalf("acquireInbox: %v", err)
		}
		return at
	}

	acquire()
	if pauseHeld(q, ceoSeat) {
		t.Fatal("a seat nobody paused was attached under the pause's hold")
	}

	e.detachInbox(ceoSeat)
	pauseSeat(t, fleet, ceoSeat, false)
	waitFor(t, "the copy to see the pause", func() bool {
		_, paused, _ := e.pauseOf(ceoSeat)
		return paused
	})
	at := acquire()
	if !pauseHeld(q, ceoSeat) {
		t.Fatal("a paused seat's acquisition left its inbox unheld")
	}
	e.pauses.mu.Lock()
	recorded, held := e.pauses.held[ceoSeat]
	e.pauses.mu.Unlock()
	if !held || recorded != at {
		t.Errorf("the hold is recorded for (%v, %v), want the attachment %v it was taken for",
			recorded, held, at)
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
					pauseSeatCtx(ctx, t, fleet, seatOf(req.Handle), true)
					waitFor(t, "the copy to see the stop", func() bool {
						p, paused, _ := e.pauseOf(seatOf(req.Handle))
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
	if stopped == nil || stopped.StoppedBy != "jane" ||
		stopped.StoppedByKind != string(iam.ActorHuman) || stopped.OperatorID != "pat:jane-token" ||
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
	p := pauseSeat(t, fleet, ceoSeat, false)
	waitFor(t, "the copy to see the pause", func() bool {
		_, paused, _ := e.pauseOf(ceoSeat)
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
	if who, ok := stopOf(err); !ok || who.By != "jane" || who.Seat != ceoSeat {
		t.Errorf("the stop names %+v, want the pause that asked for it", who)
	}
}

// A REMOVED SEAT LEAVES NO PAUSE BEHIND. The record has no age, so without a
// published company clearing it the record would outlive the seat for ever.
//
// AND ONLY THE FLEET'S CURRENT COMPANY CLEARS ONE: a node applying a revision
// the fleet has since replaced does not have a seat the newer one added, and
// would delete a pause a person had just placed on it. The control is that
// company, applied from an earlier activation: it clears nothing.
func TestARemovedSeatLeavesNoPauseBehind(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	pauseSeat(t, fleet, ceoSeat, false)
	ghost := seatOf("ghost")
	pauseSeat(t, fleet, ghost, true)
	waitFor(t, "the copy to see both", func() bool {
		_, a, _ := e.pauseOf(ceoSeat)
		_, b, _ := e.pauseOf(ghost)
		return a && b
	})
	// EVERY ACTIVATION CARRIES ITS ORIGIN: the store refuses one without.
	origin := coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorHuman,
		Source: "api", CreatedAt: time.Now()}
	earlier, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Payload: []byte("{}"), At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	current, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-2", Payload: []byte("{}"), At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	next := companyFor(t, `
name: Acme
roles:
  - name: CEO
    handle: ceo
`)
	next.ActivatedAt = earlier.At
	e.clearRemovedSeatPauses(t.Context(), next)
	if all, err := fleet.ListSeatPauses(t.Context()); err != nil || len(all) != 2 {
		t.Fatalf("a company the fleet has moved past cleared pauses: %+v (%v)", all, err)
	}

	next.ActivatedAt = current.At
	e.clearRemovedSeatPauses(t.Context(), next)
	all, err := fleet.ListSeatPauses(t.Context())
	if err != nil {
		t.Fatalf("ListSeatPauses: %v", err)
	}
	if len(all) != 1 || all[0].Seat != ceoSeat {
		t.Fatalf("pauses = %+v, want only the seat the company still has", all)
	}
	waitFor(t, "the copy to forget the removed seat", func() bool {
		_, paused, _ := e.pauseOf(ghost)
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
	out <- coord.SeatPauseUpdate{Seat: ceoSeat, Pause: &coord.SeatPause{
		Seat: ceoSeat, By: "jane", ByKind: iam.ActorHuman, At: time.Now(),
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
	if _, paused, known := e.pauseOf(ceoSeat); !known || !paused {
		t.Fatalf("paused=%v known=%v after the store went away, want the pause kept", paused, known)
	}
	if !pauseHeld(q.(*memory.Queue), ceoSeat) {
		t.Error("the hold was lifted when the watch ended")
	}
	if paused, err := e.seatPaused(ceoSeat); err != nil || !paused {
		t.Errorf("seatPaused = (%v, %v), want the kept pause", paused, err)
	}
}

// A NODE THAT HAS NOT READ THE PAUSES SAYS SO, and never "not paused".
func TestUnreadPausesAreUnknownNotUnpaused(t *testing.T) {
	t.Parallel()
	e, _ := engineOn(t)
	if _, err := e.seatPaused(ceoSeat); !errors.Is(err, errPausesUnread) {
		t.Errorf("seatPaused before any answer = %v, want errPausesUnread", err)
	}
	if _, _, known := e.pauseOf(ceoSeat); known {
		t.Error("a node that never read the pauses reported it knew them")
	}
}

// A PAUSE IS KEYED ON ITS SEAT'S AGENT ID, and holds that seat alone.
//
// The record, the hold and the screening all key on the id derived from the
// seat's handle (ADR-0013): a colleague beside the paused seat — a different
// handle, so a different id — is not paused, and the paused seat reads paused
// by its handle, which is the control.
//
// Mutation: answer the screening by anything but the seat's id, and this
// fails.
func TestAPauseHoldsItsSeatAlone(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	pauseSeat(t, fleet, ceoSeat, false)
	waitFor(t, "the copy to see the pause", func() bool {
		_, paused, _ := e.pauseOf(ceoSeat)
		return paused
	})
	e.epoch.current.Store(companyFor(t, `
name: Acme
roles:
  - name: CEO
    handle: ceo
  - name: Chief
    handle: chief
`))

	if paused, known := e.pauseOfHandle("ceo"); !known || !paused {
		t.Errorf("the paused seat reads paused=%v known=%v, want paused", paused, known)
	}
	if paused, _ := e.pauseOfHandle("chief"); paused {
		t.Error("a colleague beside the paused seat reads paused")
	}
}

// A RESUME THAT LANDS AS A PAUSED SEAT IS ACQUIRED LEAVES ITS INBOX UNHELD.
//
// The node that acquires a paused seat attaches its inbox already held, and the
// resume that lifts the hold has to find it. So the hold is taken and recorded
// as one step, under the lock every pause and resume is folded under: a resume
// lands wholly before it (nothing paused, nothing held) or wholly after it (a
// record, which it lifts). Recorded under that lock and taken after it, by the
// node, a resume landing between the two lifted a hold that was not there yet,
// and the hold the node took next was recorded nowhere: the seat attached held,
// with nothing left to lift it until it moved or was paused and resumed again.
//
// The resume is staged at the queue's take of the hold, through the node's own
// acquisition: it runs at once when nothing holds the pause lock, and otherwise
// as soon as whatever holds it lets go, as a real one would.
//
// Mutation: take the hold outside the pause lock, and the seat attaches held.
func TestAResumeAsAPausedSeatIsAcquiredLeavesItsInboxUnheld(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, q := engineOn(t)
	subject, group := topics.AgentInbox(ceoSeat), topics.AgentInboxGroup(ceoSeat)

	// THE SEAT HELD ELSEWHERE as the pause lands: this node holds no
	// attachment of its inbox, so the pause takes nothing here.
	if _, err := q.Detach(ctx, subject, group); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	e.detachInbox(ceoSeat)
	e.adoptSeatPauses(ctx, map[uuid.UUID]coord.SeatPause{})
	pause := coord.SeatPause{Seat: ceoSeat, By: "jane", ByKind: iam.ActorHuman, Reason: "looping"}
	e.applySeatPause(ctx, coord.SeatPauseUpdate{Seat: ceoSeat, Pause: &pause})
	if pauseHeld(q.(*memory.Queue), ceoSeat) {
		t.Fatal("a pause took a hold on a seat this node does not hold")
	}

	staged := &resumeAtHold{
		EventQueue: q, subject: subject, lock: &e.pauses.mu, resumed: make(chan struct{}),
		resume: func() { e.applySeatPause(ctx, coord.SeatPauseUpdate{Seat: ceoSeat}) },
	}
	e.backends.Queue = staged
	n, err := node.New(node.Config{
		Queue:  staged,
		Coord:  coordmem.New(),
		NodeID: "node-a",
		Owner:  "node-a:1",
		Seats:  func() []placement.Seat { return nil },
		Turn: func(context.Context, string, []*events.Event) queue.Result {
			return queue.Ack()
		},
		// AS THE ENGINE WIRES ITS NODE ([New]).
		SeatReady: func(ctx context.Context, s placement.Seat, lease coord.Lease, at queue.Attachment) error {
			return e.prepareSeat(ctx, s, lease.Epoch, lease.Owner, at)
		},
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}

	seat := placement.Seat{ID: ceoSeat, Handle: "ceo"}
	if err := n.OnAcquire(ctx, seat, coord.Lease{Epoch: 1, Owner: "node-a:1"}); err != nil {
		t.Fatalf("OnAcquire: %v", err)
	}
	if !staged.fired.Load() {
		t.Fatal("the acquisition of a paused seat took no pause hold before the attach")
	}
	<-staged.resumed

	if _, paused, _ := e.pauseOf(ceoSeat); paused {
		t.Fatal("the staged resume did not land")
	}
	if pauseHeld(q.(*memory.Queue), ceoSeat) {
		t.Fatalf("the seat attached held %v by a pause a person had resumed",
			q.(*memory.Queue).PauseHolds(subject, group))
	}
}

// resumeAtHold is a queue that stages a person's resume at the take of the
// pause hold on one inbox, once: at once, before the take, when nothing holds
// lock, and otherwise on a goroutine that waits for lock as a real resume
// does. resumed closes once the resume has run.
type resumeAtHold struct {
	queue.EventQueue
	subject string
	lock    *sync.Mutex
	resume  func()
	fired   atomic.Bool
	resumed chan struct{}
}

func (s *resumeAtHold) PauseTopic(ctx context.Context, topic, group string, at queue.Attachment, reason string) error {
	if topic == s.subject && reason == seatPauseHold && s.fired.CompareAndSwap(false, true) {
		if s.lock.TryLock() {
			s.lock.Unlock()
			s.resume()
			close(s.resumed)
		} else {
			go func() {
				defer close(s.resumed)
				s.resume()
			}()
		}
	}
	return s.EventQueue.PauseTopic(ctx, topic, group, at, reason)
}

// A PAUSE FOLDED WHILE A RESUME'S LIFT IS IN FLIGHT KEEPS THE SEAT HELD.
//
// The lift drops its record under the pause lock and makes its queue call
// outside it, because the in-memory twin's resume drains into handlers that
// read the pause copy. A pause folded in between holds the same attachment
// again, which changes nothing on the queue, and records it, and the late
// resume then lifts that pause's hold: the seat was paused and recorded held
// with its inbox not held. So the lift reads the pause again once its call has
// returned, and takes back the hold of one in force.
//
// The pause is staged at the queue's resume of the pause hold, before the
// resume reaches the queue: inside the window, with the lock free.
//
// Mutation: drop the read after the resume, and the seat is left paused with
// its inbox unheld.
func TestAPauseFoldedAsAResumeIsLiftedKeepsTheSeatHeld(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, q := engineOn(t)
	mq := q.(*memory.Queue)
	e.adoptSeatPauses(ctx, map[uuid.UUID]coord.SeatPause{})
	pause := coord.SeatPause{Seat: ceoSeat, By: "jane", ByKind: iam.ActorHuman, Reason: "looping"}
	e.applySeatPause(ctx, coord.SeatPauseUpdate{Seat: ceoSeat, Pause: &pause})
	if !pauseHeld(mq, ceoSeat) {
		t.Fatal("the pause took no hold on a seat this node holds")
	}

	again := coord.SeatPause{Seat: ceoSeat, By: "pat", ByKind: iam.ActorHuman, Reason: "still looping"}
	staged := &foldAtResume{EventQueue: q, subject: topics.AgentInbox(ceoSeat), hold: seatPauseHold,
		fold: func() { e.applySeatPause(ctx, coord.SeatPauseUpdate{Seat: ceoSeat, Pause: &again}) }}
	e.backends.Queue = staged
	e.applySeatPause(ctx, coord.SeatPauseUpdate{Seat: ceoSeat})

	if !staged.fired.Load() {
		t.Fatal("the resume lifted no pause hold, so nothing was staged in its window")
	}
	if _, paused, _ := e.pauseOf(ceoSeat); !paused {
		t.Fatal("the pause staged in the lift's window did not land")
	}
	if !pauseHeld(mq, ceoSeat) {
		t.Errorf("the seat is paused by %s with its inbox unheld %v: the resume's late lift "+
			"took the hold that pause owns", again.By, mq.PauseHolds(topics.AgentInbox(ceoSeat),
			topics.AgentInboxGroup(ceoSeat)))
	}
	e.pauses.mu.Lock()
	at, recorded := e.pauses.held[ceoSeat]
	e.pauses.mu.Unlock()
	if !recorded || at != e.inboxOf(ceoSeat) {
		t.Errorf("the hold is recorded=%v for attachment %d, want recorded for %d, which "+
			"the next resume lifts", recorded, at, e.inboxOf(ceoSeat))
	}
}

// A LIFT THAT TOOK A PAUSE'S HOLD BACK SAYS THE INBOX STAYS HELD.
//
// A pause folded while a lift was in flight holds the seat again once the
// lift's resume returns, and its `seat_paused` line came before the lift's.
// The lift's `seat_pause_lifted` then read as the seat's last word, a seat
// whose mail flows, while its inbox was held. So the line says what the lift's
// re-read found: held_again, and a detail saying the inbox stays held.
//
// NOT PARALLEL: it reads the process's log, which [logging.Configure] owns.
//
// Mutation: log the lift without what its re-read found, and this fails.
func TestAPauseLiftThatTookItsHoldBackSaysTheInboxStaysHeld(t *testing.T) {
	ctx := t.Context()
	lines := processLog(t)
	e, q := engineOn(t)
	e.adoptSeatPauses(ctx, map[uuid.UUID]coord.SeatPause{})
	pause := coord.SeatPause{Seat: ceoSeat, By: "jane", ByKind: iam.ActorHuman, Reason: "looping"}
	e.applySeatPause(ctx, coord.SeatPauseUpdate{Seat: ceoSeat, Pause: &pause})
	e.applySeatPause(ctx, coord.SeatPauseUpdate{Seat: ceoSeat})
	e.applySeatPause(ctx, coord.SeatPauseUpdate{Seat: ceoSeat, Pause: &pause})

	again := coord.SeatPause{Seat: ceoSeat, By: "pat", ByKind: iam.ActorHuman, Reason: "still looping"}
	staged := &foldAtResume{EventQueue: q, subject: topics.AgentInbox(ceoSeat), hold: seatPauseHold,
		fold: func() { e.applySeatPause(ctx, coord.SeatPauseUpdate{Seat: ceoSeat, Pause: &again}) }}
	e.backends.Queue = staged
	e.applySeatPause(ctx, coord.SeatPauseUpdate{Seat: ceoSeat})
	if !staged.fired.Load() {
		t.Fatal("the resume lifted no pause hold, so nothing was staged in its window")
	}

	saysHeldAgain(t, lines, "seat_pause_lifted", "agent_id", ceoSeat.String())
}

// processLog points the process's log at a JSON buffer for the rest of the
// case and answers the lines written so far under one message. A case that
// calls it reads every goroutine of the process, so it is never parallel.
func processLog(t *testing.T) func(msg string) []map[string]any {
	t.Helper()
	logs := &syncBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	return func(msg string) []map[string]any {
		t.Helper()
		return logLines(t, bytes.NewBufferString(logs.String()))[msg]
	}
}

// saysHeldAgain holds a lift's lines for one seat (key, value) to two: the
// plain lift first, which says the hold is lifted, then the lift whose re-read
// took a hold back, which says the inbox stays held.
func saysHeldAgain(t *testing.T, lines func(string) []map[string]any, msg, key, value string) {
	t.Helper()
	var said []map[string]any
	for _, line := range lines(msg) {
		if line[key] == value {
			said = append(said, line)
		}
	}
	if len(said) != 2 {
		t.Fatalf("%d %s lines for %s, want the plain lift's and the window's: %v",
			len(said), msg, value, said)
	}
	if again, _ := said[0]["held_again"].(bool); again || said[0]["held_again"] == nil {
		t.Errorf("the plain lift's %s says held_again=%v, want false: %v", msg,
			said[0]["held_again"], said[0])
	}
	if detail, _ := said[0]["detail"].(string); strings.Contains(detail, "stays held") {
		t.Errorf("the plain lift's %s says the inbox stays held: %q", msg, detail)
	}
	if again, _ := said[1]["held_again"].(bool); !again {
		t.Errorf("the lift that took the hold back logs %s with held_again=%v, want true: "+
			"its line reads as a seat whose mail flows, while its inbox is held: %v",
			msg, said[1]["held_again"], said[1])
	}
	if detail, _ := said[1]["detail"].(string); !strings.Contains(detail, "inbox stays held") {
		t.Errorf("the lift that took the hold back says %q, want that the inbox stays held", detail)
	}
}

// foldAtResume is a queue that runs fold once, at the resume of hold on one
// inbox and before that resume reaches the queue: the window a lift leaves
// between dropping its record under its lock and its queue call outside it.
// Late, it runs fold once the resume has reached the queue and before it
// returns, where the in-memory twin hands the resumed mail over.
type foldAtResume struct {
	queue.EventQueue
	subject, hold string
	late          bool
	fold          func()
	fired         atomic.Bool
}

func (s *foldAtResume) ResumeTopic(ctx context.Context, topic, group string, at queue.Attachment, reason string) error {
	if topic != s.subject || reason != s.hold || !s.fired.CompareAndSwap(false, true) {
		return s.EventQueue.ResumeTopic(ctx, topic, group, at, reason)
	}
	if !s.late {
		s.fold()
		return s.EventQueue.ResumeTopic(ctx, topic, group, at, reason)
	}
	err := s.EventQueue.ResumeTopic(ctx, topic, group, at, reason)
	s.fold()
	return err
}

// A SEAT'S RELEASE LEAVES NO RECORD OF A PAUSE HOLD BEHIND.
//
// The release's detach drops every hold on the seat's inbox and ends the
// attachment each was taken for, and the release forgets the seat's parks.
// The records of a person's pause hold and of the no-model pause were left:
// each named the attachment that had ended, and a later lift lifted nothing
// on it and logged that it had (`seat_pause_lifted`, `seat_inbox_resumed`).
//
// Mutation: keep either record at the release, and this fails.
func TestASeatsReleaseLeavesNoRecordOfItsPauseHolds(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, q := engineOn(t)
	e.adoptSeatPauses(ctx, map[uuid.UUID]coord.SeatPause{})
	pause := coord.SeatPause{Seat: ceoSeat, By: "jane", ByKind: iam.ActorHuman, Reason: "looping"}
	e.applySeatPause(ctx, coord.SeatPauseUpdate{Seat: ceoSeat, Pause: &pause})
	if err := e.pause(ctx, "ceo", "no turn engine"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	e.pauses.mu.Lock()
	_, held := e.pauses.held[ceoSeat]
	e.pauses.mu.Unlock()
	if !held || !slices.Contains(recorded(e), "ceo") {
		t.Fatalf("before the release: pause hold recorded=%v, no-model record %v; want both",
			held, recorded(e))
	}

	// AS THE NODE RELEASES A SEAT: its inbox detached, then the hook.
	if _, err := q.Detach(ctx, topics.AgentInbox(ceoSeat), topics.AgentInboxGroup(ceoSeat)); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	e.releaseSeat(ctx, placement.Seat{ID: ceoSeat, Handle: "ceo"})

	e.pauses.mu.Lock()
	at, held := e.pauses.held[ceoSeat]
	e.pauses.mu.Unlock()
	if held {
		t.Errorf("the released seat's pause hold is still recorded, for the ended attachment %d", at)
	}
	if got := recorded(e); slices.Contains(got, "ceo") {
		t.Errorf("the released seat's no-model pause is still recorded: %v", got)
	}
}
