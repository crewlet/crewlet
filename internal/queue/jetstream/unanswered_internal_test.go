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

			topic := seatInbox("unanswered-" + sanitizeName(unanswered.Error()))
			group := seatGroup("unanswered-" + sanitizeName(unanswered.Error()))

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

	topic, group := seatInbox("refused"), seatGroup("refused")
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

// lookupTermJS DROPS the first request of every existence lookup, as a member
// that is not leading the object drops a read without a word, and answers from
// the real broker every request sent while a dropped one is still outstanding:
// the re-ask of a read that keeps listening for its lost request. It records
// each dropped request, by the lookup's call, and whether a re-ask was sent
// beside it.
//
// A LOOKUP'S FIRST REQUEST IS TOLD BY WHAT IS OUTSTANDING, not by a count. A
// request that finds no dropped one of its call outstanding opens a lookup; one
// that finds one is a re-ask of it. Counted as every other request instead, a
// re-ask the broker answered slower than the read term was followed by a
// second one, which was dropped as if it opened a lookup, and the next
// lookup's first request was answered.
type lookupTermJS struct {
	jetstream.JetStream

	mu sync.Mutex
	// held is each call's dropped request still outstanding, and dropped
	// every request each call had dropped, in order.
	held    map[string]*droppedAsk
	dropped map[string][]*droppedAsk
}

// droppedAsk is one dropped request, and whether a re-ask was sent while it was
// still outstanding.
type droppedAsk struct{ reasked bool }

// ask records a request of call and answers the dropped request it opens, or
// nil for a re-ask, which is answered.
func (d *lookupTermJS) ask(call string) *droppedAsk {
	d.mu.Lock()
	defer d.mu.Unlock()
	if held := d.held[call]; held != nil {
		held.reasked = true
		return nil
	}
	drop := &droppedAsk{}
	d.held[call], d.dropped[call] = drop, append(d.dropped[call], drop)
	return drop
}

// ended is call's dropped request ending, once its read has stopped listening.
func (d *lookupTermJS) ended(call string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.held, call)
}

func (d *lookupTermJS) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	call := "stream " + name
	if d.ask(call) != nil {
		<-ctx.Done()
		d.ended(call)
		return nil, ctx.Err()
	}
	return d.JetStream.Stream(ctx, name)
}

func (d *lookupTermJS) Consumer(ctx context.Context, stream, name string) (jetstream.Consumer, error) {
	call := "consumer " + name
	if d.ask(call) != nil {
		<-ctx.Done()
		d.ended(call)
		return nil, ctx.Err()
	}
	return d.JetStream.Consumer(ctx, stream, name)
}

// lookupTermPatience bounds [TestEveryLookupIsAskedAgainAtTheReadTerm] as a
// hang guard and nothing more. At the read term the case's lookups end in a
// fraction of a second. A lookup asked at the case's hour-long write term never
// ends inside it, and neither does one held as a single request to the
// thirty-second lookup ceiling, which the guard sits under.
const lookupTermPatience = 20 * time.Second

// EVERY LOOKUP IS ASKED AGAIN AT THE READ TERM, never at a write's.
//
// The server answers a read when it processes it or never
// ([jsprovision.ReadTerm]): an object another node has just asked for is in
// flight, and until the member preferred to lead it has applied the assignment
// every other member drops a lookup of it without a word. A lookup held for a
// write's term waits on a reply that does not exist — a three-member fleet
// booting together was measured spending sixteen idle seconds on one stream
// lookup that way. So a lookup whose request is dropped is asked again a read
// term and a re-ask later, its dropped request still outstanding beside it.
//
// Every lookup the provisioning path makes has its first request dropped. They
// include the stream's before its create, the mailbox's before its create and
// the attachment's open. Each must be asked again while that request is still
// outstanding.
//
// THE WRITE'S TERM IS AN HOUR, so what tells the cadences apart is whether the
// re-ask comes at all, never how long it took. A lookup asked at the write's
// term, or not asked again, is still waiting when [lookupTermPatience] ends the
// case, and the step reports it. A lookup that abandons its lost request before
// asking again finds nothing outstanding, so each of its requests is dropped in
// turn and it never ends either. Measured instead against a gap of a read term
// past the cadence, the case failed on a loaded race-detector run whose re-ask
// came 63 ms behind its 70 ms timer: how late a timer's goroutine wakes is the
// scheduler's latency, which the read never promised.
func TestEveryLookupIsAskedAgainAtTheReadTerm(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	timing := q.provisioning()
	timing.ReadTerm, timing.ReAsk, timing.AskTerm = 50*time.Millisecond, 20*time.Millisecond, time.Hour
	q.timing = timing
	rec := &lookupTermJS{JetStream: q.js, held: map[string]*droppedAsk{},
		dropped: map[string][]*droppedAsk{}}
	q.js = rec
	ctx, cancel := context.WithTimeout(t.Context(), lookupTermPatience)
	defer cancel()
	// failed reports a step that did not provision, and what one still
	// asking when the case's patience ran out means.
	failed := func(step string, err error) {
		t.Helper()
		if ctx.Err() != nil {
			t.Fatalf("%s: %v — still asking after %v: a dropped lookup held for the "+
				"write's %v term, or never asked again, waits on a reply nobody will "+
				"send", step, err, lookupTermPatience, timing.AskTerm)
		}
		t.Fatalf("%s: %v", step, err)
	}

	spec, err := specForSubject("lookupterm.probe", time.Hour)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	if err := q.ensureStream(ctx, spec); err != nil {
		failed("ensure stream", err)
	}
	topic := seatInbox("lookup-term")
	group := seatGroup("lookup-term")
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		failed("ensure subscription", err)
	}
	if err := q.Subscribe(ctx, topic, group, func(context.Context, *events.Event) queue.Result {
		return queue.Ack()
	}); err != nil {
		failed("subscribe", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if n := len(rec.dropped["stream "+spec.name]); n < 1 {
		t.Errorf("no lookup of the stream %s was dropped, so nothing here held it "+
			"to the term it is asked again at", spec.name)
	}
	if n := len(rec.dropped["consumer "+consumerName(topic, group)]); n < 2 {
		t.Errorf("the mailbox was looked up %d time(s) with its first request "+
			"dropped, want the provisioning probe and the attachment's open", n)
	}
	for call, drops := range rec.dropped {
		for i, drop := range drops {
			if !drop.reasked {
				t.Errorf("%s, lookup %d: its dropped request was never asked again "+
					"while it was outstanding — a lookup that abandons a request "+
					"it presumed lost replaces a late answer with a request a "+
					"term behind it", call, i+1)
			}
		}
	}
}
