// Package stream is the dashboard's live channel: one WebSocket per tab,
// carrying the RESULT of applying each event to the server's projection.
//
// The fan-out is separated from the transport, and the split is structural
// rather than stylistic. [Hub] holds the clients, the per-client queue and the
// backpressure rule, and no WebSocket is reachable from it — so "a slow tab
// loses its oldest envelope rather than stalling every other tab" is a property
// a test can assert directly, instead of one inferred from a socket that
// happened not to fall over.
//
// # A drop is repaired, and on the wire
//
// The other half of that rule is what happens to the connection the loss
// happened on, and it is two things. The SERVER REPAIRS what it can: the
// socket's writer checks the count after every frame it writes, and once it
// has moved it sends a fresh [KindSnapshot] — see writeLoop in socket.go — so a
// pushed state delta a slow tab lost is rebuilt without the tab having to
// notice. And
// the loss is TOLD: a frame carries [Envelope.Dropped], the running count of
// what this connection has lost, omitted entirely while that is zero, so the
// evidence reaches the tab itself rather than only an operator reading logs
// after the reader closed it. See that field for why it is a field rather than
// a frame of its own, why it is cumulative, and what no resync can return.
//
// THE REPAIR IS THE SERVER'S rather than the client's because it is the same
// repair for every client — a snapshot the projection can build at any moment
// — and a client that ignores the count still receives it.
package stream

import (
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("api.stream")

// writeTimeout bounds ONE frame written to a client.
//
// A TCP peer that vanishes without a FIN — a closed laptop, a dropped NAT
// entry, a mobile handoff — leaves a Write blocked until the kernel gives up,
// which is minutes with default keepalives. Until then the socket's goroutine,
// its queue and its hub registration stay live, so a page nobody is reading
// holds a slot on every broadcast.
//
// THIRTY SECONDS, and it is deliberately generous: [QueueDepth] below decides
// that a slow tab must not be disconnected, so this exists to tell GONE from
// SLOW and nothing else. A tighter deadline would sever a mobile tab
// mid-snapshot and contradict that decision.
const writeTimeout = 30 * time.Second

// QueueDepth is how many envelopes one client may have waiting.
//
// Drop-OLDEST past it, never block. A slow tab must not stall the publish path
// or any other tab, and it must not be disconnected either: a disconnect is a
// visible failure for a reader who did nothing wrong, where falling behind is
// REPAIRED by the snapshot the writer sends once it has written another frame,
// and TOLD on the frames that land ([Envelope.Dropped]).
//
// WHAT A DROP COSTS IS PER KIND, and it is stated that way rather than as a
// blanket "a snapshot rebuilds it, so a drop is recoverable" — which is true
// of the state pushes and false of the answers beside them in this same
// queue. See WHAT IT HOLDS IS NOT ONLY SUPERSEDED STATE below, and
// [Envelope.Dropped] for what is recovered of each.
//
// Oldest rather than newest, because what a dashboard shows is the CURRENT
// state: the newest envelope is the one that makes the screen right, and
// dropping it to keep an older one would leave the tab further behind than
// doing nothing.
//
// The remedy is owed to the READER and not only to the protocol, so it
// happens while the tab is connected rather than waiting for a reconnect that
// a still-connected tab never performs.
//
// Five hundred and twelve: the depth is what a tab may fall behind by before
// its screen needs repairing, and the answer to a tab that exceeds it is a
// fresh snapshot for the pushes — which the writer sends — and the question
// again for the answers below, rather than a deeper buffer. A larger queue
// only delays that repair while holding more superseded state per connection,
// and a smaller one would drop during an ordinary render pause, which is the
// case the buffer exists for. What would justify moving it is a measured drop
// rate on a company's real ingest, which [Envelope.Dropped] and the
// `stream_client_left_behind` log line make observable.
//
// WHAT IT HOLDS IS NOT ONLY SUPERSEDED STATE, and that is the part not to
// forget when moving it. [KindResult] and [KindError] — one query's answer,
// correlated by an id its client minted — ride this same queue under the same
// drop-oldest rule, and no snapshot carries them, so dropping one costs a
// client its answer until it asks again rather than costing it a number that
// the next push corrects. The depth is therefore a bound on answers in flight
// as well as on state deltas; five hundred and twelve is far above either,
// since a screen has a handful of queries outstanding at a time and a
// projection publishes a few frames a second.
const QueueDepth = 512

// The push kinds. Frozen — the dashboard ships unchanged and is the
// compatibility reference, so a renamed kind is a broken client, not a
// refactor.
const (
	KindSnapshot  = "snapshot"
	KindEvent     = "event"
	KindAgents    = "agents"
	KindSeats     = "seats"
	KindSandboxes = "sandboxes"
	KindTokens    = "tokens"
	KindBudget    = "budget"
	KindSchedules = "schedules"
	KindOrg       = "org"
	KindTools     = "tools"
	KindHealth    = "health"

	// KindResult and KindError answer one query, correlated by the
	// client-minted id it was asked under.
	KindResult = "result"
	KindError  = "error"

	KindPong = "pong"
)

// Envelope is one server-to-client frame.
//
// ID, What and Error are omitted on a push and present on a query answer, which
// is what lets one type carry both directions of the protocol without a client
// having to know which shape to expect from which kind.
type Envelope struct {
	Kind  string `json:"kind"`
	Data  any    `json:"data,omitempty"`
	TS    string `json:"ts,omitempty"`
	ID    int64  `json:"id,omitempty"`
	What  string `json:"what,omitempty"`
	Error string `json:"error,omitempty"`

	// Dropped is how many envelopes THIS CONNECTION has lost to
	// backpressure before this frame: a running total, monotonic for the
	// life of the socket, stamped per client by [Client.send] because
	// [Hub.Broadcast] hands each client its own copy of the value.
	//
	// A DROP IS REPORTED, NOT INFERRED. [QueueDepth] decides that a tab
	// behind by more than its queue loses envelopes rather than stalling
	// the publish path, and that decision is right — but a dropped envelope
	// is a state delta, so the tab that lost one is rendering something
	// other than the truth with nothing on screen saying so. Until this
	// field existed the count reached a log line at [Hub.Unregister] and
	// nowhere else: evidence for an operator reading logs, after the reader
	// who was shown the wrong number had already closed the tab.
	//
	// WHAT IS RECOVERED, and it turns on WHICH frame was lost. The
	// envelopes themselves are gone and none is individually recoverable,
	// so the remedy is never a replay — but the kinds of frame in this
	// queue recover differently, and a blanket "the snapshot rebuilds it"
	// is false for exactly the half that looks like a fault.
	//
	// A lost STATE PUSH is a delta, and everything it carried is in the
	// projection: once the socket's writer has written another frame it
	// sends a fresh [KindSnapshot], built from the projection then, so the
	// tab is repaired without doing anything. A lost `event` push is
	// half-recovered: its feed row is in that snapshot while it is among
	// the newest [livestate.EventFeedLimit], but the payload the push
	// carried is not — the snapshot's feed rows carry none — and the event
	// rows are durable besides, in `crewlet_events`, which the `events`
	// and `event` queries read.
	//
	// A lost ANSWER is not recovered. [KindResult] and [KindError] travel
	// through this same queue under the same drop-oldest rule, and an
	// answer is correlated by an id its client minted rather than being
	// state any projection holds — so no snapshot returns it and the
	// question has to be ASKED AGAIN. Nothing here re-sends it: the
	// client's own answer timeout is what notices, which on the dashboard
	// is the `timeout` code its socket raises itself (see
	// vocabulary_test.go's client-only codes).
	//
	// A FIELD RATHER THAN A FRAME OF ITS OWN, which was the obvious
	// alternative and is wrong twice over. A notice queued as an envelope
	// competes for the very slot it is reporting on — it displaces a real
	// state frame in order to say a state frame was displaced — and under a
	// burst the queue fills with notices about notices. Riding on the frame
	// that DID land costs no slot and cannot itself be dropped.
	//
	// CUMULATIVE RATHER THAN A PER-FRAME DELTA, for the same reason: a
	// delta lives on exactly one frame and that frame is as droppable as
	// any other, so one lost delta under-reports for the rest of the
	// connection. A running total is self-healing — whichever frame next
	// reaches the client carries the whole truth — and a client that only
	// ever compares it against the last value it saw needs no other state.
	//
	// Absent on the wire while it is zero, which is every frame of every
	// connection that keeps up, so a client that ignores it sees exactly
	// the bytes it saw before the field existed.
	Dropped int `json:"dropped,omitempty"`
}

// Push builds a broadcast envelope stamped now.
func Push(kind string, data any, now time.Time) Envelope {
	return Envelope{Kind: kind, Data: data, TS: now.UTC().Format(time.RFC3339Nano)}
}

// Client is one connected dashboard.
type Client struct {
	// out carries envelopes to the socket's own writer goroutine. Buffered
	// to QueueDepth; a send that would block drops the oldest instead.
	out chan Envelope

	mu      sync.Mutex
	closed  bool
	dropped int
}

// NewClient builds a client with an empty queue.
func NewClient() *Client {
	return &Client{out: make(chan Envelope, QueueDepth)}
}

// Out is the channel a transport reads envelopes from. Closed by [Client.Close].
func (c *Client) Out() <-chan Envelope { return c.out }

// Dropped reports how many envelopes this client has lost to backpressure.
//
// Reported rather than merely counted: a tab that is behind is a tab showing
// something other than the truth, and the number is the only evidence of it
// that survives the drop. It reaches TWO readers, deliberately — the tab
// itself, on [Envelope.Dropped] of the next frame that lands; and an
// operator, in the [Hub.Unregister] log line, which is the only place a
// connection that dropped envelopes and then went away without ever draining
// another frame is visible at all.
func (c *Client) Dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// send queues an envelope, dropping the oldest if the queue is full.
//
// Never blocks. The caller is the ingest path, shared by every client, so one
// slow reader blocking here would stop the whole fan-out.
//
// The lock is held ACROSS the channel operations, not merely around the closed
// check. Checking and then releasing leaves a window in which Close runs and
// the send lands on a closed channel — which panics, and takes the whole
// broadcast with it. The two ends genuinely race: the transport closes when its
// socket dies while the ingest path is fanning out. Holding it is cheap because
// nothing here blocks: every operation is a select with a default.
func (c *Client) send(env Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	for {
		// STAMPED INSIDE THE LOOP, so a frame that had to displace one to
		// get in carries the total INCLUDING that displacement. Stamping
		// once before the loop would have every drop reported one frame
		// late, and the last drop of a burst never reported at all if the
		// connection then goes quiet. env is this call's own copy —
		// [Hub.Broadcast] passes by value — so this is per client.
		env.Dropped = c.dropped
		select {
		case c.out <- env:
			return
		default:
		}
		// Full. Take one off the front and try again. The take can only
		// fail if the transport's reader emptied the queue in between,
		// and then the next send succeeds — so the loop always makes
		// progress. Looping rather than dropping the NEW envelope is
		// what keeps the newest state on screen.
		select {
		case <-c.out:
			c.dropped++
		default:
		}
	}
}

// supersededBySnapshot is every push kind whose whole content a [KindSnapshot]
// carries again, built from the projection at the moment it is built.
//
// It is what lets a resync ([Client.catchUp]) DISCARD a queued frame of one of
// these kinds rather than deliver it after the fresher snapshot: everything it
// said is in the snapshot, and delivered afterwards it would set its part of
// the screen back to an older value until the next push of its kind.
//
// [KindEvent] IS NOT HERE, deliberately. The snapshot's `events` are feed rows,
// which carry no payload, while an `event` push carries the whole envelope —
// so an event push holds what no snapshot does, and a resync that discarded one
// would lose it. Nor are the ANSWERS — [KindResult], [KindError], [KindPong] —
// which reply to one client frame and are held by no projection at all. A kind
// missing from this set is KEPT, which is the safe default: a kept frame is
// delivered, and a discarded one is gone.
var supersededBySnapshot = map[string]bool{
	KindSnapshot:  true,
	KindAgents:    true,
	KindSeats:     true,
	KindSandboxes: true,
	KindTokens:    true,
	KindBudget:    true,
	KindSchedules: true,
	KindOrg:       true,
	KindTools:     true,
	KindHealth:    true,
}

// catchUp empties the queue for a resync, and reports what it took.
//
// It returns, in queue order, every frame a snapshot does NOT supersede (see
// [supersededBySnapshot]), the highest [Envelope.Dropped] any frame it took
// carried, and whether the queue is still open. The superseded frames are
// discarded: they are not lost, because the snapshot the caller builds next
// carries what they did — and they are not counted as drops.
//
// BOUNDED BY WHAT WAS QUEUED WHEN IT STARTED, so a publisher filling the queue
// as fast as this empties it cannot hold the writer here; whatever arrives
// after the count was taken is left for the ordinary loop.
//
// No lock: [Client.send] takes a frame off the front under its own lock when
// the queue is full, and two receivers on one channel are safe. Whichever of
// them takes a given frame, it is taken once.
func (c *Client) catchUp() (kept []Envelope, latest int, open bool) {
	for range len(c.out) {
		select {
		case env, ok := <-c.out:
			if !ok {
				return kept, latest, false
			}
			latest = max(latest, env.Dropped)
			if !supersededBySnapshot[env.Kind] {
				kept = append(kept, env)
			}
		default:
			return kept, latest, true
		}
	}
	return kept, latest, true
}

// Close stops the client and releases its queue.
//
// Idempotent, because both ends can reach it: the transport closes when the
// socket dies, and the hub closes when it is shutting down.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.out)
}

// Hub is the set of connected clients.
type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}
}

// NewHub builds an empty hub.
func NewHub() *Hub { return &Hub{clients: map[*Client]struct{}{}} }

// Register adds a client to the fan-out.
//
// BEFORE its snapshot is sent, deliberately. The window between registering and
// snapshotting delivers envelopes describing state the snapshot also carries —
// which is harmless, because the client dedupes streamed envelopes against the
// snapshot by event id. Registering AFTER would instead lose everything
// published in that window, which nothing recovers.
func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
}

// Unregister removes a client and closes it.
func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	_, present := h.clients[c]
	delete(h.clients, c)
	h.mu.Unlock()
	if present {
		if dropped := c.Dropped(); dropped > 0 {
			// THE HINT IS PER KIND, because the operator is the one
			// reader who cannot check it against the code. A resync
			// rebuilds the pushed state and returns no query answer, so
			// a blanket "the snapshot repaired it" would tell an operator
			// the tab recovered everything it lost. See [Envelope.Dropped].
			log.Info("stream_client_left_behind", "dropped", dropped,
				"hint", "the tab could not keep up and lost envelopes; a "+
					"loss is followed by a fresh snapshot once the connection "+
					"writes another frame, which rebuilds pushed state but "+
					"returns no answer to a query the tab had asked — those "+
					"are sent once and must be asked again")
		}
	}
	c.Close()
}

// Clients reports how many are connected.
func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Broadcast queues an envelope for every client.
//
// Never blocks on any of them: each client's own queue absorbs the difference,
// and one that has fallen behind loses its oldest envelope rather than holding
// up the publish path.
func (h *Hub) Broadcast(env Envelope) {
	h.mu.RLock()
	targets := slices.Collect(maps.Keys(h.clients))
	h.mu.RUnlock()

	for _, c := range targets {
		c.send(env)
	}
}

// Close disconnects every client.
func (h *Hub) Close() {
	h.mu.Lock()
	targets := slices.Collect(maps.Keys(h.clients))
	h.clients = map[*Client]struct{}{}
	h.mu.Unlock()

	for _, c := range targets {
		c.Close()
	}
}

// Encode serializes an envelope for the wire.
//
// Here rather than at the transport so the frame a test asserts about is the
// frame a browser receives, byte for byte.
func Encode(env Envelope) ([]byte, error) { return json.Marshal(env) }
