package topics

import (
	"strings"

	"github.com/google/uuid"
)

// THE WAITING CHANNEL: that a person's message reached a seat's priority lane,
// told to whichever node runs the seat so it can show the person their message
// is queued behind what the seat is already doing.
//
// EPHEMERAL, on the queue's broadcast verb and under no log's prefix, for the
// presence channel's reason: what it says is worth nothing once the seat picks
// the message up, so a stored copy would be a replicated write per message
// that nobody reads back, and a node that misses one shows no queued line,
// which is the state before this channel existed. The message itself rides
// the seat's durable inbox; this is only the news that it is there.
const (
	// SeatWaitingPrefix prefixes every waiting subject.
	SeatWaitingPrefix = "crewlet.waiting"

	// SeatWaitingAll matches every seat's waiting subject, for the node
	// that listens for the seats it runs.
	SeatWaitingAll = SeatWaitingPrefix + ".*"
)

// SeatWaiting is the waiting subject of the seat with this id, or "" for no
// seat, which is not routable.
func SeatWaiting(seat uuid.UUID) string {
	if seat == uuid.Nil {
		return ""
	}
	return SeatWaitingPrefix + "." + seat.String()
}

// SeatFromWaiting recovers the seat id from a waiting subject, reporting
// whether the subject was one: the exact inverse of [SeatWaiting].
func SeatFromWaiting(subject string) (uuid.UUID, bool) {
	rest, ok := strings.CutPrefix(subject, SeatWaitingPrefix+".")
	if !ok || strings.Contains(rest, ".") {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(rest)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}
