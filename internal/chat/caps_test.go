package chat_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/node"
)

// TestTheBroadcastCapIsTheNodesTurnBudget keeps two numbers that are one
// number from drifting apart in silence.
//
// [chat.MaxCollectiveRecipients] is how many agents one `@channel` wakes, and
// it is not a number chosen for chat: it is [node.DefaultMaxConcurrent], the
// turns one process runs at once. The reasoning only works while they are
// equal. A person in a busy room reads passively and an agent has no passive
// read, so every wake here is a two-phase turn against that budget — and one
// broadcast that exceeds it takes the node's entire capacity for a message
// addressed to nobody in particular, with every other trigger queued behind
// it.
//
// THEY CANNOT BE ONE CONSTANT. This package is the wire vocabulary of a
// state-log domain and has no business importing the seat host; the
// dependency exists in this TEST file and nowhere in the shipped code, which
// is the same arrangement `store.DefaultReaderConns` has with the socket's
// query concurrency. So each names the other in its doc, and this is what
// makes the pair a fact rather than a comment — the tree has already paid for
// the alternative, most recently in three prose counts of the coordination
// estate that went stale together and that nothing went red over.
func TestTheBroadcastCapIsTheNodesTurnBudget(t *testing.T) {
	t.Parallel()
	if chat.MaxCollectiveRecipients != node.DefaultMaxConcurrent {
		t.Fatalf("one @channel wakes up to %d agents against a node that runs "+
			"%d turns at once. Above the budget a single broadcast occupies the "+
			"whole node and every other trigger queues behind a message "+
			"addressed to nobody in particular; below it the cap is refusing "+
			"wakes the node could have served. Move both, or write down here "+
			"why a broadcast's fan-out and a node's turn budget are no longer "+
			"the same number",
			chat.MaxCollectiveRecipients, node.DefaultMaxConcurrent)
	}
}
