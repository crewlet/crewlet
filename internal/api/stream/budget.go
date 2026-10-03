package stream

// MaxInFlightQueries bounds how many queries ONE SOCKET may have running at
// once.
//
// # Four, and per socket
//
// FOUR is what one dashboard tab needs running at once, and it is the number
// internal/store's reader floor is sized from: eight, so that one full tab's
// four fit inside the socket surface's half of the pool (see [queryCeiling])
// and the engine's own reads keep the other four — the two numbers are one
// decision stated twice, so a change to either is a change to both. A larger
// allowance would not run a fifth query any sooner on a small host: it would
// park it at the node's ceiling or in `database/sql`'s wait for a connection
// rather than here, holding a goroutine against everybody else's queries
// instead of against this tab's.
//
// PER SOCKET, which is the unit every connection-oriented server bounds a
// client by: a person's second tab runs its own four rather than waiting on
// their first. It used to be per PRINCIPAL, a budget shared and refcounted
// across every socket a person held, and that machinery bounded the wrong
// thing — one person, where what has to be bounded is the POOL.
//
// It is NOT sized to a screen's burst. The shell keeps five reads standing on
// every page — the viewer, the Inbox count, My work's count, the projects and
// the pinned views — two asked the moment a tab opens and three the moment the
// viewer answers, so a screen's first reads queue behind them for the length
// of a tracker read, and after the first paint the five poll on independent
// 60 s to 5 min timers and rarely coincide. That queue is the design working:
// a burst waits HERE, on its own socket — and it queues rather than piling up
// because the bound is taken on the read loop's own goroutine (see readLoop),
// so a store scan cannot stall the live feed.
const MaxInFlightQueries = 4

// queryCeiling is how many queries EVERY SOCKET ON A NODE may have running at
// once, together, given how many connections ordinary reads may hold on its
// store (internal/store's DB.Readers): half of them, and never fewer than
// one.
//
// # Why there is a node-wide ceiling at all
//
// [MaxInFlightQueries] bounds one socket and nothing bounds how many sockets
// there are: any caller holding `state:read` can open as many as it likes, and
// N sockets at four queries each take every reader the store has. What queues
// behind them is the ENGINE's own reads — a seat's tool lookups mid-turn, the
// coverage probes, the /health body — in the same pool, first come first
// served. The store's reserved connection keeps exactly one read out of that
// queue, resolving who is acting, and nothing else; so without this ceiling
// one account and a script that opens sockets could starve every seat turn
// and health probe on a node.
//
// # Why half
//
// The socket surface is the one reader population an outside caller drives,
// and the engine's own reads are what run the company; neither may starve the
// other, and the store sizes its pool for both — its floor of eight is one
// full tab in the sockets' half and the same again for the engine's. Half
// moves with the host as the pool does (max(8, GOMAXPROCS)), because a larger
// node runs more seats whose reads need the other half.
//
// # Where a query waits for it
//
// In its own goroutine, after taking its socket's slot and with the socket's
// context, never on the read loop: the read loop answers the keepalive, and a
// ceiling some OTHER socket's burst is holding must not stop this one's pings.
// The per-socket slot taken first is what bounds how many goroutines wait
// here — four per socket at most.
func queryCeiling(readers int) int {
	return max(1, readers/2)
}
