package stream_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD ASKS NO MORE AT ONCE THAN THIS SOCKET RUNS.
//
// A screen that asks a burst of lookups of its own — the chat screen's room
// details, one `chat_channel` read per room it offers — bounds the burst to
// one wave of [stream.MaxInFlightQueries], because past that number the reader
// stops taking frames until a query finishes and every read the screen asks
// next waits behind the burst. So the dashboard carries the number
// (`MAX_IN_FLIGHT_QUERIES` in `contract/wire.ts`) and derives its bound from
// it, which only works while the two agree. Above the engine's, the burst
// takes two waves and holds the rail and the transcript behind its own
// lookups — the bound read eight while this socket ran four, under a comment
// saying it was one wave. Below it, a list is cut shorter than the socket
// would have answered in the same time.
func TestTheDashboardAsksNoMoreAtOnceThanTheSocketRuns(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "MAX_IN_FLIGHT_QUERIES")
	if err != nil {
		// FAILS rather than skips: the dashboard source is committed, so a
		// missing declaration is a renamed one.
		t.Fatal(err)
	}
	client, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("MAX_IN_FLIGHT_QUERIES is %q, which is not an integer: %v", raw, err)
	}
	if client != stream.MaxInFlightQueries {
		t.Errorf("the dashboard bounds a burst of its own lookups at %d and this socket "+
			"runs %d queries at once — change MAX_IN_FLIGHT_QUERIES in contract/wire.ts "+
			"to the engine's number", client, stream.MaxInFlightQueries)
	}
}
