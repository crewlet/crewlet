package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

func askEvent(channelID, brief string) *events.Event {
	return &events.Event{
		ID: uuid.New(), Type: types.A2ARequestType, Source: "ceo",
		Payload: map[string]any{
			"channel_id": channelID, "requester": "ceo",
			"content": brief, "sender_role": "Chief Executive",
		},
		DelegationDepth: 1,
		DelegationChain: []string{"ceo"},
	}
}

// A COLLEAGUE'S QUESTION REACHES THE SEAT THAT HAS TO ANSWER IT.
//
// The regression this exists for: DescribeTrigger read only the "text"
// payload key, and the A2A wakes put their body under "content" — so an
// answering seat was handed the bare string "(a2a_request)" as its whole
// task. It ran a full turn against a blank ask, which a model answers by
// inventing one. Nothing failed; the answer was simply about nothing.
func TestAColleaguesQuestionReachesTheAnsweringTurn(t *testing.T) {
	t.Parallel()
	got := DescribeTrigger([]*events.Event{askEvent("a2a-1", "What broke last night?")})
	if got != "What broke last night?" {
		t.Errorf("the answering turn's task = %q", got)
	}
}

// AND SO DOES AN ANSWER, on the way back.
func TestAnAnswerReachesTheSeatThatAsked(t *testing.T) {
	t.Parallel()
	reply := &events.Event{
		ID: uuid.New(), Type: types.A2AMessageType, Source: "cto",
		Payload: map[string]any{
			"channel_id": "a2a-1", "sender": "cto",
			"content": "the deploy at 02:14 rolled back", "question": "What broke?",
		},
	}
	if got := DescribeTrigger([]*events.Event{reply}); got != "the deploy at 02:14 rolled back" {
		t.Errorf("the woken turn's task = %q", got)
	}
}

// A TRIGGER WITH NO READABLE BODY STILL NAMES ITS TYPE, which is what stops
// a turn being handed a blank ask.
func TestATriggerWithNoBodyStillNamesItself(t *testing.T) {
	t.Parallel()
	bare := &events.Event{ID: uuid.New(), Type: types.A2ARequestType,
		Payload: map[string]any{"channel_id": "a2a-1"}}
	if got := DescribeTrigger([]*events.Event{bare}); got != "(a2a_request)" {
		t.Errorf("trigger = %q", got)
	}
}

// THE ASK IS FOUND IN THE PARTITION, and only an ask.
func TestTheAskIsPickedOutOfThePartition(t *testing.T) {
	t.Parallel()
	other := &events.Event{ID: uuid.New(), Type: types.A2AMessageType}
	ask := askEvent("a2a-7", "?")
	if got := askOf([]*events.Event{other, ask}); got != ask {
		t.Error("the ask was not found beside another a2a event")
	}
	if got := askOf([]*events.Event{other}); got != nil {
		t.Error("a partition with no ask reported one")
	}
	if got := askOf(nil); got != nil {
		t.Error("an empty partition reported an ask")
	}
	// A nil in the slice must not panic: a partition is assembled from a
	// broker's delivery, not from a literal.
	if got := askOf([]*events.Event{nil, ask}); got != ask {
		t.Error("a nil event hid the ask behind it")
	}
}

// THE ECHO IS THE ASKER'S ONLY CONTEXT.
//
// The asking turn ENDED when it asked and nothing rehydrates it, so without
// the question echoed back the woken turn receives an answer with no record
// of what it asked.
func TestTheQuestionIsEchoedBack(t *testing.T) {
	t.Parallel()
	if got := askedQuestion(askEvent("a2a-1", "What broke?")); got != "What broke?" {
		t.Errorf("echo = %q", got)
	}
	if got := askedQuestion(&events.Event{Payload: map[string]any{}}); got != "" {
		t.Errorf("echo = %q, want none", got)
	}
}

// THE ANSWER LEG, END TO END.
//
// The regression this exists for: a2a.Service.Reply had no caller outside its
// own suite. An ask woke its target, the target ran a whole turn, and the
// answer went nowhere — the asker, told by a2a_ask to finish its turn rather
// than wait, was never woken, and the terminal state of every exchange was
// the maintenance sweep closing an idle channel an hour later.
func TestAnAnsweredAskWakesTheAskerAndClosesTheChannel(t *testing.T) {
	e := watchdogEngine(t)
	company := e.Company()
	svc := e.a2aService(company)
	if svc == nil {
		t.Fatal("no a2a service on an engine with a fleet and a queue")
	}

	// The asker's mailbox has to exist before the reply is published: the
	// agent stream uses interest retention, so a publish to a subject no
	// durable consumer covers is dropped in silence.
	woken := make(chan *events.Event, 4)
	inbox, group := topics.AgentInbox("ceo"), topics.AgentInboxGroup("ceo")
	if err := e.Backends().Queue.Subscribe(t.Context(), inbox, group,
		func(_ context.Context, ev *events.Event) queue.Result {
			woken <- ev
			return queue.Ack()
		}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	channelID, err := svc.Open(t.Context(), a2a.Ask{
		Requester: "ceo", Target: "cto", Brief: "What broke last night?",
		SenderRole: "CEO", DelegationDepth: 0,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The answering seat's turn, finished.
	e.answerColleague(t.Context(), company, Request{
		Handle: "cto", WorkKey: "wk-answer", Depth: 1,
		Events: []*events.Event{askEvent(channelID, "What broke last night?")},
	}, turn.Result{Decision: phase.Done, Artifact: "the deploy at 02:14 rolled back"})

	var reply *events.Event
	deadline := time.After(10 * time.Second)
	for reply == nil {
		select {
		case ev := <-woken:
			if ev.Type == types.A2AMessageType {
				reply = ev
			}
		case <-deadline:
			t.Fatal("the asker was never woken with the answer")
		}
	}
	// THE TYPED PAYLOAD. The wake used to carry its body in the envelope's
	// free-form bag, which meant the ask could only be read by a consumer
	// that knew the key; it is a registered payload now, so the brief the
	// turn is given comes off the same struct the decoder builds.
	answer, ok := events.DataAs[*types.A2AMessage](reply)
	if !ok {
		t.Fatalf("the reply does not carry a typed A2AMessage payload")
	}
	if answer.Content != "the deploy at 02:14 rolled back" {
		t.Errorf("the answer carried %q", answer.Content)
	}
	if answer.Question != "What broke last night?" {
		t.Errorf("the echo carried %q — the asker's turn ended, so this is its only context",
			answer.Question)
	}
	// And both halves reach the woken seat as its ask: the channel is
	// closed by the time this lands, so a brief without the question leaves
	// the requester reading a reply with no antecedent.
	brief := DescribeTrigger([]*events.Event{reply})
	if !strings.Contains(brief, "the deploy at 02:14 rolled back") ||
		!strings.Contains(brief, "What broke last night?") {
		t.Errorf("the asker's turn is given %q", brief)
	}
	// UNCHANGED, not incremented: the ask is the delegation and this is
	// that hop completing.
	if reply.DelegationDepth != 1 {
		t.Errorf("the reply charges depth %d, want the ask's 1", reply.DelegationDepth)
	}

	// One ask, one answer, then CLOSED — rather than left to the idle
	// sweep an hour later.
	ch, err := a2a.NewCoordStore(e.Backends().Fleet).Get(t.Context(), channelID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ch.Open() {
		t.Error("the channel was left open after its one answer")
	}
}

// A SUSPENDED TURN HAS NOT ANSWERED YET.
//
// It is parked on a detached coding run and is still going. Replying now
// would send the asker an artifact from a turn that has not finished; the
// resumed turn comes back through the same frame and answers then.
func TestASuspendedTurnDoesNotAnswerYet(t *testing.T) {
	e := watchdogEngine(t)
	company := e.Company()
	svc := e.a2aService(company)
	channelID, err := svc.Open(t.Context(), a2a.Ask{
		Requester: "ceo", Target: "cto", Brief: "?", SenderRole: "CEO",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	e.answerColleague(t.Context(), company, Request{
		Handle: "cto", WorkKey: "wk", Depth: 1,
		Events: []*events.Event{askEvent(channelID, "?")},
	}, turn.Result{Suspended: true})

	ch, err := a2a.NewCoordStore(e.Backends().Fleet).Get(t.Context(), channelID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ch.Open() {
		t.Error("a suspended turn closed the channel it has not answered")
	}
}

// A TURN THAT PRODUCED NOTHING STILL OWES AN ANSWER.
//
// Silence leaves the asker waiting on a channel the sweep closes an hour
// later with no explanation, which is strictly worse than a short "I could
// not" the asker can act on.
func TestATurnThatDeliveredNothingStillAnswers(t *testing.T) {
	e := watchdogEngine(t)
	company := e.Company()
	svc := e.a2aService(company)
	channelID, err := svc.Open(t.Context(), a2a.Ask{
		Requester: "ceo", Target: "cto", Brief: "?", SenderRole: "CEO",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	e.answerColleague(t.Context(), company, Request{
		Handle: "cto", WorkKey: "wk", Depth: 1,
		Events: []*events.Event{askEvent(channelID, "?")},
	}, turn.Result{Decision: phase.Failed})

	ch, err := a2a.NewCoordStore(e.Backends().Fleet).Get(t.Context(), channelID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ch.Open() {
		t.Error("a failed turn left its asker waiting on an open channel")
	}
}

// A TURN NOBODY ASKED FOR ANSWERS NOBODY. The ordinary case, and the one a
// bug here would break loudest: every turn in the company runs through this.
func TestATurnWithNoAskAnswersNobody(t *testing.T) {
	e := watchdogEngine(t)
	e.answerColleague(t.Context(), e.Company(), Request{
		Handle: "cto", WorkKey: "wk",
		Events: []*events.Event{{ID: uuid.New(), Type: "external_notification"}},
	}, turn.Result{Decision: phase.Done, Artifact: "posted"})
}

// THE DELEGATION CAP READS THE TRIGGER, so it can bound anything at all.
//
// The regression this exists for: Request.Depth was set at no site on the
// inbox path, so every turn ran at depth 0, turn.CheckDepth compared a
// constant zero against the limit and could never fire, and
// turn_engine.delegation_depth_limit bounded nothing. The one guard against
// two agents asking each other the same question until a budget runs out was
// inert — which only became reachable once an answer could travel back.
func TestTheDelegationDepthComesOffTheTrigger(t *testing.T) {
	t.Parallel()
	deep := askEvent("a2a-1", "?")
	deep.DelegationDepth = 2
	deep.DelegationChain = []string{"ceo", "cto"}

	depth, chain := delegationOf([]*events.Event{deep})
	if depth != 2 {
		t.Errorf("depth = %d, want the trigger's 2", depth)
	}
	if len(chain) != 2 {
		t.Errorf("chain = %v, want the trigger's", chain)
	}
}

// THE DEEPEST OF A BATCH, not the first.
//
// A coalesced partition can hold triggers that arrived by different routes,
// and taking the first would let a shallow one arriving beside a deep ask
// reset the count — which is exactly the reset an unbounded ping-pong needs.
func TestACoalescedPartitionTakesItsDeepestTrigger(t *testing.T) {
	t.Parallel()
	shallow := &events.Event{ID: uuid.New(), Type: "external_notification"}
	deep := askEvent("a2a-1", "?")
	deep.DelegationDepth, deep.DelegationChain = 3, []string{"a", "b", "c"}

	depth, chain := delegationOf([]*events.Event{shallow, deep, shallow})
	if depth != 3 || len(chain) != 3 {
		t.Errorf("depth = %d chain = %v, want the deepest trigger's", depth, chain)
	}
	// And a batch nobody delegated is depth zero with no chain, which is
	// what a person's message or a schedule tick actually is.
	if d, c := delegationOf([]*events.Event{shallow}); d != 0 || c != nil {
		t.Errorf("an undelegated batch reported depth %d chain %v", d, c)
	}
	if d, _ := delegationOf(nil); d != 0 {
		t.Errorf("an empty batch reported depth %d", d)
	}
}

// A COLLEAGUE IS TOLD WHAT ACTUALLY HAPPENED.
//
// Only `done` produced something for them. The other decisions carry an
// artifact that means something else — `skipped` holds the PLANNER'S private
// reasoning that nobody was asking, which is both internal and wrong, since
// somebody plainly was — and forwarding it verbatim sends the wrong thing
// while looking like an answer.
func TestTheAnswerSaysWhatTheTurnActuallyDid(t *testing.T) {
	t.Parallel()
	if got := answerContent(turn.Result{Decision: phase.Done, Artifact: "the answer"}); got != "the answer" {
		t.Errorf("a delivered turn answered %q", got)
	}
	skipped := answerContent(turn.Result{
		Decision: phase.Skipped, Artifact: "nobody was asking this seat to do anything",
	})
	if strings.Contains(skipped, "nobody was asking") {
		t.Errorf("the planner's private reasoning was sent to a colleague: %q", skipped)
	}
	if skipped == "" {
		t.Error("a skipped turn answered with silence, so the asker waits out the sweep")
	}
	breached := answerContent(turn.Result{
		Decision: phase.Failed,
		Breach:   &turn.Breach{Kind: turn.BreachDepth},
	})
	if !strings.Contains(breached, "depth") {
		t.Errorf("a breach did not say which guard stopped it: %q", breached)
	}
	// A `done` turn that produced no text is still not silence.
	if got := answerContent(turn.Result{Decision: phase.Done}); got == "" {
		t.Error("an empty artifact answered with silence")
	}
}
