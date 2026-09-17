package jetstream

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// stallingJS answers the first n existence probes with the error a broker that
// never replied produces, and passes everything else through.
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
type stallingJS struct {
	jetstream.JetStream

	mu      sync.Mutex
	streams int // how many more Stream lookups to leave unanswered
	consume int // how many more Consumer lookups to leave unanswered
	err     error
}

func (s *stallingJS) take(n *int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if *n <= 0 {
		return false
	}
	*n--
	return true
}

func (s *stallingJS) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	if s.take(&s.streams) {
		return nil, s.err
	}
	return s.JetStream.Stream(ctx, name)
}

func (s *stallingJS) Consumer(ctx context.Context, stream, name string) (jetstream.Consumer, error) {
	if s.take(&s.consume) {
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
	for _, unanswered := range []error{
		context.DeadlineExceeded,
		nats.ErrTimeout,
		nats.ErrNoResponders,
	} {
		t.Run(unanswered.Error(), func(t *testing.T) {
			q := newQueue(t)
			ctx := t.Context()

			topic := topics.AgentInbox("unanswered-" + sanitizeName(unanswered.Error()))
			group := topics.AgentInboxGroup("unanswered-" + sanitizeName(unanswered.Error()))

			// BOTH PROBES AT ONCE, because a subscribe reaches
			// both: EnsureSubscription looks the consumer up and
			// streamFor may look the stream up on the way.
			real := q.js
			q.js = &stallingJS{JetStream: real, streams: 1, consume: 1, err: unanswered}

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
	q := newQueue(t)
	ctx := t.Context()

	refused := errors.New("nats: authorization violation")
	real := q.js
	q.js = &stallingJS{JetStream: real, consume: 1, err: refused}

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
