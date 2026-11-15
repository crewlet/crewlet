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
// Sized against #1204's red-first runs. At this lag the suite's old bound, three
// seconds from a wait's start, cut both cases every run, and the redelivery
// case's attempts came two to six seconds apart. Its consumer and the leaf's
// link still come up, because the lag is set only once the link is up (at 700
// ms the consumer's own creation did not finish).
const leafLag = 500 * time.Millisecond

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
// way once its client is up.
func openLaggingLeafForTest(t *testing.T, cfg Config, opts ...queue.Option) *Queue {
	t.Helper()
	var link *linktest.Link
	q := openLeafAcross(t, cfg, func(member string) string {
		link = linktest.Lagging(t, member)
		return link.Addr()
	}, opts...)
	link.Lag(leafLag)
	return q
}
