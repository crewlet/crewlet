package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
)

const longRoom = "chat:room/1"

// collected is a run of ceo's on conversation that ran out of context, or not.
func collected(conversation string, out, continued bool) *events.Event {
	rec := types.ExecutorCollected{Agent: ceoSeat.String(), ConversationKey: conversation,
		HarnessSessionContinued: continued}
	if out {
		rec.Failed, rec.ErrorKind = true, types.ExecutorErrorContextExhausted
	}
	return events.New(rec, events.TraceContext{})
}

// message is a chat post on conversation written as kind.
func message(conversation string, kind iam.ActorKind) *events.Event {
	ev := events.New(types.ExternalNotification{NotificationSource: "chat", SourceEventType: "post",
		Sender: "cto", Writer: &types.NotificationWriter{Kind: kind, Seat: "cto"}}, events.TraceContext{})
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	ev.Payload[notify.ConversationField] = conversation
	return ev
}

// killed is a run of ceo's on conversation whose harness the kernel killed for
// memory, on a continued session or not.
func killed(conversation string, continued bool) *events.Event {
	return events.New(types.ExecutorCollected{Agent: ceoSeat.String(), ConversationKey: conversation,
		HarnessSessionContinued: continued, Failed: true, ErrorKind: types.ExecutorErrorOutOfMemory,
		MemoryKills: 1, MemoryBytes: 2 << 30}, events.TraceContext{})
}

func holdsOf(t *testing.T, f coord.ConversationHolds) []coord.ConversationHold {
	t.Helper()
	held, err := f.ConversationHolds(t.Context(), ceoSeat)
	if err != nil {
		t.Fatalf("ConversationHolds: %v", err)
	}
	return held
}

// A CONTEXT THAT DID NOT FIT TWICE IN A ROW, THE SECOND TIME ON A FRESH
// SESSION, HOLDS ITS CONVERSATION: once is a long conversation whose session
// is retired, a second on the session it fell back on can still fit fresh,
// and a run that finished in between starts the count again.
func TestTheSecondContextThatDoesNotFitHoldsTheConversation(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	ctx := t.Context()
	for _, ev := range []*events.Event{
		collected(longRoom, true, true), collected(longRoom, false, false), collected(longRoom, true, false),
		collected(longRoom, true, true), collected("chat:room/2", true, false),
	} {
		e.noteConversationRun(ctx, "", ev)
	}
	if held := holdsOf(t, fleet); len(held) != 0 {
		t.Fatalf("no fresh session ran out twice in a row, and %+v is held", held)
	}
	e.noteConversationRun(ctx, "", collected(longRoom, true, false))
	waitFor(t, "the conversation to be held", func() bool { return len(holdsOf(t, fleet)) == 1 })
	h := holdsOf(t, fleet)[0]
	if h.Conversation != longRoom || h.Kind != string(failureContextExhausted) || h.Basis == "" {
		t.Errorf("the hold is %+v, want %s held for its context", h, longRoom)
	}
}

// A HARNESS THE KERNEL KILLED FOR MEMORY TWICE IN A ROW HOLDS ITS CONVERSATION,
// on whatever session: the work does not fit its box, which a fresh session
// does not change. Two different failures in a row are not twice, and the hold
// is judged against the seat's sandbox, not its models.
//
// Mutation: hold on one kill, count a context failure and a kill as one kind,
// or fingerprint the models for a kill, and this goes red.
func TestTheSecondRunKilledForMemoryHoldsTheConversation(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	ctx := t.Context()
	for _, ev := range []*events.Event{
		killed(longRoom, false), collected(longRoom, true, false), killed(longRoom, false),
		collected(longRoom, false, false), killed(longRoom, true),
	} {
		e.noteConversationRun(ctx, "", ev)
	}
	if held := holdsOf(t, fleet); len(held) != 0 {
		t.Fatalf("no run was killed for memory twice in a row, and %+v is held", held)
	}
	e.noteConversationRun(ctx, "", killed(longRoom, true))
	waitFor(t, "the conversation to be held", func() bool { return len(holdsOf(t, fleet)) == 1 })
	h := holdsOf(t, fleet)[0]
	if h.Conversation != longRoom || h.Kind != string(failureOutOfMemory) || h.Basis == "" {
		t.Errorf("the hold is %+v, want %s held for memory", h, longRoom)
	}
	// ON THE SANDBOX, where a company configures both it and models.
	both := capabilityCompany(t)
	memory, _ := failureBasis(both, "ceo", failureOutOfMemory)
	sandbox, _ := failureBasis(both, "ceo", failureSandboxSettings)
	models, _ := failureBasis(both, "ceo", failureContextExhausted)
	if memory != sandbox || memory == models {
		t.Errorf("a memory hold's basis is %q, want the sandbox's %q and not the models' %q", memory, sandbox, models)
	}
}

// A HOLD FOR MEMORY IS LIFTED BY A CHANGED SANDBOX, and only a hold taken on a
// sandbox the company no longer configures is.
//
// Mutation: judge a memory hold against the models, and this goes red.
func TestAChangedSandboxLiftsAMemoryHold(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	ctx := t.Context()
	current, _ := failureBasis(e.Company(), "ceo", failureOutOfMemory)
	for _, h := range []coord.ConversationHold{
		{Seat: ceoSeat, Conversation: longRoom, Kind: string(failureOutOfMemory), Basis: "a box that is gone", At: time.Now()},
		{Seat: ceoSeat, Conversation: "chat:room/2", Kind: string(failureOutOfMemory), Basis: current, At: time.Now()},
	} {
		if ok, err := fleet.CreateConversationHold(ctx, h); err != nil || !ok {
			t.Fatalf("hold: %v (%v)", ok, err)
		}
	}
	if e.conversationHeld(ctx, "ceo", longRoom, []*events.Event{message(longRoom, iam.ActorAgent)}) {
		t.Error("a memory hold taken on another sandbox still holds the conversation")
	}
	if !e.conversationHeld(ctx, "ceo", "chat:room/2", []*events.Event{message("chat:room/2", iam.ActorAgent)}) {
		t.Error("a memory hold on the seat's sandbox as it is was lifted")
	}
}

// A HELD CONVERSATION STARTS NO TURN, AND THE SEAT'S OTHERS DO: its wakes are
// settled unrun, and a person writing on it lifts the hold and runs.
func TestAHeldConversationStartsNoTurnUntilAPersonWritesOnIt(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	ctx := t.Context()
	basis, _ := failureBasis(e.Company(), "ceo", failureContextExhausted)
	if ok, err := fleet.CreateConversationHold(ctx, coord.ConversationHold{Seat: ceoSeat, Conversation: longRoom,
		Kind: string(failureContextExhausted), Basis: basis, At: time.Now()}); err != nil || !ok {
		t.Fatalf("hold: %v (%v)", ok, err)
	}
	var turns atomic.Int32
	var skipped []types.TurnTriggerSkipped
	d := &Dispatcher{
		Conditions: func(string) inbox.Conditions {
			return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
		},
		Ledgered: inbox.Ledgered,
		Held:     e.conversationHeld,
		Observe: func(_ context.Context, ev *events.Event) {
			if s, ok := events.DataAs[*types.TurnTriggerSkipped](ev); ok {
				skipped = append(skipped, *s)
			}
		},
		Turn: func(context.Context, Request) (turn.Result, error) {
			turns.Add(1)
			return turn.Result{}, nil
		},
	}
	dispatch := func(ev *events.Event) queue.Result { return d.Dispatch(ctx, "ceo", []*events.Event{ev}) }
	if got := dispatch(message(longRoom, iam.ActorAgent)); got.Outcome != queue.OutcomeAck || turns.Load() != 0 {
		t.Fatalf("an agent's message on the held conversation answered %+v after %d turns, want settled unrun",
			got, turns.Load())
	}
	// THE TURN LEDGER COUNTS IT AS HELD BY THE BREAKER, not as worked.
	if len(skipped) != 1 || skipped[0].Disposition != types.DispositionBreakerHeld || skipped[0].Wake == "" {
		t.Errorf("the held wake was recorded as %+v, want one skip counted breaker_held under its wake", skipped)
	}
	dispatch(message("chat:room/2", iam.ActorAgent))
	if turns.Load() != 1 {
		t.Fatalf("another conversation ran %d turns, want 1", turns.Load())
	}
	dispatch(message(longRoom, iam.ActorHuman))
	if turns.Load() != 2 || len(holdsOf(t, fleet)) != 0 {
		t.Errorf("a person's message ran %d turns in all and left %+v held, want it run and the hold lifted",
			turns.Load(), holdsOf(t, fleet))
	}
}

// A HOLD TAKEN ON OTHER MODELS IS LIFTED WHEN IT IS NEXT READ: the change of
// models is what the conversation waited on.
func TestNewModelsLiftAConversationHold(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	ctx := t.Context()
	if ok, err := fleet.CreateConversationHold(ctx, coord.ConversationHold{Seat: ceoSeat, Conversation: longRoom,
		Kind: string(failureContextExhausted), Basis: "models that are gone", At: time.Now()}); err != nil || !ok {
		t.Fatalf("hold: %v (%v)", ok, err)
	}
	if e.conversationHeld(ctx, "ceo", longRoom, []*events.Event{message(longRoom, iam.ActorAgent)}) {
		t.Error("a hold taken on other models still holds the conversation")
	}
	if held := holdsOf(t, fleet); len(held) != 0 {
		t.Errorf("the stale hold is still recorded: %+v", held)
	}
}

// A SEAT THE COMPANY REMOVES TAKES ITS HELD CONVERSATIONS WITH IT, as its pause
// goes with it; a seat it keeps keeps them.
func TestARemovedSeatLeavesNoConversationHeld(t *testing.T) {
	t.Parallel()
	e, _, fleet := pausingEngine(t)
	ctx := t.Context()
	for _, seat := range []uuid.UUID{ceoSeat, seatOf("ghost")} {
		if ok, err := fleet.CreateConversationHold(ctx, coord.ConversationHold{Seat: seat, Conversation: longRoom,
			Kind: string(failureContextExhausted), Basis: "b", At: time.Now()}); err != nil || !ok {
			t.Fatalf("hold: %v (%v)", ok, err)
		}
	}
	origin := coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorHuman, Source: "api", CreatedAt: time.Now()}
	activated, err := fleet.Activate(ctx, coord.ActivationRequest{RevisionID: "rev-1", Payload: []byte("{}"),
		At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	next := companyFor(t, "name: Acme\nroles:\n  - name: CEO\n    handle: ceo\n")
	next.ActivatedAt = activated.At
	e.clearRemovedConversationHolds(ctx, next)
	all, err := fleet.ListConversationHolds(ctx)
	if err != nil || len(all) != 1 || all[0].Seat != ceoSeat {
		t.Errorf("after the ghost seat left the holds are %+v (%v), want ceo's alone", all, err)
	}
}
