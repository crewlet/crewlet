package engine

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// askEvent builds an ask the way internal/a2a builds it: events.New over the
// registered payload, never a hand-written Payload bag. No producer emits a
// bag (the typed struct marshals flat beside the envelope), so asserting
// against one lets a read of Payload["channel_id"] pass while every real ask
// is answered nowhere.
func askEvent(channelID, brief string) *events.Event {
	ev := events.New(types.A2ARequest{
		ChannelID: channelID, Requester: "ceo",
		SenderRole: "Chief Executive", Content: brief,
	}, events.TraceContext{})
	ev.Source = "ceo"
	ev.DelegationDepth = 1
	ev.DelegationChain = []string{"ceo"}
	return ev
}

// answerDispatched answers as the dispatch frame does: the ask off the turn's
// trigger, and the turn as its request names it.
func answerDispatched(ctx context.Context, e *Engine, c *Company, req Request, res turn.Result) {
	e.answerColleague(ctx, c, answeringOf(req), colleagueAskOf(askOf(req.Events)), res, nil)
}

// A COLLEAGUE'S QUESTION REACHES THE SEAT THAT HAS TO ANSWER IT: a trigger
// description that misses the A2A body hands the seat "(a2a_request)" as its
// whole task, and a model answers a blank ask by inventing one.
func TestAColleaguesQuestionReachesTheAnsweringTurn(t *testing.T) {
	t.Parallel()
	got := DescribeTrigger([]*events.Event{askEvent("a2a-1", "What broke last night?")}, "")
	if !strings.Contains(got, "What broke last night?") {
		t.Errorf("the answering turn's task = %q", got)
	}
	// And it names WHO asked: an anonymous question has no reply target a
	// model can reason about.
	if !strings.Contains(got, "Chief Executive") {
		t.Errorf("the answering turn's task does not name the asker: %q", got)
	}
}

// AND SO DOES AN ANSWER, on the way back.
func TestAnAnswerReachesTheSeatThatAsked(t *testing.T) {
	t.Parallel()
	reply := events.New(types.A2AMessage{
		ChannelID: "a2a-1", Sender: "cto", SenderRole: "CTO",
		Content: "the deploy at 02:14 rolled back", Question: "What broke?",
	}, events.TraceContext{})
	reply.Source = "cto"
	got := DescribeTrigger([]*events.Event{reply}, "")
	if !strings.Contains(got, "the deploy at 02:14 rolled back") {
		t.Errorf("the woken turn's task = %q", got)
	}
	// The echo travels with it: the asking turn ENDED when it asked, so
	// without the question the answer has no antecedent.
	if !strings.Contains(got, "What broke?") {
		t.Errorf("the woken turn's task carries no question: %q", got)
	}
}

// A TRIGGER WITH NO READABLE BODY STILL NAMES ITS TYPE, which is what stops
// a turn being handed a blank ask.
func TestATriggerWithNoBodyStillNamesItself(t *testing.T) {
	t.Parallel()
	// A BODY-LESS ENVELOPE — which is exactly what an event of a type this
	// build does not know decodes to: no typed payload.
	bare := &events.Event{ID: uuid.New(), Type: types.A2ARequestType}
	if got := DescribeTrigger([]*events.Event{bare}, ""); got != "(a2a_request)" {
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

// THE ECHO IS THE ASKER'S ONLY CONTEXT: the asking turn ended when it asked
// and nothing rehydrates it, so without the question the woken turn has an
// answer with no record of what it asked.
func TestTheQuestionIsEchoedBack(t *testing.T) {
	t.Parallel()
	if got := askedQuestion(askEvent("a2a-1", "What broke?")); got != "What broke?" {
		t.Errorf("echo = %q", got)
	}
	if got := askedQuestion(&events.Event{Type: types.A2ARequestType}); got != "" {
		t.Errorf("echo = %q, want none", got)
	}
	// AND THE ADDRESS, which is what decides whether the answer goes
	// anywhere at all.
	if got := askChannel(askEvent("a2a-9", "?")); got != "a2a-9" {
		t.Errorf("the answer would be sent to %q — an ask with no address is dropped", got)
	}
	if got := askChannel(&events.Event{Type: types.A2ARequestType}); got != "" {
		t.Errorf("a body-less ask reported channel %q", got)
	}
}

// THE ANSWER LEG, END TO END: an ask wakes its target, the target's turn
// answers on the channel, and the asker, told by a2a_ask to end its turn
// rather than wait, is woken by that answer and not left to the idle sweep.
func TestAnAnsweredAskWakesTheAskerAndClosesTheChannel(t *testing.T) {
	t.Parallel()
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
	inbox, group := seatInbox(e, "ceo"), seatInboxGroup(e, "ceo")
	if err := e.Backends().Queue.Subscribe(t.Context(), inbox, group,
		func(_ context.Context, ev *events.Event) queue.Result {
			woken <- ev
			return queue.Ack()
		}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// AND THE CLOSE RECORD, the audit row the Turn screen draws, subscribed
	// before the answer for the same reason as the inbox.
	closes := make(chan *events.Event, 4)
	if err := e.Backends().Queue.Subscribe(t.Context(),
		topics.Event("a2a_channel_closed"), "close-probe",
		func(_ context.Context, ev *events.Event) queue.Result {
			closes <- ev
			return queue.Ack()
		}); err != nil {
		t.Fatalf("subscribe to the close record: %v", err)
	}

	channelID, err := svc.Open(t.Context(), a2a.Ask{
		Requester: "ceo", Target: "cto", Brief: "What broke last night?",
		SenderRole: "CEO", DelegationDepth: 0,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The answering seat's turn, finished.
	answerDispatched(t.Context(), e, company, Request{
		Handle: "cto", WorkKey: "wk-answer", RunID: "run-answer", Depth: 1,
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
	// THE TYPED PAYLOAD: the brief the turn is given comes off the same
	// registered struct the decoder builds, not a free-form bag only a
	// consumer knowing the key could read.
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
	brief := DescribeTrigger([]*events.Event{reply}, "")
	if !strings.Contains(brief, "the deploy at 02:14 rolled back") ||
		!strings.Contains(brief, "What broke last night?") {
		t.Errorf("the asker's turn is given %q", brief)
	}
	// UNCHANGED, not incremented: the ask is the delegation and this is
	// that hop completing.
	if reply.DelegationDepth != 1 {
		t.Errorf("the reply charges depth %d, want the ask's 1", reply.DelegationDepth)
	}

	// THE ANSWERING RUN, not the unit of work: a trigger that fails without
	// acting is redelivered, so one work key runs several times and a parent
	// naming it could not say which attempt answered (ADR-0017).
	if reply.ParentTurnID != "run-answer" {
		t.Errorf("the reply points at %q, want the answering RUN",
			reply.ParentTurnID)
	}

	// One ask, one answer, then CLOSED, not left to the idle sweep.
	ch, err := a2a.NewCoordStore(e.Backends().Fleet).Get(t.Context(), channelID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ch.Open() {
		t.Error("the channel was left open after its one answer")
	}

	// AND THE CLOSE SAYS WHO AND WHEN: an unset ClosedBy reads as "system",
	// the sweep's wording, and with no turn id the row never appears on the
	// answering turn's page.
	var record *types.A2AChannelClosed
	closeDeadline := time.After(10 * time.Second)
	for record == nil {
		select {
		case ev := <-closes:
			if payload, ok := events.DataAs[*types.A2AChannelClosed](ev); ok &&
				payload.ChannelID == channelID {
				record = payload
			}
		case <-closeDeadline:
			t.Fatal("the close was never announced")
		}
	}
	if record.ClosedBy != "cto" {
		t.Errorf("closed_by = %q, want the answering seat", record.ClosedBy)
	}
	if record.TurnID != "run-answer" || record.WorkKey != "wk-answer" {
		t.Errorf("the close names turn %q / work %q, want the answering turn",
			record.TurnID, record.WorkKey)
	}
}

// A SUSPENDED TURN HAS NOT ANSWERED YET: it is parked on its executor's run,
// and the frame that ends it answers, off the ask on the run's record
// (a2aparked_internal_test.go).
func TestASuspendedTurnDoesNotAnswerYet(t *testing.T) {
	t.Parallel()
	e := watchdogEngine(t)
	company := e.Company()
	svc := e.a2aService(company)
	channelID, err := svc.Open(t.Context(), a2a.Ask{
		Requester: "ceo", Target: "cto", Brief: "?", SenderRole: "CEO",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	answerDispatched(t.Context(), e, company, Request{
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

// A TURN THAT PRODUCED NOTHING STILL OWES AN ANSWER: silence leaves the asker
// on a channel the idle sweep closes without waking it, worse than a short
// "I could not" it can act on.
func TestATurnThatDeliveredNothingStillAnswers(t *testing.T) {
	t.Parallel()
	e := watchdogEngine(t)
	company := e.Company()
	svc := e.a2aService(company)
	channelID, err := svc.Open(t.Context(), a2a.Ask{
		Requester: "ceo", Target: "cto", Brief: "?", SenderRole: "CEO",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	answerDispatched(t.Context(), e, company, Request{
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

// A COLLEAGUE IS TOLD WHAT THE TURN'S RUN DID NOT CONTINUE, beside the answer
// and never in it: the answer is the colleague's words, fenced as theirs, and
// the engine's verdict rides its own field, its detail left out, said after the
// fence. Nothing is said of a run that lost none.
//
// Mutation: put the sentence in the answer, carry the detail, or drop the
// verdict from the wake, and a case goes red.
func TestAColleagueIsToldWhatTheRunsSessionLost(t *testing.T) {
	t.Parallel()
	lost := *executorkeep.Lost(executorkeep.LossTooLarge, executorkeep.InsteadFresh, "the store said ghp_secret")
	for name, tc := range map[string]struct {
		verdicts []executorkeep.NotContinued
		want     *executorkeep.NotContinued
	}{
		"a run that lost its session": {[]executorkeep.NotContinued{lost}, withoutDetail(&lost)},
		"a run that continued it":     {nil, nil},
	} {
		e := watchdogEngine(t)
		company := e.Company()
		woken := make(chan *events.Event, 4)
		if err := e.Backends().Queue.Subscribe(t.Context(), seatInbox(e, "ceo"), seatInboxGroup(e, "ceo"),
			func(_ context.Context, ev *events.Event) queue.Result {
				woken <- ev
				return queue.Ack()
			}); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		channelID, err := e.a2aService(company).Open(t.Context(), a2a.Ask{
			Requester: "ceo", Target: "cto", Brief: "?", SenderRole: "CEO",
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		run := sandbox.PendingRun{AgentHandle: "cto", TurnID: "t1", WorkKey: "wk", DelegationDepth: 1,
			NotContinued: tc.verdicts}
		sent := e.answerColleague(t.Context(), company, answeringRun(run), colleagueAskOf(askEvent(channelID, "?")),
			turn.Result{Decision: phase.Done, Artifact: "the answer"}, nil)
		if !sent.replied || sent.sent != "the answer" {
			t.Errorf("%s: the colleague was sent %q (replied %v), want the answer alone", name, sent.sent, sent.replied)
		}
		var answer *types.A2AMessage
		for answer == nil {
			select {
			case ev := <-woken:
				answer, _ = events.DataAs[*types.A2AMessage](ev)
			case <-t.Context().Done():
				t.Fatal("the asker was never woken with the answer")
			}
		}
		if answer.Content != "the answer" || !reflect.DeepEqual(answer.SessionNotContinued, tc.want) {
			t.Errorf("%s: the wake carries %q and %+v, want the answer and %+v", name, answer.Content,
				answer.SessionNotContinued, tc.want)
		}
		b := provenance.NewBoundary()
		brief := answer.FencedBrief(b)
		_, words, label := answer.Quoted()
		fenced := label.Fence(b, words)
		if tc.want != nil && (!strings.Contains(brief, fenced) || !strings.Contains(brief, lost.Sentence()) ||
			strings.Contains(fenced, lost.Sentence()) || strings.Contains(brief, "ghp_secret")) {
			t.Errorf("%s: the asker reads %q, want the sentence after the colleague's fence, and no detail", name, brief)
		}
	}
}

// A TURN NOBODY ASKED FOR ANSWERS NOBODY. The ordinary case, and the one a
// bug here would break loudest: every turn in the company runs through this.
func TestATurnWithNoAskAnswersNobody(t *testing.T) {
	t.Parallel()
	e := watchdogEngine(t)
	answerDispatched(t.Context(), e, e.Company(), Request{
		Handle: "cto", WorkKey: "wk",
		Events: []*events.Event{{ID: uuid.New(), Type: "external_notification"}},
	}, turn.Result{Decision: phase.Done, Artifact: "posted"})
}

// THE DELEGATION CAP READS THE TRIGGER, so it bounds anything at all: a turn
// whose Request.Depth is never set runs at depth 0, turn.CheckDepth never
// fires (turn_engine.delegation_depth_limit bounds nothing), and two agents
// can ask each other the same question until a budget runs out.
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

// THE DEEPEST OF A BATCH, not the first: a coalesced partition can hold
// triggers from different routes, and taking a shallow one beside a deep ask
// resets the count, which is the reset an unbounded ping-pong needs.
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

// answerText is the text a colleague is answered with ([answerContent]).
func answerText(res turn.Result, err error) string {
	text, _ := answerContent(res, err)
	return text
}

// A COLLEAGUE IS TOLD WHAT ACTUALLY HAPPENED: only `done` produced something
// for them, and forwarding another decision's artifact (`skipped` holds the
// executor's private reasoning that nobody asked) sends the wrong thing
// looking like an answer.
func TestTheAnswerSaysWhatTheTurnActuallyDid(t *testing.T) {
	t.Parallel()
	if got := answerText(turn.Result{Decision: phase.Done, Artifact: "the answer"}, nil); got != "the answer" {
		t.Errorf("a delivered turn answered %q", got)
	}
	skipped := answerText(turn.Result{
		Decision: phase.Skipped, Artifact: "nobody was asking this seat to do anything",
	}, nil)
	if strings.Contains(skipped, "nobody was asking") {
		t.Errorf("the executor's private reasoning was sent to a colleague: %q", skipped)
	}
	if skipped == "" {
		t.Error("a skipped turn answered with silence, so the asker waits out the sweep")
	}
	breached := answerText(turn.Result{
		Decision: phase.Failed,
		Breach:   &turn.Breach{Kind: types.GuardDepthCap},
	}, nil)
	if !strings.Contains(breached, "depth") {
		t.Errorf("a breach did not say which guard stopped it: %q", breached)
	}
	// A `done` turn that produced no text is still not silence.
	if got := answerText(turn.Result{Decision: phase.Done}, nil); got == "" {
		t.Error("an empty artifact answered with silence")
	}
	// A TURN A PERSON STOPPED says so, whatever its result held when the stop
	// reached it, rather than asking for more detail: the question was not
	// what ended it.
	stopped := answerText(turn.Result{Decision: phase.Failed},
		fmt.Errorf("resume: %w", turn.ErrStoppedByPerson))
	if !strings.Contains(stopped, "a person stopped my turn") || strings.Contains(stopped, "more detail") {
		t.Errorf("a stopped turn answered %q", stopped)
	}
}

// A BLOCKED TURN'S ACCOUNT IS AN ANSWER, not a failure to produce one: it is
// the one way `done` arrives with no prose, and `submit_work` refuses a
// blocked outcome without evidence, so there is always something to say.
// Answered with the apology instead, the asker, whose channel answerColleague
// closes straight after, has nowhere to follow up and re-asks.
func TestABlockedTurnAnswersWithWhatStoppedIt(t *testing.T) {
	t.Parallel()
	blocked := turn.Result{
		Decision: phase.Done,
		LastWork: &turn.Work{
			Outcome:  turn.OutcomeBlocked,
			Evidence: "asked @founder which repo to file against; waiting on them",
		},
	}
	got := answerText(blocked, nil)
	if !strings.Contains(got, "waiting on them") {
		t.Errorf("the asker was not told what the turn is blocked on: %q", got)
	}
	if strings.Contains(got, "could not produce an answer") {
		t.Errorf("a turn that did the right thing was reported as a failure: %q", got)
	}

	// The artifact still wins where there is one: the evidence is the
	// fallback for an empty one, never a replacement for the seat's own
	// answer.
	withBoth := blocked
	withBoth.Artifact = "the answer itself"
	if got := answerText(withBoth, nil); got != "the answer itself" {
		t.Errorf("the evidence displaced a real artifact: %q", got)
	}

	// And a blocked round that somehow carries no evidence falls through
	// rather than answering with an empty string — silence is the one
	// outcome the asker cannot act on.
	bare := turn.Result{
		Decision: phase.Done,
		LastWork: &turn.Work{Outcome: turn.OutcomeBlocked, Evidence: "   "},
	}
	if got := answerText(bare, nil); strings.TrimSpace(got) == "" {
		t.Error("a blocked turn with no evidence answered with silence")
	}
}
