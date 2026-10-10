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
// # A socket is pushed what its reach is served
//
// Every socket carries the reach its handshake resolved (ADR-0031), and every
// push kind names the lowest reach it is sent to ([KindReach]) in ONE table
// that covers every [Kind] — TestEveryPushKindHasAReach reads the constants
// from this package's source and fails on one the table leaves out, so a new
// kind is a decision rather than a broadcast to everybody. A kind whose
// content differs by reach is built once PER REACH a connected client holds,
// never once per client, and each client is sent the richest variant its own
// reach covers ([Variants]): the `agents` push is an admin's overlay and a
// member's [livestate.AgentStatus], so a member's tab never holds the prompt a
// seat is running on. The snapshot a socket is handed on connect is assembled
// for its reach from the same table, and a key a frame presents upgrades that
// one question and never the pushes.
package stream

import (
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
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
// or any other tab, and it must not be disconnected either: the reconnect flow
// refetches a snapshot, so dropping is recoverable and a disconnect is a
// visible failure for a reader who did nothing wrong.
//
// Oldest rather than newest, because what a dashboard shows is the CURRENT
// state: the newest envelope is the one that makes the screen right, and
// dropping it to keep an older one would leave the tab further behind than
// doing nothing.
const QueueDepth = 512

// Kind is what a frame from the socket is: a push, one of the two answers to a
// query, or the pong.
//
// A NAMED TYPE, so the one place a kind is spelled is a constant below and
// every frame is built from one. The dashboard's `PushKind` union is its copy
// of exactly these constants — `contract/wire.ts`, held both ways by
// `TestTheDashboardKnowsExactlyThePushKindsTheEngineSends`, which reads every
// constant of this type from this package's source — and the socket's
// dispatch falls silently through a kind that union does not name. So a kind
// is frozen once a dashboard renders it: renaming one is a broken client, not
// a refactor.
type Kind string

// The push kinds.
const (
	KindSnapshot  Kind = "snapshot"
	KindEvent     Kind = "event"
	KindAgents    Kind = "agents"
	KindSeats     Kind = "seats"
	KindSandboxes Kind = "sandboxes"
	KindTokens    Kind = "tokens"
	KindBudget    Kind = "budget"
	KindSchedules Kind = "schedules"
	KindOrg       Kind = "org"
	KindTools     Kind = "tools"
	KindHealth    Kind = "health"

	// KindResult and KindError answer one query, correlated by the
	// client-minted id it was asked under.
	KindResult Kind = "result"
	KindError  Kind = "error"

	KindPong Kind = "pong"
)

// Envelope is one server-to-client frame.
//
// ID, What and Error are omitted on a push and present on a query answer, which
// is what lets one type carry both directions of the protocol without a client
// having to know which shape to expect from which kind.
type Envelope struct {
	Kind  Kind   `json:"kind"`
	Data  any    `json:"data,omitempty"`
	TS    string `json:"ts,omitempty"`
	ID    int64  `json:"id,omitempty"`
	What  string `json:"what,omitempty"`
	Error string `json:"error,omitempty"`

	// RetryAfterSeconds is how long to wait before asking again, on an
	// `unavailable` error and nowhere else: the socket's Retry-After, from
	// [RetryAfterSeconds] — the helper the REST 503's header comes from, so
	// a screen waits the same time whichever transport refused it.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`

	// Detail is the refusal's own sentence, on a `bad_params` error and
	// nowhere else — see [RefusedError] for why that one and only that one.
	Detail string `json:"detail,omitempty"`
}

// Push builds a broadcast envelope stamped now.
func Push(kind Kind, data any, now time.Time) Envelope {
	return Envelope{Kind: kind, Data: data, TS: now.UTC().Format(time.RFC3339Nano)}
}

// route is how a frame of one kind reaches a client.
type route int

const (
	// routeBroadcast is a push the hub fans out to every client whose
	// reach covers the kind's.
	routeBroadcast route = iota + 1
	// routeHandshake is the one frame a socket is handed as it connects,
	// assembled for that socket's own reach and sent to it alone.
	routeHandshake
	// routeReply answers one client's own frame. Its reach is the
	// question's (the query registry's), never the kind's, and it is
	// never broadcast.
	routeReply
)

// delivery is who a kind is sent to, and how.
type delivery struct {
	route route
	// reach is the lowest reach a client must hold to be sent a push of
	// the kind at all. [auth.ReachOpen] for a handshake or a reply, which
	// every socket receives: what each CARRIES is decided per reach (the
	// snapshot) or per question (an answer).
	reach auth.Reach
}

// kinds is every frame kind's delivery: TOTAL over [Kind], which
// TestEveryPushKindHasAReach holds against the constants above.
//
// THE WITHHOLDING RULE OF ADR-0031, one kind at a time. `health` is open
// because it is the body of the open `/health` probe, so withholding the push
// would be cosmetic; `org` is public because the chart is the company's public
// face (and what the public reach serves of it is the org projection's own
// classification); `agents`, `seats` and `budget` are member because who is
// working on what and which seats are out of budget is what the company
// published — each with a member variant that carries the state and none of
// what the machine processed to reach it — and so is `schedules`, the routines
// a person plans around. `event`, `sandboxes`, `tokens` and `tools` are what
// the machine processed and how it is run: whole event payloads, coding runs
// and their boxes, spend, and every tool every server serves with its schema.
var kinds = map[Kind]delivery{
	KindSnapshot:  {routeHandshake, auth.ReachOpen},
	KindEvent:     {routeBroadcast, auth.ReachAdmin},
	KindAgents:    {routeBroadcast, auth.ReachMember},
	KindSeats:     {routeBroadcast, auth.ReachMember},
	KindSandboxes: {routeBroadcast, auth.ReachAdmin},
	KindTokens:    {routeBroadcast, auth.ReachAdmin},
	KindBudget:    {routeBroadcast, auth.ReachMember},
	KindSchedules: {routeBroadcast, auth.ReachMember},
	KindOrg:       {routeBroadcast, auth.ReachPublic},
	KindTools:     {routeBroadcast, auth.ReachAdmin},
	KindHealth:    {routeBroadcast, auth.ReachOpen},
	KindResult:    {routeReply, auth.ReachOpen},
	KindError:     {routeReply, auth.ReachOpen},
	KindPong:      {routeReply, auth.ReachOpen},
}

// KindReach is the lowest reach a client must hold to be PUSHED kind, and
// false for a kind that is never broadcast — the snapshot, a query's answer and
// the pong, each of which is sent to one client — or one this build does not
// know.
func KindReach(kind Kind) (auth.Reach, bool) {
	d, ok := kinds[kind]
	if !ok || d.route != routeBroadcast {
		return "", false
	}
	return d.reach, true
}

// Variants builds one push's data for a reach: the richest variant a client
// holding that reach is served. Called at most once per reach per broadcast,
// and only for a reach some connected client holds, so a variant nobody is
// connected to read is never built. ok false sends clients of that reach
// nothing.
type Variants func(reach auth.Reach) (data any, ok bool)

// Same is the Variants of a push every reach it is sent to reads alike, from
// data already in hand.
func Same(data any) Variants {
	return func(auth.Reach) (any, bool) { return data, true }
}

// Shared is the Variants of a push every reach reads alike, built by build at
// most once — and not at all when no client is sent it.
func Shared(build func() any) Variants {
	data := sync.OnceValue(build)
	return func(auth.Reach) (any, bool) { return data(), true }
}

// Client is one connected dashboard.
type Client struct {
	// out carries envelopes to the socket's own writer goroutine. Buffered
	// to QueueDepth; a send that would block drops the oldest instead.
	out chan Envelope

	// reach is what the socket's handshake resolved, fixed for the life of
	// the connection: a key a frame presents upgrades its own question and
	// never this.
	reach auth.Reach

	mu      sync.Mutex
	closed  bool
	dropped int
}

// NewClient builds a client with an empty queue, pushed what reach is served.
// A reach that is not one of [auth.Reaches] covers nothing, so such a client is
// pushed nothing at all rather than read as any reach.
func NewClient(reach auth.Reach) *Client {
	return &Client{out: make(chan Envelope, QueueDepth), reach: reach}
}

// Reach is the reach this client is pushed for.
func (c *Client) Reach() auth.Reach { return c.reach }

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

// Broadcast pushes kind to every client whose reach covers the kind's, each
// sent the variant built for its own reach.
//
// ONCE PER REACH, NEVER PER CLIENT: a variant is built the first time a client
// of its reach is reached and that one envelope is queued for every other
// client of the same reach, so a hundred member tabs cost one member build —
// and a reach nobody connected with costs none.
//
// A kind that is not a push — the snapshot, an answer, the pong, or one this
// build does not know — is refused and logged: each goes to one client, and
// fanning one out would hand every socket another's answer.
//
// Never blocks on any client: each client's own queue absorbs the difference,
// and one that has fallen behind loses its oldest envelope rather than holding
// up the publish path.
func (h *Hub) Broadcast(kind Kind, build Variants, now time.Time) {
	need, ok := KindReach(kind)
	if !ok {
		log.Error("stream_broadcast_refused", "kind", kind,
			"hint", "only a push kind is broadcast; a snapshot, an answer and a "+
				"pong are sent to the one client they are for")
		return
	}
	h.mu.RLock()
	targets := slices.Collect(maps.Keys(h.clients))
	h.mu.RUnlock()

	type built struct {
		env  Envelope
		send bool
	}
	byReach := map[auth.Reach]built{}
	for _, c := range targets {
		if !c.reach.Covers(need) {
			continue
		}
		variant, done := byReach[c.reach]
		if !done {
			data, send := build(c.reach)
			variant = built{env: Push(kind, data, now), send: send}
			byReach[c.reach] = variant
		}
		if variant.send {
			c.send(variant.env)
		}
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
