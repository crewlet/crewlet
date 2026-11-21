package stream

import (
	"time"

	"github.com/crewlet/crewlet/internal/presence"
)

// MaxInFlightQueries bounds how many queries ONE SOCKET may have running at
// once.
//
// FOUR is a tab's share of the READER POOL, which is sized from it:
// internal/store's reader floor is eight, "two full dashboards at four queries
// each", so a change to either is a change to both. PER SOCKET: three tabs
// offer twelve, and a burst past the pool waits in its queue. A larger
// allowance would only park a fifth query in `database/sql`'s wait instead of
// here. Who is acting runs on the store's RESERVED connection
// (identityReserve), so it never waits. Not sized to a screen's burst: it
// waits HERE because the bound is taken on the read loop's goroutine (see
// readLoop). Gitlet's pages of code go over REST around it.
const MaxInFlightQueries = 4

// MaxRoomsPerSocket bounds how many artifact rooms ONE SOCKET is in at once.
//
// EIGHT. A tab shows one artifact at a time and keeps its room while it does,
// so an honest client is in one or two (a page and the artifact it opened);
// eight leaves room for a screen that previews a few side by side while
// bounding what one socket can make this node hold: a presence member, a hub
// membership and a decision on every narrowing, per room.
const MaxRoomsPerSocket = 8

// presenceFrames is one socket's allowance of `presence` frames: at most
// [presence.ClientRate] a second, sustained and in a burst, the rate
// the dashboard throttles itself to. A frame past it is dropped, which loses
// nothing a person sees for long: the next frame says where they are now, and
// the room's fan-out is coalesced to fewer than this anyway. It protects the
// node from a socket that does not throttle, never an honest one.
type presenceFrames struct {
	tokens float64
	last   time.Time
}

// take spends one frame's allowance at now, reporting whether there was one.
func (b *presenceFrames) take(now time.Time) bool {
	const rate = float64(presence.ClientRate)
	if b.last.IsZero() {
		b.tokens = rate
	} else {
		b.tokens = min(rate, b.tokens+now.Sub(b.last).Seconds()*rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
