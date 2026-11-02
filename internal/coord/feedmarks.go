package coord

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// FeedMark is how far a wake feed's fleet-wide group had acknowledged on one
// log, as the broker last reported it to a node consuming the group.
//
// # Why the group's own position is not enough
//
// A durable consumer IS the fleet's position on its log, and while it exists
// nothing else is needed: a restart, a reopen after a broker blip and a peer
// joining all resume from it. But a consumer can go while its log stays (an
// operator's delete, a broker that lost its consumer state), and the group is
// then created again. Created with nothing to start from, it starts at the
// log's first retained record, which on a company's chat or tracker log is a
// week of records: every one of them was derived into a wake again, and the
// two dedupe layers behind the feed remember minutes, not days, so agents
// were woken a second time for work they had already done.
//
// So each node consuming a group records the group's acknowledgement floor
// here as it advances, and a group created again starts at the record after
// it. What is replayed is what was acknowledged in the moments before the
// mark was last written, which those dedupe layers do collapse.
//
// # In the positions bucket
//
// A key class beside the node rows and the trim floors, for the reason they
// share it: it has no age. A mark that expired would read as a group that has
// never been consumed, which is the replay it exists to prevent.
type FeedMark struct {
	// Stream is the log's stream name, and Group the feed's group as its
	// domain declares it ([statelog.Domain.FeedGroup]). Together they are
	// the key: one group on two logs is two marks, as it is two consumers.
	Stream string `json:"stream"`
	Group  string `json:"group"`

	// Generation is the log's generation the floor is a sequence of. A
	// reanchor renumbers a log, and a floor from another generation names
	// a record in a number space the log no longer has, so a reader on
	// another generation does not start from it.
	Generation uint32 `json:"generation"`

	// Floor is the group's acknowledgement floor: every record at or below
	// it was handled by some node. A group created again starts at
	// Floor+1.
	Floor uint64 `json:"floor"`

	// At is when the mark was written, for an operator reading it.
	At time.Time `json:"at"`
}

// FeedMarkRegister holds every wake feed's mark.
type FeedMarkRegister interface {
	// RaiseFeedMark writes a mark unless the one held for its log and group
	// is at the same generation and already at or above its floor, and
	// reports whether it wrote. A mark at a higher generation replaces one
	// at a lower, whatever its floor.
	//
	// RAISE-ONLY, because every node consuming the group writes it, from a
	// floor it read a moment earlier: two nodes' writes can land in the
	// opposite order to their reads, and a plain write would then move the
	// mark back to a floor the group has already passed.
	RaiseFeedMark(ctx context.Context, m FeedMark) (bool, error)

	// FeedMark reads one log's mark for one group, and false when none has
	// been written.
	FeedMark(ctx context.Context, stream, group string) (FeedMark, bool, error)
}

// FeedMarkKey is a mark's key in the register.
func FeedMarkKey(stream, group string) string { return DocumentKey("feedmark", stream, group) }

// Validate reports why a mark cannot be written.
func (m FeedMark) Validate() error {
	switch {
	case strings.TrimSpace(m.Stream) == "":
		return fmt.Errorf("coord: a feed mark names no stream, and the stream is half its key")
	case strings.TrimSpace(m.Group) == "":
		return fmt.Errorf("coord: the feed mark on %s names no group, and the group is half its key",
			m.Stream)
	case m.Floor == 0:
		return fmt.Errorf("coord: the feed mark for %s on %s is at 0, which says no record was "+
			"handled and is what an absent mark already says", m.Group, m.Stream)
	}
	return nil
}

// Supersedes reports whether m should replace held: a later generation, or
// the same generation at a higher floor.
func (m FeedMark) Supersedes(held FeedMark) bool {
	if m.Generation != held.Generation {
		return m.Generation > held.Generation
	}
	return m.Floor > held.Floor
}
