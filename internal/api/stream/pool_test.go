package stream_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/store"
)

// TestTheQueryConcurrencyIsTheStoresReadPool is the only thing standing
// between two constants that are one number.
//
// A socket admits [stream.MaxInFlightQueries] queries at once and each takes a
// connection from a pool sized [store.DefaultReaderConns]. Set the pool
// SMALLER and the extra queries have not started — they are queuing for a
// connection, which is what the `pool_starved` alarm names and what a person
// waiting on a first paint actually sees. Set it LARGER and the surplus
// connections only deepen a queue, because under WAL writers serialise on the
// file lock however many readers there are.
//
// They cannot be one constant: [store] is a leaf that `config` itself depends
// on, so importing the socket would be a cycle. So each names the other in its
// own doc, and this asserts the pair, because a comment is not a gate — and
// the failure mode of a drift is silent on both sides.
func TestTheQueryConcurrencyIsTheStoresReadPool(t *testing.T) {
	t.Parallel()
	if stream.MaxInFlightQueries != store.DefaultReaderConns {
		t.Fatalf("a socket admits %d concurrent queries against a read pool of "+
			"%d connections. Below the pool the spare connections deepen a "+
			"queue nobody is served from; above it every query past the pool "+
			"queues before it starts and raises pool_starved. Move both, or "+
			"write down here why the screen's burst and the pool are no longer "+
			"the same number",
			stream.MaxInFlightQueries, store.DefaultReaderConns)
	}
}
