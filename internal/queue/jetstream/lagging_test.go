package jetstream

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
	"github.com/crewlet/crewlet/internal/queue/queuetest"
)

// leafLag is how late a lagging leaf link delivers every chunk, each way: half
// a second, so a round trip to the member costs a second, every time.
//
// Sized against #1204's red-first runs, which measured the redelivery case's
// attempts seconds apart through it. Its consumer and the leaf's link still
// come up, because the lag is set only once the client is up, and the client
// is up only once the leaf has linked and the member's JetStream has answered
// across the link (awaitLeafReady); at 700 ms the consumer's own creation did
// not finish.
const leafLag = 500 * time.Millisecond

// The leaf's queue windows through a link that lags leafLag, in place of
// testTimings', which are sized for a loopback that answers at once.
//
// EVERY WINDOW THAT WAITS FOR AN ANSWER HAS TO HOLD THE ROUND TRIP, or the lag
// does not slow a delivery down, it loses it. The client listens for a fetch's
// messages for the fetch's expiry and one second more (nats.go's pull
// consumer, a fixed second), and a message the member hands to that fetch
// comes back a round trip after the fetch was sent. At testTimings' 25 ms poll
// that left 25 ms for everything but the lag. Beside two spinning goroutines,
// as on a loaded runner, the member handed one event out six times and the
// handler saw it once, ten seconds late; at a lag of 520 ms, seven times and
// never. Each handout crossed back after its fetch had stopped listening and
// came round again an ack window later into the same race, which is how CI
// failed the redelivery case with nothing delivered for 30 s.
const (
	// lagFetchWait is two round trips, so a fetch listens for three
	// seconds: a redelivery, handed over about a second into the fetch
	// (lagNakDelay), crosses back with most of a second to spare, and only
	// a message that lands in the last moment of a window is lost, to come
	// round again an ack window later.
	lagFetchWait = 4 * leafLag
	// lagNakDelay spaces each redelivery a round trip after the failure
	// reaches the member, flat (the ceiling is the delay), so the handout
	// lands a second into the fetch the failing loop sent beside its Nak,
	// two seconds before that fetch stops listening, where a doubling would
	// push the third onto the window's end. It is also what puts the
	// redelivery case's attempts two seconds apart, so a wait bounded at
	// three seconds from its start cuts it every run.
	lagNakDelay = 2 * leafLag
	// lagAckWait is ten round trips: what a handout spends before its ack
	// reaches the member here (a tail fetch's wait, a publish's round trip
	// from inside the handler, the ack's own crossing) is about three seconds,
	// past testTimings' two, which could redeliver an event its handler
	// still held. Ten still redelivers a handout lost on its way back well
	// inside queuetest's stallFor.
	lagAckWait = 20 * leafLag
)

// A WAIT THAT KEEPS RECEIVING IS NEVER CUT, through a leaf whose link to its
// member lags as a loaded broker does: the cases a loaded CI runner's pauses
// cut on main while they were still delivering (#1204) pass when every request
// is answered a second late, round trip, for as long as their deliveries keep
// coming. queuetest.RunLagged says which cases and why only those.
//
// Mutation: bound queuetest's journal waits at three seconds from their start,
// as settleFor did, and both cases fail here.
func TestCasesThroughALaggingLeaf(t *testing.T) {
	t.Parallel()
	queuetest.RunLagged(t, func(t *testing.T, opts ...queue.Option) queue.EventQueue {
		return openLaggingLeafForTest(t, Config{}, opts...)
	}, capabilitiesFor(func(t *testing.T, cfg Config) *Queue { return openLaggingLeafForTest(t, cfg) }))
}

// openLaggingLeafForTest is openLeafForTest whose leaf link lags leafLag each
// way once its client is up, with the leaf queue's windows sized to that lag
// wherever cfg leaves them unset.
func openLaggingLeafForTest(t *testing.T, cfg Config, opts ...queue.Option) *Queue {
	t.Helper()
	if cfg.FetchWait == 0 {
		cfg.FetchWait = lagFetchWait
	}
	if cfg.NakDelay == 0 && cfg.NakCeiling == 0 {
		cfg.NakDelay, cfg.NakCeiling = lagNakDelay, lagNakDelay
	}
	if cfg.AckWait == 0 {
		cfg.AckWait = lagAckWait
	}
	var link *linktest.Link
	q := openLeafAcross(t, cfg, func(member string) string {
		link = linktest.Lagging(t, member)
		return link.Addr()
	}, opts...)
	link.Lag(leafLag)
	return q
}
