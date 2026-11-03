package stream

import (
	"time"

	"github.com/crewlet/crewlet/internal/presence"
)

// MaxInFlightQueries bounds how many queries ONE SOCKET may have running at
// once.
//
// # Four, and per socket
//
// FOUR is a dashboard tab's share of the node's READER POOL, and it is the
// number that pool is sized from: internal/store's reader floor is eight,
// derived as "two full dashboards at four queries each", so this constant and
// that floor are one decision stated twice — a change to either is a change to
// both. The pool is shared with the engine's own reads (a seat's tool lookups,
// the coverage probes, the health body), and a larger allowance would not run
// a fifth query any sooner on a small host: it would park it in
// `database/sql`'s wait for a connection rather than here, holding a goroutine
// and a connection's worth of queue against the engine's reads instead of
// against this tab's.
//
// PER SOCKET, which is the unit that floor counts — a dashboard is a tab — and
// the unit every connection-oriented server bounds a client by. A person with
// three tabs open offers twelve concurrent queries, and that is accepted
// rather than engineered away: a burst past the pool waits in the pool's own
// queue, and the one read that must never wait behind it — resolving who is
// acting — runs on the store's RESERVED connection, which no ordinary read may
// take (internal/store's identityReserve). It used to be per PRINCIPAL, a
// budget shared by every socket a person held and refcounted across them, and
// that machinery bought a bound the reserve already gives where it matters.
//
// It is NOT sized to a screen's burst. The shell keeps five reads standing on
// every page — the viewer, the Inbox count, My work's count, the projects and
// the pinned views — two asked the moment a tab opens and three the moment the
// viewer answers, so a screen's first reads queue behind them for the length
// of a tracker read, and after the first paint the five poll on independent
// 60 s to 5 min timers and rarely coincide. That queue is the design working:
// a burst waits HERE, on its own socket, rather than in the pool every reader
// on the node shares — and it queues rather than piling into the pool because
// the bound is taken on the read loop's own goroutine (see readLoop), so a
// store scan cannot stall the live feed.
//
// AND THE DASHBOARD READS ITS PAGES OF CODE AROUND IT: a repository listing,
// a push history and, once a repository's pages land, its trees, files and
// diffs are answers a reader asks for many of, so the Code workspace asks them
// over REST rather than queue them here in front of the shell's own reads
// (dashboard/src/lib/useCodeQuery.ts). A change to this bound is a change to
// that reason.
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
