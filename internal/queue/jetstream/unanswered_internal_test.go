package jetstream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// stallingJS answers EVERY existence probe with the error a broker that never
// replied produces, until it is told to stop, and passes everything else
// through.
//
// EMBEDDED rather than hand-written, because [jetstream.JetStream] is a wide
// interface and a stub of the whole thing would be a second broker to keep
// correct. What is under test is one switch arm, so exactly the two methods
// that reach it are overridden.
//
// It models the failure precisely: the real call returns [context.Canceled] or
// [context.DeadlineExceeded] from nats.go's own request path when the metadata
// group holds a request past the deadline — see [jsprovision.Unanswered] for
// why the client's error set is what it is.
//
// EVERY attempt, not the first. Stalling one and letting the next through is
// what made this file's own case pass with the branch it protects deleted:
// [jsprovision.Ask] re-asks, the second attempt reached the real broker and
// answered "not found", and the caller took the ordinary absent path. The
// probe has to go unanswered until its whole ceiling is spent, which is what
// the queue's own timing ([Queue.timing]) exists to make affordable.
type stallingJS struct {
	jetstream.JetStream

	mu     sync.Mutex
	silent bool
	asks   int
	err    error
}

// speak stops stalling and reports how many times it was asked, which is the
// other half of the claim: the fall-through is reached by EXHAUSTION, so more
// than one request must have been sent.
func (s *stallingJS) speak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent = false
	return s.asks
}

func (s *stallingJS) stall() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.silent {
		return false
	}
	s.asks++
	return true
}

func (s *stallingJS) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	if s.stall() {
		return nil, s.err
	}
	return s.JetStream.Stream(ctx, name)
}

func (s *stallingJS) Consumer(ctx context.Context, stream, name string) (jetstream.Consumer, error) {
	if s.stall() {
		return nil, s.err
	}
	return s.JetStream.Consumer(ctx, stream, name)
}

// A LOOKUP THE BROKER NEVER ANSWERED IS NOT A LOOKUP THAT SAID "NO".
//
// # The failure this protects against
//
// Every provisioning path here read a lookup's three possible answers as two:
// `err == nil` meant observe, [jetstream.ErrStreamNotFound] meant create, and
// EVERYTHING ELSE failed the boot. A metadata group that was merely slow to
// answer therefore produced `ensure stream CREWLET_CONFIG: context deadline
// exceeded` and a node that refused to start — on a cluster where nothing was
// wrong with the stream, the config or the peer.
//
// It is the engine's own three-valued rule (see internal/coord) applied to the
// broker: "it is there", "it is not there" and "nobody said" are three facts,
// and collapsing the last two is what turned a transient stall into a failed
// boot. The create that follows answers the question either way, so an
// unanswered probe costs one wasted round trip and nothing else.
//
// Run over every error nats.go produces for an unanswered request, because the
// one that reached CI was a deadline and the others come from the same client
// on the same path.
func TestAnUnansweredExistenceProbeStillProvisions(t *testing.T) {
	t.Parallel()
	for _, unanswered := range []error{
		context.DeadlineExceeded,
		nats.ErrTimeout,
		nats.ErrNoResponders,
	} {
		t.Run(unanswered.Error(), func(t *testing.T) {
			t.Parallel()
			q := newQueue(t)
			// A SHORT CEILING AND A SHORT PAUSE, because the branch
			// under test is only reached once a probe has spent its
			// WHOLE ceiling — see [Queue.timing]. At the shipped
			// thirty seconds this case would cost thirty seconds to
			// prove; here it costs a quarter of one, and proves the
			// same thing, because what is exercised is the
			// EXHAUSTION rather than the duration.
			//
			// THE CEILING IS DERIVED FROM THE PAUSE rather than a
			// number of its own: it has to outlast the gap between
			// attempts or only one attempt fits, and the assertion
			// below that more than one went out would then be
			// measuring a single timeout. Room for four gaps and the
			// attempts around them. Set after the open, so the
			// engine's own streams were provisioned at production
			// timing and only this case's probe runs at this one.
			timing := q.Clustered().Timing()
			timing.ReAsk = 50 * time.Millisecond
			timing.Lookup = 5 * timing.ReAsk
			q.timing = timing
			ctx := t.Context()

			topic := topics.AgentInbox("unanswered-" + sanitizeName(unanswered.Error()))
			group := topics.AgentInboxGroup("unanswered-" + sanitizeName(unanswered.Error()))

			// BOTH PROBES AT ONCE, because a subscribe reaches
			// both: EnsureSubscription looks the consumer up and
			// streamFor may look the stream up on the way.
			real := q.js
			stalled := &stallingJS{JetStream: real, silent: true, err: unanswered}
			q.js = stalled

			created, err := q.EnsureSubscription(ctx, topic, group)
			if err != nil {
				t.Fatalf("a subscription whose existence probe went unanswered "+
					"failed to provision: %v — a broker that did not reply is "+
					"not a broker that said the mailbox is absent", err)
			}
			if !created {
				t.Error("EnsureSubscription reported it did not create the " +
					"mailbox, but nothing had made one: an unanswered probe " +
					"must leave `existed` false and let the create decide")
			}

			// THE PROBE WAS EXHAUSTED, not answered. More than one
			// request had to go out, or [jsprovision.Ask] did not
			// re-ask and this case is measuring a single timeout
			// rather than the ceiling.
			if asks := stalled.speak(); asks < 2 {
				t.Errorf("the probe was sent %d time(s) before the boot carried "+
					"on; the ceiling is meant to hold several attempts, and one "+
					"means Ask did not re-ask", asks)
			}

			// AND IT REALLY IS THERE. The point is a provisioned
			// object, not a call that returned nil.
			q.js = real
			if _, err := q.js.Consumer(ctx, q.mustStream(t, topic), consumerName(topic, group)); err != nil {
				t.Errorf("the mailbox is not there after a successful "+
					"EnsureSubscription: %v", err)
			}
		})
	}
}

// AND A PROBE THAT FAILED FOR A REASON THAT IS NOT SILENCE STILL FAILS.
//
// The fall-through above is scoped to "nobody answered". An error that IS an
// answer — a malformed name, an auth failure — clears by nobody waiting, and
// carrying on to the create would turn a configuration mistake into a second
// round trip ending in a worse message. This is the half that makes the switch
// a decision rather than a blanket retry.
func TestAnAnsweredFailureStillFailsTheProbe(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	ctx := t.Context()

	refused := errors.New("nats: authorization violation")
	q.js = &stallingJS{JetStream: q.js, silent: true, err: refused}

	topic, group := topics.AgentInbox("refused"), topics.AgentInboxGroup("refused")
	if _, err := q.EnsureSubscription(ctx, topic, group); !errors.Is(err, refused) {
		t.Fatalf("EnsureSubscription returned %v, want the broker's own refusal "+
			"wrapped: only silence falls through to the create", err)
	}
}

// mustStream resolves the stream a topic belongs to, failing the test if it
// cannot — the tests above need it only to confirm what they provisioned.
func (q *Queue) mustStream(t *testing.T, topic string) string {
	t.Helper()
	stream, err := q.streamFor(t.Context(), topic)
	if err != nil {
		t.Fatalf("streamFor(%q): %v", topic, err)
	}
	return stream
}

// sanitizeName turns an error string into something usable inside a subject
// token, so the three subtests do not share one mailbox.
func sanitizeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		}
	}
	return string(out)
}

// lookupTermJS DROPS every other request of each existence lookup — the
// first, the third, … — as a member that is not leading the object drops a
// read without a word, answers the rest from the real broker, and records when
// each request was sent.
type lookupTermJS struct {
	jetstream.JetStream

	mu   sync.Mutex
	sent map[string][]time.Time
}

// note records a request for call, and reports whether it is one to drop.
func (d *lookupTermJS) note(call string) (drop bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent[call] = append(d.sent[call], time.Now())
	return len(d.sent[call])%2 == 1
}

func (d *lookupTermJS) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	if d.note("stream " + name) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return d.JetStream.Stream(ctx, name)
}

func (d *lookupTermJS) Consumer(ctx context.Context, stream, name string) (jetstream.Consumer, error) {
	if d.note("consumer " + name) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return d.JetStream.Consumer(ctx, stream, name)
}

// EVERY LOOKUP IS ASKED AGAIN AT THE READ TERM, never at a write's.
//
// The server answers a read when it processes it or never
// ([jsprovision.ReadTerm]): an object another node has just asked for is in
// flight, and until the member preferred to lead it has applied the assignment
// every other member drops a lookup of it without a word. A lookup held for a
// write's term waits on a reply that does not exist — a three-member fleet
// booting together was measured spending sixteen idle seconds on one stream
// lookup that way. So a lookup whose request is dropped is asked again a read
// term and a re-ask later, its dropped request still outstanding beside it;
// asked as a write, the second request would come a write's term later, which
// here is a hundred times the read's.
//
// All three lookups the provisioning path makes: the stream's before its
// create, the mailbox's before its create, and the attachment's open — each
// one's first request dropped and its second answered.
func TestEveryLookupIsAskedAgainAtTheReadTerm(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	timing := q.provisioning()
	timing.ReadTerm, timing.ReAsk, timing.AskTerm = 50*time.Millisecond, 20*time.Millisecond, 7*time.Second
	q.timing = timing
	rec := &lookupTermJS{JetStream: q.js, sent: map[string][]time.Time{}}
	q.js = rec
	ctx := t.Context()

	spec, err := specForSubject("lookupterm.probe", time.Hour)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	if err := q.ensureStream(ctx, spec); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	topic := topics.AgentInbox("lookup-term")
	group := topics.AgentInboxGroup("lookup-term")
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		t.Fatalf("ensure subscription: %v", err)
	}
	if err := q.Subscribe(ctx, topic, group, func(context.Context, *events.Event) queue.Result {
		return queue.Ack()
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if n := len(rec.sent["stream "+spec.name]); n < 2 {
		t.Errorf("the stream %s was looked up %d time(s), so nothing here measured "+
			"the term its lookup is asked again at", spec.name, n)
	}
	if n := len(rec.sent["consumer "+consumerName(topic, group)]); n < 4 {
		t.Errorf("the mailbox was looked up %d time(s), want two for each of the "+
			"provisioning probe and the attachment's open", n)
	}
	// WELL INSIDE A WRITE'S TERM, with a read term of slack for a loaded
	// runner: what tells the cadences apart is the hundredfold between them.
	limit := 2*timing.ReadTerm + timing.ReAsk
	for call, sent := range rec.sent {
		for i := 0; i+1 < len(sent); i += 2 {
			if gap := sent[i+1].Sub(sent[i]); gap > limit {
				t.Errorf("%s was asked again %v after a request nobody answered, want "+
					"within %v — a dropped lookup held for a write's %v term waits on "+
					"a reply nobody will send", call, gap, limit, timing.AskTerm)
			}
		}
	}
}
