package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/worklet"
)

// wake builds one inbox trigger of the given type THE WAY ITS PRODUCER BUILDS
// IT: events.New over the registered payload, never a hand-written Payload bag.
// The typed body marshals flat beside the envelope and Payload carries only
// what a producer stamps there (the conversation key), so "addressed" in the
// bag is a shape nothing publishes, and a test against it passes for a
// ReplyFor that answers false for every real notification.
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
	case types.WorkPickupType:
		return events.New(types.WorkPickup{Item: "t-1", Key: "ENG-1", Status: "todo"},
			events.TraceContext{})
	case types.DelegatedReviewType:
		return events.New(types.DelegatedReview{RequestID: "req-1", Requester: "sre", Kind: "ssh_operate"},
			events.TraceContext{})
	case types.ExternalNotification{}.EventType():
		return inbound(false)
	}
	// A ledgered type with no producer shape here: a bare envelope, so the
	// coverage assertion below reports which type is missing.
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

// EVERY LEDGERED TYPE IS COVERED, asserted over inbox's own set rather than a
// copy: a trigger type forgotten here would silently take the unaddressed
// default, and its seat would stop answering what it was asked.
func TestReplyIsDerivedForEveryLedgeredTriggerType(t *testing.T) {
	t.Parallel()
	want := map[string]turn.ReplyKind{
		types.A2ARequestType:                     turn.ReplyEngine,
		types.A2AMessageType:                     turn.ReplyNone,
		types.TaskAssigned{}.EventType():         turn.ReplyTool,
		types.WorkPickupType:                     turn.ReplyTool,
		types.DelegatedReviewType:                turn.ReplyTool,
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
// unless the source said somebody is waiting (notify.Prompt.Addressed); the
// flag rides the event so the engine never learns a vendor's vocabulary.
func TestAnAddressedNotificationOwesAnAnswer(t *testing.T) {
	t.Parallel()
	addressed := inbound(true)
	if got := ReplyFor([]*events.Event{addressed}); got.Kind != turn.ReplyTool {
		t.Errorf("an addressed notification = %s, want tool", got)
	}
	// AND ACROSS THE WIRE: the dispatch often runs on a node that did not
	// publish the event, so the obligation survives a marshal round trip.
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
	// ABSENT DECODES AS UNADDRESSED: an omission is a freedom to stay
	// silent, never an obligation nobody recorded.
	unflagged := &events.Event{Type: types.ExternalNotification{}.EventType()}
	if got := ReplyFor([]*events.Event{unflagged}); got.Kind != turn.ReplyNone {
		t.Errorf("an event with no flag = %s, want none", got)
	}
}

// STRONGEST WINS, so a merge cannot launder an obligation: if any part of a
// coalesced partition asked this seat something, the turn owes an answer, in
// WHICHEVER ORDER the broker delivered the events.
func TestTheStrongestObligationInAPartitionWins(t *testing.T) {
	t.Parallel()
	passing := inbound(false)
	asked := inbound(true)
	ask := wake(t, types.A2ARequestType)

	if got := ReplyFor([]*events.Event{passing, asked}); got.Kind != turn.ReplyTool {
		t.Errorf("a burst carrying one ask = %s, want tool", got)
	}
	// A TOOL OBLIGATION OUTRANKS THE ENGINE'S: only the tool one is enforced
	// (a round is sent back until a tool delivered), and an A2A ask is
	// answered from the turn's artifact either way, so engine would let the
	// turn end in text the tool-side requester never sees.
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
	// The counterfactual, or a hardcoded obligation passes.
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

// AN INFORMED ANSWER OWES THE CHAT SURFACE, NOT THE TRACKER IT CAME FROM. The
// answer to a decision whose asker promised to report in a channel wakes it
// from the tracker, where a comment would close the turn unseen by the person
// who answered. Owes names the chat surface, and [turn.Check] sends the turn
// back until a tool there has run. An owed wake is awaited whether or not its
// source read it as addressed, Owes survives the wire, and a wake owing
// nothing keeps its source.
func TestAnInformedAnswerOwesTheChatSurface(t *testing.T) {
	t.Parallel()
	answered := func(addressed bool, owes string) *events.Event {
		salient := "Chose “Hold”: the audit first"
		return events.New(types.ExternalNotification{
			NotificationSource: worklet.Source, SourceEventType: "comment",
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
	if got := ReplyFor([]*events.Event{answered(true, "")}); got != turn.ToolReply(worklet.Source) {
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
