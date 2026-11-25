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
// A pass hands a delivery to a peer ([queue.OutcomePass]), so its spacing ages
// by the count every node shares, the broker's delivery count: a per-node count
// would return a message each older node passes at that node's first second,
// spending its budget in a fraction of the ten minutes it spans. Recording a
// failure would start its first genuine failure up the curve, and nothing is
// wrong with the node, so it keeps taking mail. Read off the message, since no
// broker reports back what the attachment asked of it.
func TestAPassIsSpacedByItsDeliveriesAndIsNoFailure(t *testing.T) {
	t.Parallel()
	a := &attachment{
		q:   &Queue{cfg: Config{NakDelay: time.Second, NakCeiling: 30 * time.Second}},
		log: slog.New(slog.DiscardHandler),
	}
	msg := &answeredMsg{md: &jetstream.MsgMetadata{NumDelivered: 4,
		Sequence: jetstream.SequencePair{Stream: 7}}}

	a.apply(t.Context(), delivery{msg: msg, ev: &events.Event{Type: "external_notification"}},
		queue.Pass("newer_record", errors.New("a peer on a newer build reads it")))
	// Owed until the loop's next delivery is in hand
	// (TestASpacedReturnGoesBackOnlyOnceTheLoopHoldsItsNextDelivery).
	a.sendSpaced()

	if len(msg.delays) != 1 || msg.naked != 0 || msg.termed != 0 {
		t.Fatalf("a pass of a fourth delivery was answered with spaced returns %v, %d "+
			"immediate ones and %d terminations; want one spaced return",
			msg.delays, msg.naked, msg.termed)
	}
	// The fourth spacing is eight seconds (1, 2, 4, 8), spread by a fifth
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
// [attachment.applyPartition] answers a partition in a switch of its own, so it
// owes each message what the single path owes a pass. An immediate return
// (handBack) is sent at once, not owed behind the next drain, and reaches the
// broker beside that drain's pull in either order, so queuetest's
// a_passed_partition_returns_for_a_peer_and_the_rest_of_its_drain_runs cannot
// catch one here, and the spacing is read off the messages instead.
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
	a.sendSpaced()

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

// answeredMsg is a delivered message that records how it was answered (spaced
// returns, immediate ones, in-progress acknowledgements, terminations, each
// also told to onTerm when set).
// Anything else asked of it panics on the nil embedded interface: a case asking
// for more than it set up.
type answeredMsg struct {
	jetstream.Msg
	md         *jetstream.MsgMetadata
	data       []byte
	onTerm     func()
	delays     []time.Duration
	naked      int
	termed     int
	acked      int
	progressed int
}

func (m *answeredMsg) Ack() error { m.acked++; return nil }

func (m *answeredMsg) Metadata() (*jetstream.MsgMetadata, error) { return m.md, nil }

func (m *answeredMsg) Data() []byte { return m.data }

func (m *answeredMsg) NakWithDelay(delay time.Duration) error {
	m.delays = append(m.delays, delay)
	return nil
}

func (m *answeredMsg) Nak() error { m.naked++; return nil }

func (m *answeredMsg) InProgress() error { m.progressed++; return nil }

func (m *answeredMsg) Term() error {
	m.termed++
	if m.onTerm != nil {
		m.onTerm()
	}
	return nil
}
