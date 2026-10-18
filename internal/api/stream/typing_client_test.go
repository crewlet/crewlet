package stream_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD RE-ASSERTS A TYPING FLAG WELL INSIDE THE ENGINE'S EXPIRY.
//
// The hub forgets a typing flag nobody re-asserted within [stream.ChatTypingTTL]
// — that is what clears the indicator under a tab whose lid closed mid-word —
// and the chat screen re-asserts at most every `TYPING_THROTTLE_MS`
// (`contract/chat.ts`) while somebody types. The TTL is sized as TWO of those
// intervals, so a re-assert that came late is invisible and a second one is
// not. Neither side can see the other's number, and the drift is silent in the
// one direction that matters: a throttle past half the TTL blinks the indicator
// off and on under somebody who never stopped typing, and nothing anywhere
// fails. So the relation is held here, by the package that owns the TTL.
func TestTheDashboardReassertsTypingInsideTheEnginesExpiry(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "TYPING_THROTTLE_MS")
	if err != nil {
		// FAILS rather than skips: the dashboard source is committed, so a
		// missing declaration is a renamed one.
		t.Fatal(err)
	}
	ms, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("TYPING_THROTTLE_MS is %q, which is not an integer: %v", raw, err)
	}
	throttle := time.Duration(ms) * time.Millisecond
	if throttle <= 0 {
		t.Fatalf("TYPING_THROTTLE_MS is %v; a typing flag re-asserted on every "+
			"keystroke is a fleet-wide presence probe per character", throttle)
	}
	if 2*throttle > stream.ChatTypingTTL {
		t.Errorf("the dashboard re-asserts typing every %v and the engine forgets a "+
			"flag after %v, so one late re-assert blinks the indicator off under "+
			"somebody still typing — TYPING_THROTTLE_MS in contract/chat.ts must be "+
			"at most half the engine's TTL", throttle, stream.ChatTypingTTL)
	}
}
