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
// The durable consumer is the group's position while it exists, but it can go
// while its log stays (an operator's delete, lost consumer state), and a group
// created from nothing starts at the log's first retained record: days of
// records re-derived into wakes, past the minutes the feed's dedupe layers
// remember. So every consuming node records the acknowledgement floor here and
// a recreated group starts after it; only the last moments replay, which the
// dedupe collapses. No age: an expired mark would read as a group never
// consumed, the replay it exists to prevent ([PositionKey]).
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
	// reports whether it wrote. A higher generation replaces a lower one,
	// whatever its floor.
	//
	// RAISE-ONLY because every consuming node writes from a floor it read a
	// moment earlier: two writes can land in the opposite order to their
	// reads, and a plain write would move the mark back.
	RaiseFeedMark(ctx context.Context, m FeedMark) (bool, error)

	// FeedMark reads one log's mark for one group, and false when none has
	// been written.
	FeedMark(ctx context.Context, stream, group string) (FeedMark, bool, error)
}

// FeedMarkKey is a mark's key in the register.
func FeedMarkKey(stream, group string) string { return DocumentKey(FeedMarkClass, stream, group) }

// FeedMarkClass is the key class a feed mark is filed under in the positions
// register.
const FeedMarkClass = "feedmark"

// Validate reports why a mark cannot be written.
func (m FeedMark) Validate() error {
	switch {
	case strings.TrimSpace(m.Stream) == "":
		return fmt.Errorf("coord: a feed mark names no stream, and the stream is half its key")
	case strings.TrimSpace(m.Group) == "":
		return fmt.Errorf("coord: the feed mark on %s names no group, and the group is half its key",
			m.Stream)
	case !ValidStreamName(m.Stream) || !ValidDomainName(m.Group):
		return fmt.Errorf("coord: a feed mark names %q on %q, which is no feed group on a log's "+
			"stream", m.Group, m.Stream)
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
