package engine

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// wake builds one inbox trigger of the given type, THE WAY ITS PRODUCER
// BUILDS IT: events.New over the registered payload.
//
// Never a hand-written Payload bag. The envelope's bag and the typed body are
// different places — the body marshals flat beside the envelope and Payload
// carries only what a producer explicitly stamps there (today, the
// conversation key) — so an event literal carrying "addressed" in the bag is a
// shape nothing publishes. Asserting against it is how ReplyFor's read of
// Payload["addressed"] stayed green for a whole release while answering false
// for every real notification the company ever received.
func wake(t *testing.T, kind string) *events.Event {
	t.Helper()
	switch kind {
	case types.A2ARequestType:
		return events.New(types.A2ARequest{
			ChannelID: "a2a-1", Requester: "ceo", Content: "?",
		}, events.TraceContext{})
	case types.A2AMessageType:
		return events.New(types.A2AMessage{
			ChannelID: "a2a-1", Sender: "cto", Content: "answered",
		}, events.TraceContext{})
	case types.TaskAssigned{}.EventType():
		return events.New(types.TaskAssigned{Description: "ship it"}, events.TraceContext{})
	case types.ExternalNotification{}.EventType():
		return inbound(false)
	}
	// A ledgered type this file has no producer shape for. Left as a bare
	// envelope on purpose: the coverage assertion below is what reports it,
	// and failing here instead would hide which type is missing.
	return &events.Event{Type: kind}
}

// inbound builds an inbound notification as internal/notify publishes it.
func inbound(addressed bool) *events.Event {
	salient := "the message"
	return events.New(types.ExternalNotification{
		NotificationSource: "slack", SourceEventType: "message",
		Sender: "ana", Body: "enriched prompt", SalientBody: &salient,
		Addressed: addressed,
	}, events.TraceContext{})
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
		if got := ReplyFor([]*events.Event{wake(t, kind)}); got != expect {
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
	addressed := inbound(true)
	if got := ReplyFor([]*events.Event{addressed}); got != turn.ReplyTool {
		t.Errorf("an addressed notification = %s, want tool", got)
	}
	// AND ACROSS THE WIRE. The dispatch that reads this flag routinely runs
	// on a node that did not publish the event, so the obligation has to
	// survive a marshal/unmarshal round trip — which is precisely what the
	// envelope-bag read did not.
	raw, err := json.Marshal(addressed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back events.Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := ReplyFor([]*events.Event{&back}); got != turn.ReplyTool {
		t.Errorf("an addressed notification off the wire = %s, want tool", got)
	}
	// ABSENT DECODES AS UNADDRESSED, which is the safe half: an event
	// written by a build that predates the field is a freedom to stay
	// silent rather than an obligation nobody recorded.
	older := &events.Event{Type: types.ExternalNotification{}.EventType()}
	if got := ReplyFor([]*events.Event{older}); got != turn.ReplyNone {
		t.Errorf("an event with no flag = %s, want none", got)
	}
}

// STRONGEST WINS, and a merge must not be able to launder an obligation: if
// any part of a coalesced partition asked this seat something, the turn owes
// an answer — WHICHEVER ORDER the broker delivered the events in.
func TestTheStrongestObligationInAPartitionWins(t *testing.T) {
	t.Parallel()
	passing := inbound(false)
	asked := inbound(true)
	ask := wake(t, types.A2ARequestType)

	if got := ReplyFor([]*events.Event{passing, asked}); got != turn.ReplyTool {
		t.Errorf("a burst carrying one ask = %s, want tool", got)
	}
	// A TOOL OBLIGATION OUTRANKS THE ENGINE'S. The tool one is what the
	// engine enforces — a round is sent back until a tool delivered — while
	// an A2A ask is answered from the turn's artifact whatever this says.
	// So where both are owed, tool loses the asker nothing and keeps the
	// check; engine would let the turn end in text the tool-side requester
	// never sees. And the answer cannot depend on which event came first,
	// or it would depend on the broker.
	for _, order := range [][]*events.Event{{asked, ask}, {ask, asked}, {ask, passing, asked}} {
		if got := ReplyFor(order); got != turn.ReplyTool {
			t.Errorf("a partition carrying an A2A ask and an addressed notification = %s, want tool", got)
		}
	}
	for _, order := range [][]*events.Event{{passing, ask}, {ask, passing}} {
		if got := ReplyFor(order); got != turn.ReplyEngine {
			t.Errorf("a partition carrying an A2A ask and a passing mention = %s, want engine", got)
		}
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
	if got := ReplyFor([]*events.Event{nil, wake(t, types.A2ARequestType)}); got != turn.ReplyEngine {
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
