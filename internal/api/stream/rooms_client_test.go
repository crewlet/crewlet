package stream_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD JOINS NO MORE ARTIFACT ROOMS THAN A SOCKET HOLDS.
//
// The socket refuses a listener that would be one room past the bound at once,
// rather than sending a join the engine answers `bad_params`; a bound looser
// than the engine's would send that join and report the refusal a round trip
// later, and one tighter would refuse a panel the engine would have served.
func TestTheDashboardJoinsNoMoreRoomsThanTheEngineHolds(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "MAX_ARTIFACT_ROOMS")
	if err != nil {
		t.Fatal(err)
	}
	client, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("MAX_ARTIFACT_ROOMS is %q, which is not an integer: %v", raw, err)
	}
	if client != stream.MaxRoomsPerSocket {
		t.Errorf("the dashboard joins at most %d rooms per socket and the engine holds %d: "+
			"change contract/wire.ts to the engine's figure", client, stream.MaxRoomsPerSocket)
	}
}
