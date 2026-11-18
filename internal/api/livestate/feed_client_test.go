package livestate_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD KEEPS THE FEED THE ENGINE KEEPS.
//
// A tab trims its activity feed to `MAX_EVENTS` and the projection keeps, and
// a snapshot ships, [livestate.EventFeedLimit]: one number written twice.
// Shorter on the client and a reconnect's snapshot is cut to fit; longer and
// the tab holds rows no snapshot can resend, so a refresh loses them. Either
// way the screens saying "this tab keeps the last N events" state a wrong N.
//
// Read through [clientsource.Scalar], which takes one literal and refuses an
// expression — `MAX_EVENTS = 200 * 2` would otherwise be read as half of it.
func TestTheDashboardKeepsTheFeedTheEngineKeeps(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "MAX_EVENTS")
	if err != nil {
		t.Fatal(err)
	}
	// Base 0 reads what TypeScript writes: a `0x` prefix, and the `_`
	// separators both languages take.
	client, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("MAX_EVENTS is %q, which is not an integer: %v", raw, err)
	}
	if client != livestate.EventFeedLimit {
		t.Errorf("the dashboard keeps %d feed rows and the engine keeps and ships %d — "+
			"change MAX_EVENTS in contract/wire.ts to the engine's number",
			client, livestate.EventFeedLimit)
	}
}
