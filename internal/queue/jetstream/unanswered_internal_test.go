package jetstream

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue"
)

// stallingJS answers EVERY existence probe with err, the error an unanswered
// request produces ([jsprovision.Unanswered] has the client's set), until told
// to stop, and passes everything else through. It embeds [jetstream.JetStream]
// and overrides only the two methods the switch under test reaches, since a
// stub of the whole interface would be a second broker to keep correct. It
// stalls every attempt, not the first: [jsprovision.Ask] re-asks, and a second
// attempt reaching the real broker answers "not found", the ordinary absent
// path, which passed with the guarded branch deleted.
type stallingJS struct {
	jetstream.JetStream

	mu     sync.Mutex
	silent bool
	asks   int
	err    error
}

// speak stops stalling and reports how many times it was asked: the
// fall-through is reached by EXHAUSTION, so more than one request went out.
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
// "It is there", "it is not there" and "nobody said" are three facts (the
// rule internal/coord states); read as two, a merely slow metadata group
// failed the boot with "context deadline exceeded". The create that follows
// answers the question either way, so an unanswered probe costs one round
// trip. Run over every error nats.go produces for an unanswered request.
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
			// A short ceiling: what is exercised is the EXHAUSTION of a
			// probe's whole ceiling ([Queue.timing]), not its duration.
			// The ceiling is derived from the pause, with room for four
			// gaps, or only one attempt fits and the more-than-one
			// assertion below measures a single timeout. Set after the
			// open, so the engine's own streams provisioned at
			// production timing.
			timing := q.Clustered().Timing()
			timing.ReAsk = 50 * time.Millisecond
			timing.Lookup = 5 * timing.ReAsk
			q.timing = timing
			ctx := t.Context()

			topic := seatInbox("unanswered-" + sanitizeName(unanswered.Error()))
			group := seatGroup("unanswered-" + sanitizeName(unanswered.Error()))

			// Both probes: EnsureSubscription looks the consumer up, and
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

			// Exhausted, not answered: one request means [jsprovision.Ask]
			// did not re-ask.
			if asks := stalled.speak(); asks < 2 {
				t.Errorf("the probe was sent %d time(s) before the boot carried "+
					"on; the ceiling is meant to hold several attempts, and one "+
					"means Ask did not re-ask", asks)
			}

			// A provisioned object, not merely a call that returned nil.
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
// An error that IS an answer (a malformed name, an auth failure) does not
// clear by waiting; carrying on to the create would turn a configuration
// mistake into a worse message. This half makes the switch a decision rather
// than a blanket retry.
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
// cannot.
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
// not leading the object drops a read without a word, and answers from the real
// broker every request sent while a dropped one is outstanding (the re-ask). It
// records each dropped request per call, and whether a re-ask went beside it.
//
// A lookup's first request is told by what is outstanding, not by a count: a
// count dropped a slow re-ask's successor as a new lookup. A request on a done
// context is never sent (nats.go checks before publishing), so it opens no
// lookup though a read's last re-ask can arrive after the read ended; checked
// under mu, since a dropped request ends only after its context does.
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

// ask records a request of call on ctx and answers the dropped request it
// opens, or nil for a re-ask, which is answered. sent is false for a request
// on a done context, which neither opens a lookup nor asks one again.
func (d *lookupTermJS) ask(ctx context.Context, call string) (drop *droppedAsk, sent bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil {
		return nil, false
	}
	if held := d.held[call]; held != nil {
		held.reasked = true
		return nil, true
	}
	drop = &droppedAsk{}
	d.held[call], d.dropped[call] = drop, append(d.dropped[call], drop)
	return drop, true
}

// withheld is nil for a request of call the real broker answers, and otherwise
// ctx's error: at once for a request never sent, and once its read stops
// listening for a dropped one.
func (d *lookupTermJS) withheld(ctx context.Context, call string) error {
	drop, sent := d.ask(ctx, call)
	if sent && drop == nil {
		return nil
	}
	if sent {
		<-ctx.Done()
		d.ended(call)
	}
	return ctx.Err()
}

// ended is call's dropped request ending, once its read has stopped listening.
func (d *lookupTermJS) ended(call string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.held, call)
}

func (d *lookupTermJS) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	if err := d.withheld(ctx, "stream "+name); err != nil {
		return nil, err
	}
	return d.JetStream.Stream(ctx, name)
}

func (d *lookupTermJS) Consumer(ctx context.Context, stream, name string) (jetstream.Consumer, error) {
	if err := d.withheld(ctx, "consumer "+name); err != nil {
		return nil, err
	}
	return d.JetStream.Consumer(ctx, stream, name)
}

// lookupTermPatience bounds [TestEveryLookupIsAskedAgainAtTheReadTerm] as a
// hang guard only: at the read term its lookups end in a fraction of a second,
// while one at the case's hour-long write term, or held as one request to the
// thirty-second lookup ceiling (which this sits under), never ends inside it.
const lookupTermPatience = 20 * time.Second

// EVERY LOOKUP IS ASKED AGAIN AT THE READ TERM, never at a write's.
//
// A server answers a read when it processes it or never ([jsprovision.ReadTerm]):
// until a just-requested object's preferred leader applies the assignment,
// every other member drops a lookup of it, so one held for a write's term
// waits on no reply. A dropped lookup is asked again a read term and a re-ask
// later, its dropped request still outstanding; every lookup the provisioning
// path makes (the stream's and mailbox's before their creates, the
// attachment's open) has its first request dropped. The write term is an hour,
// so the case judges whether the re-ask comes, never its timing (a timer's
// wake-up is the scheduler's): a lookup not re-asked, re-asked at a write's
// term or abandoning its request first outlasts [lookupTermPatience].
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

// answerAfter answers every lookup it is asked "not found", once answer is
// closed: the re-ask of [TestALateReAskOpensNoLookup], held until the read
// has sent the request behind it.
type answerAfter struct {
	jetstream.JetStream
	answer <-chan struct{}
}

func (a answerAfter) wait(ctx context.Context, absent error) error {
	select {
	case <-a.answer:
		return absent
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a answerAfter) Stream(ctx context.Context, _ string) (jetstream.Stream, error) {
	return nil, a.wait(ctx, jetstream.ErrStreamNotFound)
}

func (a answerAfter) Consumer(ctx context.Context, _, _ string) (jetstream.Consumer, error) {
	return nil, a.wait(ctx, jetstream.ErrConsumerNotFound)
}

// A RE-ASK THAT REACHES THE BROKER AFTER ITS READ HAS ENDED OPENS NO LOOKUP,
// so [lookupTermJS] never counts it as a lookup that was not asked again.
func TestALateReAskOpensNoLookup(t *testing.T) {
	t.Parallel()
	for _, method := range []struct {
		name string
		ask  func(context.Context, jetstream.JetStream) error
	}{
		{"stream s", func(ctx context.Context, js jetstream.JetStream) error {
			_, err := js.Stream(ctx, "s")
			return err
		}},
		{"consumer c", func(ctx context.Context, js jetstream.JetStream) error {
			_, err := js.Consumer(ctx, "s", "c")
			return err
		}},
	} {
		t.Run(method.name, func(t *testing.T) {
			t.Parallel()
			third := make(chan struct{})
			rec := &lookupTermJS{JetStream: answerAfter{answer: third},
				held: map[string]*droppedAsk{}, dropped: map[string][]*droppedAsk{}}
			timing := jsprovision.Clustered(false).Timing()
			timing.ReadTerm, timing.ReAsk = time.Millisecond, time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), lookupTermPatience)
			defer cancel()

			// The first two requests to arrive are the dropped one and its
			// re-ask, in either order; the third waits until both have
			// ended, so it reaches the fake on the read's cancelled context.
			var arrived atomic.Int32
			var firstTwo sync.WaitGroup
			firstTwo.Add(2)
			_, err := jsprovision.ReadWith(ctx, timing, func(ctx context.Context) (struct{}, error) {
				switch n := arrived.Add(1); {
				case n <= 2:
					defer firstTwo.Done()
				case n == 3:
					close(third)
					fallthrough
				default:
					firstTwo.Wait()
				}
				return struct{}{}, method.ask(ctx, rec)
			}, nil)
			if !jsprovision.NotYetVisible(err) {
				t.Fatalf("the read returned %v, want the re-ask's not-found", err)
			}

			rec.mu.Lock()
			defer rec.mu.Unlock()
			drops := rec.dropped[method.name]
			if len(drops) != 1 || !drops[0].reasked {
				t.Errorf("%d request(s) arrived and %d were counted as a dropped lookup's "+
					"first, want the one the read asked again: a request on a done context "+
					"never leaves the client, so it opens no lookup", arrived.Load(), len(drops))
			}
		})
	}
}
