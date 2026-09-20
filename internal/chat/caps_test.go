package chat_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/coord"
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

// TestTheRailIsBoundedByTheCursorsAPersonCanKeep holds the second pair of
// constants this package documents as one number.
//
// [chat.MaxRailChannels] is how many rooms a person's channel list answers
// for, and it is [coord.MaxReadCursors] — the cap the read-state record puts
// on one person's cursors. Past that cap the record has already dropped its
// stalest cursor to stay inside its own bound, so a room listed beyond it
// could not carry an unread badge that was ever right: the rail would show a
// number derived from a cursor nothing is keeping.
//
// A RAIL SMALLER than the cursor cap is merely wasteful. A rail LARGER is the
// defect, and it is silent: the extra rooms render, their badges read zero,
// and zero is indistinguishable from caught up.
func TestTheRailIsBoundedByTheCursorsAPersonCanKeep(t *testing.T) {
	t.Parallel()
	if chat.MaxRailChannels != coord.MaxReadCursors {
		t.Fatalf("the channel rail answers for %d rooms while a person keeps "+
			"%d read cursors. Above the cursor cap a room's badge is derived "+
			"from a cursor the record has already evicted, and it renders as "+
			"caught up rather than as unknown. Move both, or write down here "+
			"why the rail may list a room whose unread count cannot be kept",
			chat.MaxRailChannels, coord.MaxReadCursors)
	}
}
