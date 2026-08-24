package a2a_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/events"
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

func (d dir) IsAgentSeat(handle string) bool { return d[handle] }

var clock = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

func service(t *testing.T, seats dir) (*a2a.Service, a2a.Store, *recorder) {
	t.Helper()
	st := a2a.NewMemoryStore()
	rec := &recorder{}
	n := 0
	// A nil map must reach New as a NIL INTERFACE, not as an interface
	// holding a nil map — those are different values in Go and the second
	// refuses every ask. Assigning through the concrete type and then
	// zeroing the interface is what keeps "no directory" meaning it.
	var directory a2a.Directory
	if seats != nil {
		directory = seats
	}
	svc, err := a2a.New(st, rec, a2a.Options{
		Directory: directory,
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

	wakes := rec.onlyTo(topics.AgentInbox("bob"))
	if len(wakes) != 1 {
		t.Fatalf("wakes to bob = %d, want 1 (topics: %v)", len(wakes), rec.topics())
	}
	// THE BRIEF TRAVELS ON THE WAKE. Held anywhere else it exists on
	// exactly one node while the wake reaches whichever node owns the
	// target's seat — the same node only by luck.
	if got := wakes[0].Payload["content"]; got != "can you review this?" {
		t.Errorf("the wake carries content %q", got)
	}
	if got := wakes[0].Payload["channel_id"]; got != id {
		t.Errorf("the wake names channel %q, want %q", got, id)
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
	wake := topics.AgentInbox("bob")
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
	// agent appeared to have.
	//
	// With no directory wired at all, nothing is refused: the guard is a
	// directory question, and a service without one must not invent
	// answers.
	svc, _, _ := service(t, nil)
	if _, err := svc.Open(context.Background(), a2a.Ask{Requester: "alice", Target: "remote-bob"}); err != nil {
		t.Errorf("a target on another node was refused: %v", err)
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
	ask := rec.onlyTo(topics.AgentInbox("bob"))[0]
	if ask.DelegationDepth != 2 {
		t.Errorf("the ask carries depth %d, want the caller's 1 plus one", ask.DelegationDepth)
	}

	rec.sent = nil
	if err := svc.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "yes", DelegationDepth: ask.DelegationDepth,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	reply := rec.onlyTo(topics.AgentInbox("alice"))[0]
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
	ask := rec.onlyTo(topics.AgentInbox("bob"))[0]
	if len(ask.DelegationChain) != 1 || ask.DelegationChain[0] != "alice" {
		t.Fatalf("ask chain = %v, want [alice]", ask.DelegationChain)
	}

	rec.sent = nil
	if err := svc.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "yes", DelegationChain: ask.DelegationChain,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	reply := rec.onlyTo(topics.AgentInbox("alice"))[0]
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
	reply := rec.onlyTo(topics.AgentInbox("alice"))[0]
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
	reply := rec.onlyTo(topics.AgentInbox("alice"))[0]
	if got := reply.Payload["question"]; got != "when?" {
		t.Errorf("the reply echoes %q, want the original ask", got)
	}
	if got := reply.Payload["content"]; got != "friday" {
		t.Errorf("the reply carries %q", got)
	}
}

func TestAReplyInheritsTheAsksTrace(t *testing.T) {
	t.Parallel()
	// So the ask, the answering turn's phases, the reply and the turn it
	// wakes read as ONE trace. Without it the reply carries no trace at all
	// and a dashboard's trace link points nowhere.
	svc, _, rec := service(t, dir{"bob": true, "alice": true})
	id, _ := svc.Open(context.Background(), a2a.Ask{Requester: "alice", Target: "bob", Brief: "?"})
	ask := rec.onlyTo(topics.AgentInbox("bob"))[0]
	ask.TraceID = "0af7651916cd43dd8448eb211c80319c"
	ask.SpanID = "b7ad6b7169203331"

	rec.sent = nil
	if err := svc.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "yes", CausedBy: ask,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	reply := rec.onlyTo(topics.AgentInbox("alice"))[0]
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
		if err := svc.Close(context.Background(), id); err != nil {
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
	if _, err := a2a.New(a2a.NewMemoryStore(), nil, a2a.Options{}); err == nil {
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
