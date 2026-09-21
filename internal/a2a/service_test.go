package a2a_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

type published struct {
	topic string
	ev    *events.Event
}

type recorder struct {
	sent []published
	err  error
}

func (r *recorder) Publish(_ context.Context, topic string, ev *events.Event) error {
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, published{topic: topic, ev: ev})
	return nil
}

func (r *recorder) topics() []string {
	out := make([]string, 0, len(r.sent))
	for _, p := range r.sent {
		out = append(out, p.topic)
	}
	return out
}

func (r *recorder) onlyTo(topic string) []*events.Event {
	var out []*events.Event
	for _, p := range r.sent {
		if p.topic == topic {
			out = append(out, p.ev)
		}
	}
	return out
}

type dir map[string]bool

// SeatInbox answers the id a wake is published to, which for a case here is a
// stable derivation from the handle: what the service needs is a value it can
// build a subject from and that a test can build the same subject from.
func (d dir) SeatInbox(handle string) (uuid.UUID, bool) {
	if !d[handle] {
		return uuid.Nil, false
	}
	return seatID(handle), true
}

// seatID is the id of the seat a case calls handle.
func seatID(handle string) uuid.UUID {
	return uuid.NewSHA1(uuid.MustParse("6f1c2a4e-9d3b-4a71-8f5e-2b0c7d81a940"),
		[]byte(handle))
}

// inbox is that seat's mailbox subject.
func inbox(handle string) string { return topics.AgentInbox(seatID(handle)) }

var clock = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

func service(t *testing.T, seats dir) (*a2a.Service, a2a.Store, *recorder) {
	t.Helper()
	st := a2a.NewCoordStore(memory.NewFleet())
	rec := &recorder{}
	n := 0
	svc, err := a2a.New(st, rec, a2a.Options{
		Directory: seats,
		Now:       func() time.Time { return clock },
		NewID:     func() string { n++; return "a2a-fixed" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, st, rec
}

func TestAnAskOpensAChannelAndWakesTheTarget(t *testing.T) {
	t.Parallel()
	svc, st, rec := service(t, dir{"bob": true})
	id, err := svc.Open(context.Background(), a2a.Ask{
		Requester: "alice", Target: "bob", Brief: "can you review this?",
		SenderRole: "CTO",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ch, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("the channel was not stored: %v", err)
	}
	if ch.Messages != 1 {
		t.Errorf("messages = %d, want the brief counted", ch.Messages)
	}

	wakes := rec.onlyTo(inbox("bob"))
	if len(wakes) != 1 {
		t.Fatalf("wakes to bob = %d, want 1 (topics: %v)", len(wakes), rec.topics())
	}
	// THE BRIEF TRAVELS ON THE WAKE. Held anywhere else it exists on
	// exactly one node while the wake reaches whichever node owns the
	// target's seat — the same node only by luck.
	//
	// Asserted on the TYPED payload, and then on Brief(). The brief used to
	// travel in the envelope's free-form bag, which no reader ever opened:
	// every assertion here passed while the woken seat was handed the
	// literal string "(a2a_request)" instead of the question.
	ask, ok := events.DataAs[*types.A2ARequest](wakes[0])
	if !ok {
		t.Fatalf("the wake does not carry a typed A2ARequest payload")
	}
	if ask.Content != "can you review this?" {
		t.Errorf("the wake carries content %q", ask.Content)
	}
	if ask.ChannelID != id {
		t.Errorf("the wake names channel %q, want %q", ask.ChannelID, id)
	}
	if brief := ask.Brief(); !strings.Contains(brief, "can you review this?") {
		t.Errorf("Brief() = %q, want it to carry the question", brief)
	}
}

func TestTheChannelIsAnnouncedBeforeTheWake(t *testing.T) {
	t.Parallel()
	// The wake RUNS the other agent's turn, and on an in-process queue that
	// happens inline. Publishing it first put the answer and the close on
	// the observability topics ahead of the question that caused them, so a
	// trace read backwards.
	svc, _, rec := service(t, dir{"bob": true})
	if _, err := svc.Open(context.Background(), a2a.Ask{
		Requester: "alice", Target: "bob", Brief: "hello",
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	got := rec.topics()
	wake := inbox("bob")
	opened := topics.Event("a2a_channel_opened")
	iOpened, iWake := indexOf(got, opened), indexOf(got, wake)
	if iOpened < 0 || iWake < 0 {
		t.Fatalf("topics = %v, want both the announcement and the wake", got)
	}
	if iOpened > iWake {
		t.Errorf("the wake was published before the announcement: %v", got)
	}
	// The message record too — it is the question, and it must precede the
	// turn that answers it.
	if iSent := indexOf(got, topics.Event("a2a_message_sent")); iSent < 0 || iSent > iWake {
		t.Errorf("the brief was recorded after the wake: %v", got)
	}
}

func TestAskingYourselfIsRefused(t *testing.T) {
	t.Parallel()
	// A channel to yourself has no responder: the answering side decides
	// who replies by comparing the woken seat against the requester, so a
	// self-channel wakes the asker, reads as an incoming ANSWER, and is
	// never replied to — a turn spent on a question nobody was asked.
	svc, _, rec := service(t, dir{"alice": true})
	_, err := svc.Open(context.Background(), a2a.Ask{Requester: "alice", Target: "alice"})
	if !errors.Is(err, a2a.ErrSelfChannel) {
		t.Fatalf("err = %v, want ErrSelfChannel", err)
	}
	if !strings.Contains(err.Error(), "reason it through") {
		t.Errorf("the error does not tell the agent what to do instead: %v", err)
	}
	if len(rec.sent) != 0 {
		t.Errorf("a refused ask still published %v", rec.topics())
	}
}

func TestANonAgentTargetIsRefusedRatherThanSilentlyUnanswerable(t *testing.T) {
	t.Parallel()
	// Without the guard a human seat or a typo'd handle produces a channel
	// whose wake lands on a subscriber-less topic: the requester reports
	// success and waits on a reply that can never come.
	svc, _, rec := service(t, dir{"bob": true})
	for _, target := range []string{"founder", "typo"} {
		_, err := svc.Open(context.Background(), a2a.Ask{Requester: "alice", Target: target})
		if !errors.Is(err, a2a.ErrNotAnAgent) {
			t.Errorf("%s: err = %v, want ErrNotAnAgent", target, err)
		}
	}
	if len(rec.sent) != 0 {
		t.Errorf("a refused ask still published %v", rec.topics())
	}
	// The counterfactual: a real agent seat goes through.
	if _, err := svc.Open(context.Background(), a2a.Ask{Requester: "alice", Target: "bob"}); err != nil {
		t.Errorf("a real agent seat was refused: %v", err)
	}
}

func TestACrossNodeColleagueIsAValidTarget(t *testing.T) {
	t.Parallel()
	// The question is "does this seat exist and can it be woken", NOT "is
	// it running here". Asking a local pool made every cross-node ask fail
	// as a typo, so the more nodes a company ran, the fewer colleagues each
	// agent appeared to have. The directory answers for a seat no node in
	// this test runs, and the wake lands on that seat's own mailbox.
	svc, _, rec := service(t, dir{"remote-bob": true})
	if _, err := svc.Open(context.Background(), a2a.Ask{Requester: "alice", Target: "remote-bob"}); err != nil {
		t.Fatalf("a target on another node was refused: %v", err)
	}
	if got := rec.onlyTo(inbox("remote-bob")); len(got) == 0 {
		t.Errorf("no wake reached the colleague's mailbox; published to %v", rec.topics())
	}
}

// AND A SERVICE WITH NO DIRECTORY IS REFUSED AT CONSTRUCTION.
//
// It used to be optional, and a nil one meant "refuse nothing" — which
// answered the guard and left the caller to build the wake's subject itself,
// so "is this seat addressable" and "what address is it" were two derivations
// that could disagree. There is only one now, and a service that cannot make
// it can open channels and wake nobody: every ask succeeding and every answer
// never arriving, which reads as a slow colleague rather than as a wiring
// mistake.
func TestAServiceWithNoDirectoryIsRefused(t *testing.T) {
	t.Parallel()
	_, err := a2a.New(a2a.NewCoordStore(memory.NewFleet()), &recorder{}, a2a.Options{})
	if err == nil {
		t.Fatal("a service with no directory was built; it can address no ask at all")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}
}

func TestTheAskChargesDepthAndTheAnswerDoesNot(t *testing.T) {
	t.Parallel()
	// The ask IS the delegation; the answer is that same hop completing.
	// Charging the return leg halves the budget in the one direction nobody
	// meant to spend it: a scheduled 1:1 costs depth 1 to ask and 2 to
	// answer, so the report's first follow-up lands at 3 and the manager's
	// turn dies on a guard breach — a legitimate second exchange ending as
	// an engine failure.
	svc, _, rec := service(t, dir{"bob": true, "alice": true})
	id, err := svc.Open(context.Background(), a2a.Ask{
		Requester: "alice", Target: "bob", Brief: "?", DelegationDepth: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ask := rec.onlyTo(inbox("bob"))[0]
	if ask.DelegationDepth != 2 {
		t.Errorf("the ask carries depth %d, want the caller's 1 plus one", ask.DelegationDepth)
	}

	rec.sent = nil
	if err := svc.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "yes", DelegationDepth: ask.DelegationDepth,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	reply := rec.onlyTo(inbox("alice"))[0]
	if reply.DelegationDepth != 2 {
		t.Errorf("the reply carries depth %d, want the ask's %d unchanged",
			reply.DelegationDepth, ask.DelegationDepth)
	}
}

func TestTheChainGrowsOnEveryHopBecauseItIsProvenance(t *testing.T) {
	t.Parallel()
	// "alice → bob → alice" is exactly what happened. The chain is not a
	// gate, so a repeat visit is history rather than a cycle to suppress.
	svc, _, rec := service(t, dir{"bob": true, "alice": true})
	id, _ := svc.Open(context.Background(), a2a.Ask{Requester: "alice", Target: "bob", Brief: "?"})
	ask := rec.onlyTo(inbox("bob"))[0]
	if len(ask.DelegationChain) != 1 || ask.DelegationChain[0] != "alice" {
		t.Fatalf("ask chain = %v, want [alice]", ask.DelegationChain)
	}

	rec.sent = nil
	if err := svc.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "yes", DelegationChain: ask.DelegationChain,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	reply := rec.onlyTo(inbox("alice"))[0]
	if len(reply.DelegationChain) != 2 || reply.DelegationChain[1] != "bob" {
		t.Errorf("reply chain = %v, want [alice bob]", reply.DelegationChain)
	}
}

func TestAppendingToTheChainDoesNotRewriteTheCallersRecord(t *testing.T) {
	t.Parallel()
	// The chain comes off the triggering event. Appending into its SPARE
	// CAPACITY rewrites the record of a hop that already happened — for
	// every other holder of that slice, in place.
	//
	// Spare capacity is the whole test: a len==cap chain reallocates on
	// append and hides the aliasing completely, which is how this went
	// unasserted the first time.
	svc, _, rec := service(t, dir{"bob": true, "alice": true})
	id, _ := svc.Open(context.Background(), a2a.Ask{Requester: "alice", Target: "bob", Brief: "?"})

	chain := make([]string, 1, 4)
	chain[0] = "alice"
	rec.sent = nil
	if err := svc.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "yes", DelegationChain: chain,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if got := chain[:cap(chain)][1]; got != "" {
		t.Errorf("the reply wrote %q into the caller's backing array", got)
	}
	reply := rec.onlyTo(inbox("alice"))[0]
	if len(reply.DelegationChain) != 2 || reply.DelegationChain[1] != "bob" {
		t.Errorf("reply chain = %v, want [alice bob]", reply.DelegationChain)
	}
}

func TestAReplyEchoesTheQuestionBack(t *testing.T) {
	t.Parallel()
	// The asker's turn ENDED when it asked and nothing rehydrates it. This
	// is the one wake path with no external surface to re-read, so the echo
	// is the only way the question survives the round trip.
	svc, _, rec := service(t, dir{"bob": true, "alice": true})
	id, _ := svc.Open(context.Background(), a2a.Ask{Requester: "alice", Target: "bob", Brief: "when?"})
	rec.sent = nil
	if err := svc.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "friday", Question: "when?",
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	reply := rec.onlyTo(inbox("alice"))[0]
	answer, ok := events.DataAs[*types.A2AMessage](reply)
	if !ok {
		t.Fatalf("the reply does not carry a typed A2AMessage payload")
	}
	if answer.Question != "when?" {
		t.Errorf("the reply echoes %q, want the original ask", answer.Question)
	}
	if answer.Content != "friday" {
		t.Errorf("the reply carries %q", answer.Content)
	}
	// BOTH HALVES IN THE ASK. The channel is closed by the time this lands,
	// so a brief carrying the answer without the question leaves the
	// requester's next turn reading a reply with no antecedent.
	brief := answer.Brief()
	if !strings.Contains(brief, "when?") || !strings.Contains(brief, "friday") {
		t.Errorf("Brief() = %q, want both the question and the answer", brief)
	}
}

func TestAReplyInheritsTheAsksTrace(t *testing.T) {
	t.Parallel()
	// So the ask, the answering turn's phases, the reply and the turn it
	// wakes read as ONE trace. Without it the reply carries no trace at all
	// and a dashboard's trace link points nowhere.
	svc, _, rec := service(t, dir{"bob": true, "alice": true})
	id, _ := svc.Open(context.Background(), a2a.Ask{Requester: "alice", Target: "bob", Brief: "?"})
	ask := rec.onlyTo(inbox("bob"))[0]
	ask.TraceID = "0af7651916cd43dd8448eb211c80319c"
	ask.SpanID = "b7ad6b7169203331"

	rec.sent = nil
	if err := svc.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "yes", CausedBy: ask,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	reply := rec.onlyTo(inbox("alice"))[0]
	if reply.TraceID != ask.TraceID || reply.SpanID != ask.SpanID {
		t.Errorf("reply trace = %s/%s, want the ask's %s/%s",
			reply.TraceID, reply.SpanID, ask.TraceID, ask.SpanID)
	}
}

func TestEveryWayAReplyCanGoNowhereIsAnError(t *testing.T) {
	t.Parallel()
	// All three were one silent drop, and a reply that goes nowhere is the
	// failure the requester experiences as "they never answered".
	svc, st, _ := service(t, dir{"bob": true, "alice": true})
	id, _ := svc.Open(context.Background(), a2a.Ask{Requester: "alice", Target: "bob", Brief: "?"})

	if err := svc.Reply(context.Background(), a2a.Answer{
		ChannelID: "nope", Sender: "bob", Content: "x",
	}); !errors.Is(err, a2a.ErrNoChannel) {
		t.Errorf("unknown channel: err = %v", err)
	}
	if err := svc.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "mallory", Content: "x",
	}); !errors.Is(err, a2a.ErrNotParticipant) {
		t.Errorf("non-participant: err = %v", err)
	}
	if _, err := st.Close(context.Background(), id, clock); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := svc.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "x",
	}); !errors.Is(err, a2a.ErrClosed) {
		t.Errorf("closed channel: err = %v", err)
	}
}

func TestClosingTwiceAnnouncesOnce(t *testing.T) {
	t.Parallel()
	// Both parties may close and the second is not a fault — but two close
	// events for one channel draw two closes on a dashboard.
	svc, _, rec := service(t, dir{"bob": true})
	id, _ := svc.Open(context.Background(), a2a.Ask{Requester: "alice", Target: "bob", Brief: "?"})
	rec.sent = nil
	for range 2 {
		if err := svc.Close(context.Background(), a2a.Closure{
			ChannelID: id, ClosedBy: "bob",
		}); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	if n := len(rec.onlyTo(topics.Event("a2a_channel_closed"))); n != 1 {
		t.Errorf("close announcements = %d, want 1", n)
	}
}

func TestAServiceNeedsBothHalves(t *testing.T) {
	t.Parallel()
	if _, err := a2a.New(nil, &recorder{}, a2a.Options{}); err == nil {
		t.Error("a service built with no channel store")
	}
	if _, err := a2a.New(a2a.NewCoordStore(memory.NewFleet()), nil, a2a.Options{}); err == nil {
		t.Error("a service built with no publisher")
	}
}

func indexOf(hay []string, needle string) int {
	for i, s := range hay {
		if s == needle {
			return i
		}
	}
	return -1
}

// TWO NODES, ONE COMPANY. This is the failure the channel record moved to the
// coordination store to fix, and it is the only test here that needs two
// services: the ask is opened by the node that owns alice's seat and answered
// by the node that owns bob's, which is the ordinary case rather than an edge
// one. While the record lived in each node's own database the answering node's
// read found nothing, so a cross-node ask woke its target, spent a turn on an
// answer, and dropped it with "no such channel".
func TestAnAskOpenedOnOneNodeIsAnsweredFromAnother(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	seats := dir{"alice": true, "bob": true}

	node := func(t *testing.T) (*a2a.Service, *recorder) {
		t.Helper()
		rec := &recorder{}
		svc, err := a2a.New(a2a.NewCoordStore(fleet), rec, a2a.Options{
			Directory: seats,
			Now:       func() time.Time { return clock },
			NewID:     func() string { return "a2a-cross-node" },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return svc, rec
	}
	asking, askRec := node(t)
	answering, answerRec := node(t)

	id, err := asking.Open(context.Background(), a2a.Ask{
		Requester: "alice", Target: "bob", Brief: "can you review this?",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(askRec.onlyTo(inbox("bob"))) != 1 {
		t.Fatalf("the ask did not wake bob (topics: %v)", askRec.topics())
	}

	// The answering node has never written this channel and must still be
	// able to authorize the reply out of it.
	if err := answering.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "looks good",
	}); err != nil {
		t.Fatalf("a reply from the node that did not open the channel: %v", err)
	}
	replies := answerRec.onlyTo(inbox("alice"))
	if len(replies) != 1 {
		t.Fatalf("replies to alice = %d, want 1 (topics: %v)", len(replies), answerRec.topics())
	}
	crossNode, ok := events.DataAs[*types.A2AMessage](replies[0])
	if !ok {
		t.Fatalf("the reply does not carry a typed A2AMessage payload")
	}
	if crossNode.Content != "looks good" {
		t.Errorf("the reply carries content %q", crossNode.Content)
	}

	// And a close performed on the answering node is what the ASKING node
	// reads back. A channel closed on one node and still open on another
	// is a second answer nothing refuses — one ask and one answer is the
	// whole protocol.
	if err := answering.Close(context.Background(), a2a.Closure{
		ChannelID: id, ClosedBy: "bob",
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := asking.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "and again",
	}); !errors.Is(err, a2a.ErrClosed) {
		t.Errorf("a peer's close was invisible: err = %v, want ErrClosed", err)
	}
}

// TestEveryAuditRecordNamesThePublishingTurn is the whole point of the turn
// id on these three payloads.
//
// The Turn screen reads ONE query — EventLog.Turn, `WHERE turn_id = ?` — over
// a column filled from the payload's own top-level `turn_id` field. All three
// audit records went without one, so an A2A ask had never appeared on a turn
// this engine ran, and the panel that advertised "colleagues" failed EMPTY:
// indistinguishable from a turn that spoke to nobody.
//
// ASSERTED ON THE DECODED PAYLOAD rather than on the struct literal, because
// the key is what the column is filled from: a field renamed in its tag would
// satisfy a Go comparison and write an empty column exactly as before.
func TestEveryAuditRecordNamesThePublishingTurn(t *testing.T) {
	t.Parallel()
	svc, _, rec := service(t, dir{"bob": true})
	ctx := context.Background()
	id, err := svc.Open(ctx, a2a.Ask{
		Requester: "alice", Target: "bob", Brief: "can you review this?",
		TurnID: "run-ask", WorkKey: "wk-ask",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := svc.Reply(ctx, a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "looks good",
		TurnID: "run-answer", WorkKey: "wk-answer",
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if err := svc.Close(ctx, a2a.Closure{
		ChannelID: id, ClosedBy: "bob",
		TurnID: "run-answer", WorkKey: "wk-answer",
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}

	opened := decodeOne[*types.A2AChannelOpened](t, rec, "a2a_channel_opened")
	if opened.TurnID != "run-ask" || opened.WorkKey != "wk-ask" {
		t.Errorf("the open record names turn %q / work %q, want the ASKING turn",
			opened.TurnID, opened.WorkKey)
	}
	// TWO message records, and they name DIFFERENT turns: the brief belongs
	// to the turn that asked and the reply to the turn that answered. One
	// value for both would put the answer on the asker's page, where the
	// asker's turn had already ended before it was written.
	sent := decodeAll[*types.A2AMessageSent](t, rec, "a2a_message_sent")
	if len(sent) != 2 {
		t.Fatalf("message records = %d, want the brief and the reply", len(sent))
	}
	if sent[0].TurnID != "run-ask" || sent[0].WorkKey != "wk-ask" {
		t.Errorf("the brief names turn %q / work %q, want the ASKING turn",
			sent[0].TurnID, sent[0].WorkKey)
	}
	if sent[1].TurnID != "run-answer" || sent[1].WorkKey != "wk-answer" {
		t.Errorf("the reply names turn %q / work %q, want the ANSWERING turn",
			sent[1].TurnID, sent[1].WorkKey)
	}
	closed := decodeOne[*types.A2AChannelClosed](t, rec, "a2a_channel_closed")
	if closed.TurnID != "run-answer" || closed.WorkKey != "wk-answer" {
		t.Errorf("the close names turn %q / work %q, want the ANSWERING turn",
			closed.TurnID, closed.WorkKey)
	}
	// AND WHO CLOSED IT. ClosedBy was declared and never written, so
	// Summary's participant branch was dead and every close in this engine's
	// history read as "system" — the wording reserved for the sweep.
	if closed.ClosedBy != "bob" {
		t.Errorf("closed_by = %q, want the participant who closed it", closed.ClosedBy)
	}
	if got := closed.Summary(); strings.HasPrefix(got, "system ") {
		t.Errorf("Summary() = %q, want the participant rather than the sweep", got)
	}
}

// TestTheWakeStillPointsAtTheTurnThatCausedIt holds the second landing of the
// one field.
//
// TurnID feeds both the audit record's `turn_id` and the wake's envelope
// ParentTurnID, and they are read by different things: one answers "what else
// happened on this turn" and the other "what asked for this". A change that
// stamped the payload and dropped the envelope would leave the completion
// side of an exchange with no parent to attribute it to.
func TestTheWakeStillPointsAtTheTurnThatCausedIt(t *testing.T) {
	t.Parallel()
	svc, _, rec := service(t, dir{"bob": true})
	ctx := context.Background()
	id, err := svc.Open(ctx, a2a.Ask{
		Requester: "alice", Target: "bob", Brief: "?", TurnID: "run-ask",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	wakes := rec.onlyTo(topics.AgentInbox("bob"))
	if len(wakes) != 1 || wakes[0].ParentTurnID != "run-ask" {
		t.Fatalf("the ask's wake points at %q, want the asking turn",
			wakes[0].ParentTurnID)
	}
	if err := svc.Reply(ctx, a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "x", TurnID: "run-answer",
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	replies := rec.onlyTo(topics.AgentInbox("alice"))
	if len(replies) != 1 || replies[0].ParentTurnID != "run-answer" {
		t.Fatalf("the answer's wake points at %q, want the answering turn",
			replies[0].ParentTurnID)
	}
}

// TestTheSweepAnnouncesEveryChannelItCloses is the close event that was never
// published.
//
// [a2a.Store.CloseIdle] returns the channels it closed rather than a count
// precisely so its caller can announce them, and the one caller — the
// retention sweep — discarded the list. So a channel nobody answered ended
// silently, although that is the case worth seeing: the requester's turn ended
// when it asked, so a channel reaching the sweep means some turn never
// finished. Three places described this event anyway — that method's contract,
// the "system" branch of A2AChannelClosed.Summary, and the event-system doc.
func TestTheSweepAnnouncesEveryChannelItCloses(t *testing.T) {
	t.Parallel()
	// A SERVICE OF ITS OWN, because the shared helper mints one fixed
	// channel id: two asks under it are one row overwritten, which would
	// assert the sweep's fan-out against a fleet of one.
	st := a2a.NewCoordStore(memory.NewFleet())
	rec := &recorder{}
	n := 0
	svc, err := a2a.New(st, rec, a2a.Options{
		Directory: dir{"bob": true, "carol": true},
		Now:       func() time.Time { return clock },
		NewID:     func() string { n++; return fmt.Sprintf("a2a-%d", n) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for _, target := range []string{"bob", "carol"} {
		if _, err := svc.Open(ctx, a2a.Ask{
			Requester: "alice", Target: target, Brief: "?", TurnID: "run-ask",
		}); err != nil {
			t.Fatalf("Open: %v", err)
		}
	}
	rec.sent = nil
	// The pinned clock is the channels' own LastAt, so a cutoff after it is
	// what makes both idle.
	swept, err := svc.SweepIdle(ctx, clock.Add(time.Hour))
	if err != nil {
		t.Fatalf("SweepIdle: %v", err)
	}
	if swept != 2 {
		t.Fatalf("swept %d channels, want 2", swept)
	}
	closes := decodeAll[*types.A2AChannelClosed](t, rec, "a2a_channel_closed")
	if len(closes) != 2 {
		t.Fatalf("close announcements = %d, want one per swept channel (topics: %v)",
			len(closes), rec.topics())
	}
	for _, closed := range closes {
		// NEITHER A CLOSER NOR A TURN. A swept close is the statement that
		// no turn finished, so naming this node's sweep as the closer would
		// read as a participant, and a turn id would put the row on a turn
		// page it has nothing to do with.
		if closed.ClosedBy != "" {
			t.Errorf("a swept close names closer %q, want nobody", closed.ClosedBy)
		}
		if closed.TurnID != "" || closed.WorkKey != "" {
			t.Errorf("a swept close names turn %q / work %q, want neither",
				closed.TurnID, closed.WorkKey)
		}
		if got := closed.Summary(); !strings.HasPrefix(got, "system ") {
			t.Errorf("Summary() = %q, want the sweep's own wording", got)
		}
	}
}

// TestASweptChannelIsNotSweptTwice — the second tick must find nothing, or the
// announcement it exists for becomes a duplicate close per tick for ever.
func TestASweptChannelIsNotSweptTwice(t *testing.T) {
	t.Parallel()
	svc, _, rec := service(t, dir{"bob": true})
	ctx := context.Background()
	if _, err := svc.Open(ctx, a2a.Ask{
		Requester: "alice", Target: "bob", Brief: "?",
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := svc.SweepIdle(ctx, clock.Add(time.Hour)); err != nil {
		t.Fatalf("SweepIdle: %v", err)
	}
	rec.sent = nil
	n, err := svc.SweepIdle(ctx, clock.Add(time.Hour))
	if err != nil {
		t.Fatalf("second SweepIdle: %v", err)
	}
	if n != 0 || len(rec.sent) != 0 {
		t.Errorf("the second sweep closed %d and published %v", n, rec.topics())
	}
}

// decodeOne is the single event of a type, decoded, or a failure naming what
// was published instead.
func decodeOne[T events.Payload](t *testing.T, rec *recorder, wire string) T {
	t.Helper()
	all := decodeAll[T](t, rec, wire)
	if len(all) != 1 {
		t.Fatalf("%s records = %d, want 1 (topics: %v)", wire, len(all), rec.topics())
	}
	return all[0]
}

func decodeAll[T events.Payload](t *testing.T, rec *recorder, wire string) []T {
	t.Helper()
	var out []T
	for _, ev := range rec.onlyTo(topics.Event(wire)) {
		payload, ok := events.DataAs[T](ev)
		if !ok {
			t.Fatalf("a %s event does not carry its typed payload", wire)
		}
		out = append(out, payload)
	}
	return out
}
