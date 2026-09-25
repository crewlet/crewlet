package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/tracker"
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
	want := map[string]turn.ReplyKind{
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
		if got := ReplyFor([]*events.Event{wake(t, kind)}); got.Kind != expect {
			t.Errorf("ReplyFor(%s) = %s, want %s", kind, got, expect)
		}
	}
}

// THE VENDOR'S OWN READING OF ITS ROUTING. A notification is unaddressed
// unless the source said somebody is waiting — see notify.Prompt.Addressed —
// and the flag rides the event so the engine never has to know a third-party app's
// event vocabulary.
func TestAnAddressedNotificationOwesAnAnswer(t *testing.T) {
	t.Parallel()
	addressed := inbound(true)
	if got := ReplyFor([]*events.Event{addressed}); got.Kind != turn.ReplyTool {
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
	if got := ReplyFor([]*events.Event{&back}); got.Kind != turn.ReplyTool {
		t.Errorf("an addressed notification off the wire = %s, want tool", got)
	}
	// ABSENT DECODES AS UNADDRESSED, which is the safe half: an event
	// written by a build that predates the field is a freedom to stay
	// silent rather than an obligation nobody recorded.
	older := &events.Event{Type: types.ExternalNotification{}.EventType()}
	if got := ReplyFor([]*events.Event{older}); got.Kind != turn.ReplyNone {
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

	if got := ReplyFor([]*events.Event{passing, asked}); got.Kind != turn.ReplyTool {
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
		if got := ReplyFor(order); got.Kind != turn.ReplyTool {
			t.Errorf("a partition carrying an A2A ask and an addressed notification = %s, want tool", got)
		}
	}
	for _, order := range [][]*events.Event{{passing, ask}, {ask, passing}} {
		if got := ReplyFor(order); got.Kind != turn.ReplyEngine {
			t.Errorf("a partition carrying an A2A ask and a passing mention = %s, want engine", got)
		}
	}
	// And the counterfactual, or every assertion here passes for a
	// function that hardcodes an obligation.
	if got := ReplyFor([]*events.Event{passing, passing}); got.Kind != turn.ReplyNone {
		t.Errorf("a burst nobody addressed = %s, want none", got)
	}
	if got := ReplyFor(nil); got.Kind != turn.ReplyNone {
		t.Errorf("an empty partition = %s, want none", got)
	}
}

// A nil event in the slice must not take the dispatcher down: the partition
// comes off a broker and the loop reads it before anything has vetted it.
func TestReplyForSkipsNilEvents(t *testing.T) {
	t.Parallel()
	if got := ReplyFor([]*events.Event{nil, wake(t, types.A2ARequestType)}); got.Kind != turn.ReplyEngine {
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

// AN INFORMED ANSWER OWES THE CHAT SURFACE, NOT THE TRACKER IT CAME FROM.
//
// The answer to a decision whose asker said it would report the outcome in a
// channel wakes the asker from the TRACKER. On its source alone a comment on
// the item would close the turn, and the person who answered — told the
// outcome would be posted — would never see it. The notification's Owes names
// the chat surface, and the obligation is raised there: [turn.Check] sends the
// turn back until a tool on that surface has run. An owed wake is awaited
// whether or not its source read it as addressed, the obligation survives the
// wire (the dispatch runs on whichever node wins the delivery), and a wake
// owing nothing keeps the source it arrived on.
func TestAnInformedAnswerOwesTheChatSurface(t *testing.T) {
	t.Parallel()
	answered := func(addressed bool, owes string) *events.Event {
		salient := "Chose “Hold”: the audit first"
		return events.New(types.ExternalNotification{
			NotificationSource: tracker.Source, SourceEventType: "comment",
			Sender: "ana", Body: "enriched prompt", SalientBody: &salient,
			Addressed: addressed, Owes: owes,
		}, events.TraceContext{})
	}
	if got := ReplyFor([]*events.Event{answered(true, "slack")}); got != turn.ToolReply("slack") {
		t.Errorf("an informed answer owes %+v, want a tool delivery on slack", got)
	}
	if got := ReplyFor([]*events.Event{answered(false, "mattermost")}); got != turn.ToolReply("mattermost") {
		t.Errorf("an owed wake its source read as unaddressed owes %+v — the "+
			"promise is the obligation", got)
	}
	if got := ReplyFor([]*events.Event{answered(true, "")}); got != turn.ToolReply(tracker.Source) {
		t.Errorf("an answer owing no chat surface owes %+v, want the tracker", got)
	}

	raw, err := json.Marshal(answered(true, "slack"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"owes":"slack"`) {
		t.Errorf("the wire form does not carry the owed surface: %s", raw)
	}
	var back events.Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := ReplyFor([]*events.Event{&back}); got != turn.ToolReply("slack") {
		t.Errorf("an informed answer off the wire owes %+v, want slack", got)
	}
}
