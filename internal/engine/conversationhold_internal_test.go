package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

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
	d := &Dispatcher{
		Conditions: func(string) inbox.Conditions {
			return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
		},
		Ledgered: inbox.Ledgered,
		Held:     e.conversationHeld,
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
