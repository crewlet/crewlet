// Package stream is the dashboard's live channel: one WebSocket per tab,
// carrying the RESULT of applying each event to the server's projection.
//
// The fan-out is separated from the transport, and the split is structural
// rather than stylistic. [Hub] holds the clients, the per-client queue and the
// backpressure rule, and no WebSocket is reachable from it — so "a slow tab
// loses its oldest envelope rather than stalling every other tab" is a property
// a test can assert directly, instead of one inferred from a socket that
// happened not to fall over.
package stream

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("api.stream")

// QueueDepth is how many envelopes one client may have waiting.
//
// Drop-OLDEST past it, never block. A slow tab must not stall the publish path
// or any other tab, and it must not be disconnected either: the reconnect flow
// refetches a snapshot, so dropping is recoverable and a disconnect is a
// visible failure for a reader who did nothing wrong.
//
// Oldest rather than newest, because what a dashboard shows is the CURRENT
// state: the newest envelope is the one that makes the screen right, and
// dropping it to keep an older one would leave the tab further behind than
// doing nothing.
const QueueDepth = 512

// The push kinds. Frozen — the dashboard ships unchanged and is the
// compatibility reference, so a renamed kind is a broken client, not a
// refactor. See rewrite/decisions/502-dashboard-wire-protocol.md.
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
// that survives the drop.
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
			log.Info("stream_client_left_behind", "dropped", dropped,
				"hint", "the tab could not keep up and lost envelopes; its "+
					"reconnect refetches a snapshot")
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
	targets := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	for _, c := range targets {
		c.send(env)
	}
}

// Close disconnects every client.
func (h *Hub) Close() {
	h.mu.Lock()
	targets := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		targets = append(targets, c)
	}
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
