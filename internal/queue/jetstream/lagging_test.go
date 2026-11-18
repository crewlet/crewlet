package jetstream

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
	"github.com/crewlet/crewlet/internal/queue/queuetest"
)

// leafLag is how late a lagging leaf link delivers every chunk, each way: half
// a second, so a round trip to the member costs a second (sized against
// #1204's red-first runs). The lag is set only once the client is up
// (awaitLeafReady), so the link still forms; at 700 ms the consumer's own
// creation did not finish.
const leafLag = 500 * time.Millisecond

// The leaf's queue windows through a link that lags leafLag, in place of
// testTimings', which are sized for a loopback that answers at once.
//
// EVERY WINDOW THAT WAITS FOR AN ANSWER HAS TO HOLD THE ROUND TRIP, or the lag
// does not slow a delivery down, it loses it: the client listens for a fetch's
// expiry plus nats.go's fixed second, and a handout crosses back a round trip
// after the fetch was sent. A handout that lands after its fetch stopped
// listening comes round an ack window later into the same race, forever.
const (
	// lagFetchWait is two round trips, so a fetch listens for three
	// seconds: a redelivery handed over about a second in (lagNakDelay)
	// crosses back with most of a second to spare.
	lagFetchWait = 4 * leafLag
	// lagNakDelay spaces each redelivery a round trip after the failure
	// reaches the member, flat (the ceiling is the delay): the handout lands
	// a second into the fetch sent beside the Nak, where a doubling would
	// push the third onto the window's end. It also puts the redelivery
	// case's attempts two seconds apart, so a wait bounded at three seconds
	// from its start cuts it every run.
	lagNakDelay = 2 * leafLag
	// lagAckWait is ten round trips: a handout's ack takes about three
	// seconds to reach the member here, past testTimings' two, which would
	// redeliver an event its handler still held; ten still redelivers a lost
	// handout well inside queuetest's stallFor.
	lagAckWait = 20 * leafLag
)

// A WAIT THAT KEEPS RECEIVING IS NEVER CUT, through a leaf whose link lags as
// a loaded broker does: the cases pass with every request answered a second
// late, round trip, for as long as their deliveries keep coming.
// queuetest.RunLagged says which cases and why only those.
//
// Mutation: bound queuetest's journal waits at three seconds from their start,
// and both cases fail here.
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
