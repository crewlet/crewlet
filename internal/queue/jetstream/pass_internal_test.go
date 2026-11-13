package jetstream

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// A PASS IS SPACED BY ITS MESSAGE'S DELIVERY COUNT, RECORDS NO FAILURE AND
// QUIESCES NOTHING.
//
// A pass hands a delivery to a peer ([queue.OutcomePass]), so the count its
// spacing ages by is the one every node shares, the broker's count of the
// message's deliveries: a per-node count would bring a message the group's
// older nodes pass round back at each one's first second, and spend its budget
// in a fraction of the ten minutes it spans. It is no failure, so this node's
// record of the message's failures does not move, which would start its first
// genuine failure up the curve. And nothing is wrong with the node, so its
// attachment keeps taking mail.
//
// Read off the message the pass answers rather than off a broker, because what
// is asserted is what the attachment asks the broker for, which no broker
// reports back.
//
// Mutation: space a pass by this attachment's failures (nakOrDeadLetter), and
// the fourth delivery's pass waits a second and records a failure; quiesce on
// a pass, and the attachment stops taking mail.
func TestAPassIsSpacedByItsDeliveriesAndIsNoFailure(t *testing.T) {
	t.Parallel()
	a := &attachment{
		q:   &Queue{cfg: Config{NakDelay: time.Second, NakCeiling: 30 * time.Second}},
		log: slog.New(slog.DiscardHandler),
	}
	msg := &answeredMsg{md: &jetstream.MsgMetadata{NumDelivered: 4,
		Sequence: jetstream.SequencePair{Stream: 7}}}

	a.apply(t.Context(), msg, &events.Event{Type: "external_notification"},
		queue.Pass("newer_record", errors.New("a peer on a newer build reads it")))

	if len(msg.delays) != 1 || msg.naked != 0 || msg.termed != 0 {
		t.Fatalf("a pass of a fourth delivery was answered with spaced returns %v, %d "+
			"immediate ones and %d terminations; want one spaced return",
			msg.delays, msg.naked, msg.termed)
	}
	// The fourth spacing is eight seconds — 1, 2, 4, 8 — spread by a fifth
	// either way.
	spacing := a.q.nakSpacing(4)
	if low, high := spacing-spacing/5, spacing+spacing/5; msg.delays[0] < low || msg.delays[0] > high {
		t.Errorf("a pass of a fourth delivery waits %v, want the fourth spacing %v "+
			"(within [%v, %v]): the count it ages by is the message's own", msg.delays[0],
			spacing, low, high)
	}
	a.failuresMu.Lock()
	failures := len(a.failures)
	a.failuresMu.Unlock()
	if failures != 0 {
		t.Errorf("a pass recorded %d failure(s) of the message: a pass is no failure", failures)
	}
	if a.quiesced.Load() {
		t.Error("a pass quiesced the attachment: nothing is wrong with a node that " +
			"passes a message, and a quiesced one takes none of its other mail")
	}
}

// A PASSED PARTITION IS SPACED, MESSAGE BY MESSAGE, BY EACH ONE'S OWN DELIVERY
// COUNT, RECORDS NO FAILURE AND QUIESCES NOTHING.
//
// The batch path answers a partition's outcome in a switch of its own
// ([attachment.applyPartition]), so it owes each of its messages what the single
// path owes a pass. Spaced, a passed partition comes back behind the mail that
// was waiting when it was passed — the order queuetest's
// a_passed_partition_returns_for_a_peer_and_the_rest_of_its_drain_runs asserts
// on every backend. On the broker that order is the spacing's, because a
// return made at once and the next fetch reach the broker through two queues
// of its own, and which it takes first is theirs: the conformance case cannot
// catch a return made at once here, so what the attachment asks the broker for
// is read off the messages, as for the single path.
//
// Mutation: return a passed partition at once, as a hand-back (handBack), and
// neither message is spaced.
func TestAPassedPartitionIsSpacedByEachMessagesDeliveries(t *testing.T) {
	t.Parallel()
	a := &attachment{
		q:   &Queue{cfg: Config{NakDelay: time.Second, NakCeiling: 30 * time.Second}},
		log: slog.New(slog.DiscardHandler),
	}
	second := &answeredMsg{md: &jetstream.MsgMetadata{NumDelivered: 2,
		Sequence: jetstream.SequencePair{Stream: 7}}}
	fifth := &answeredMsg{md: &jetstream.MsgMetadata{NumDelivered: 5,
		Sequence: jetstream.SequencePair{Stream: 8}}}
	evs := []*events.Event{{Type: "external_notification"}, {Type: "external_notification"}}
	items := []delivery{{msg: second, ev: evs[0]}, {msg: fifth, ev: evs[1]}}

	a.applyPartition(t.Context(), "conversation-1", items, evs,
		queue.Pass("newer_record", errors.New("a peer on a newer build reads it")))

	for _, m := range []*answeredMsg{second, fifth} {
		if len(m.delays) != 1 || m.naked != 0 || m.termed != 0 {
			t.Fatalf("a passed partition's delivery %d was answered with spaced returns %v, "+
				"%d immediate ones and %d terminations; want one spaced return",
				m.md.NumDelivered, m.delays, m.naked, m.termed)
		}
		spacing := a.q.nakSpacing(m.md.NumDelivered)
		if low, high := spacing-spacing/5, spacing+spacing/5; m.delays[0] < low || m.delays[0] > high {
			t.Errorf("a passed partition's delivery %d waits %v, want its own spacing %v "+
				"(within [%v, %v])", m.md.NumDelivered, m.delays[0], spacing, low, high)
		}
	}
	a.failuresMu.Lock()
	failures := len(a.failures)
	a.failuresMu.Unlock()
	if failures != 0 {
		t.Errorf("a passed partition recorded %d failure(s): a pass is no failure", failures)
	}
	if a.quiesced.Load() {
		t.Error("a passed partition quiesced the attachment: nothing is wrong with a node " +
			"that passes a message, and a quiesced one takes none of its other mail")
	}
}

// answeredMsg is a delivered message that records what it was answered: the
// spaced returns ([jetstream.Msg.NakWithDelay]), the immediate ones and the
// terminations, each termination told to onTerm where a case set one. Its
// payload is data. Anything else it is asked panics on the nil interface it
// embeds, which is a case asking for more than it set up.
type answeredMsg struct {
	jetstream.Msg
	md     *jetstream.MsgMetadata
	data   []byte
	onTerm func()
	delays []time.Duration
	naked  int
	termed int
	acked  int
}

func (m *answeredMsg) Ack() error { m.acked++; return nil }

func (m *answeredMsg) Metadata() (*jetstream.MsgMetadata, error) { return m.md, nil }

func (m *answeredMsg) Data() []byte { return m.data }

func (m *answeredMsg) NakWithDelay(delay time.Duration) error {
	m.delays = append(m.delays, delay)
	return nil
}

func (m *answeredMsg) Nak() error { m.naked++; return nil }

func (m *answeredMsg) Term() error {
	m.termed++
	if m.onTerm != nil {
		m.onTerm()
	}
	return nil
}
