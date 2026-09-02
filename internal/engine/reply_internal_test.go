package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// wake builds one inbox trigger of the given type.
func wake(t *testing.T, kind string, payload map[string]any) *events.Event {
	t.Helper()
	if payload == nil {
		payload = map[string]any{}
	}
	return &events.Event{Type: kind, Payload: payload}
}

// EVERY LEDGERED TYPE IS COVERED, and the assertion is over inbox's own set
// rather than a copy of it: a trigger type added there and forgotten here
// would land on the safe default silently, and the only symptom would be a
// seat that stopped answering the one thing it was asked.
func TestReplyIsDerivedForEveryLedgeredTriggerType(t *testing.T) {
	t.Parallel()
	want := map[string]turn.Reply{
		types.A2ARequestType:                     turn.ReplyEngine,
		types.A2AMessageType:                     turn.ReplyNone,
		types.TaskAssigned{}.EventType():         turn.ReplyTool,
		types.ExternalNotification{}.EventType(): turn.ReplyNone,
	}
	for _, kind := range inbox.LedgeredTypes() {
		if _, ok := want[kind]; !ok {
			t.Errorf("%s is a ledgered trigger with no reply rule; it will "+
				"silently take the unaddressed default", kind)
		}
	}
	for kind, expect := range want {
		if got := ReplyFor([]*events.Event{wake(t, kind, nil)}); got != expect {
			t.Errorf("ReplyFor(%s) = %s, want %s", kind, got, expect)
		}
	}
}

// THE VENDOR'S OWN READING OF ITS ROUTING. A notification is unaddressed
// unless the source said somebody is waiting — see notify.Prompt.Addressed —
// and the flag rides the event so the engine never has to know a vendor's
// event vocabulary.
func TestAnAddressedNotificationOwesAnAnswer(t *testing.T) {
	t.Parallel()
	addressed := wake(t, types.ExternalNotification{}.EventType(),
		map[string]any{"addressed": true})
	if got := ReplyFor([]*events.Event{addressed}); got != turn.ReplyTool {
		t.Errorf("an addressed notification = %s, want tool", got)
	}
	// ABSENT DECODES AS UNADDRESSED, which is the safe half: an event
	// written by a build that predates the field is a freedom to stay
	// silent rather than an obligation nobody recorded.
	older := wake(t, types.ExternalNotification{}.EventType(), map[string]any{})
	if got := ReplyFor([]*events.Event{older}); got != turn.ReplyNone {
		t.Errorf("an event with no flag = %s, want none", got)
	}
}

// STRONGEST WINS, and a merge must not be able to launder an obligation: if
// any part of a coalesced partition asked this seat something, the turn owes
// an answer.
func TestTheStrongestObligationInAPartitionWins(t *testing.T) {
	t.Parallel()
	passing := wake(t, types.ExternalNotification{}.EventType(), map[string]any{})
	asked := wake(t, types.ExternalNotification{}.EventType(),
		map[string]any{"addressed": true})
	ask := wake(t, types.A2ARequestType, nil)

	if got := ReplyFor([]*events.Event{passing, asked}); got != turn.ReplyTool {
		t.Errorf("a burst carrying one ask = %s, want tool", got)
	}
	// The engine's own delivery outranks a tool one wherever both appear:
	// the artifact reaches the asker either way, and demanding a tool call
	// as well would loop the exchange.
	if got := ReplyFor([]*events.Event{asked, ask}); got != turn.ReplyEngine {
		t.Errorf("a partition carrying an A2A ask = %s, want engine", got)
	}
	// And the counterfactual, or every assertion here passes for a
	// function that hardcodes an obligation.
	if got := ReplyFor([]*events.Event{passing, passing}); got != turn.ReplyNone {
		t.Errorf("a burst nobody addressed = %s, want none", got)
	}
	if got := ReplyFor(nil); got != turn.ReplyNone {
		t.Errorf("an empty partition = %s, want none", got)
	}
}

// A nil event in the slice must not take the dispatcher down: the partition
// comes off a broker and the loop reads it before anything has vetted it.
func TestReplyForSkipsNilEvents(t *testing.T) {
	t.Parallel()
	if got := ReplyFor([]*events.Event{nil, wake(t, types.A2ARequestType, nil)}); got != turn.ReplyEngine {
		t.Errorf("ReplyFor with a nil entry = %s", got)
	}
}

// SKIP OR NOTHING. The field's one surviving reader gates on
// PlanDecisionSkip, so any other value on a turn that engaged would
// short-circuit every learning worker — silently, on exactly the successful
// turns worth learning from.
func TestOnlyASkippedTurnWritesAPlanDecision(t *testing.T) {
	t.Parallel()
	if got := skipDecision(string(phase.Skipped)); got != types.PlanDecisionSkip {
		t.Errorf("a skipped turn wrote %q, want skip", got)
	}
	for _, decision := range []phase.Decision{phase.Done, phase.Failed, phase.SelfIterate} {
		if got := skipDecision(string(decision)); got != "" {
			t.Errorf("a %s turn wrote plan_decision %q, want empty", decision, got)
		}
	}
}
