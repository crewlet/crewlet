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
// # What a client is, and what decides its bytes
//
// A [Client] carries two things beyond its queue: the [FramePosture] it is
// being served in, and the seat it has asked to be a recipient for. The
// posture decides the BYTES — one encode serves every client in the same
// posture — and the seat decides the AUDIENCE for the kinds routed by seat.
// Everything else about the fan-out follows from those two and from the total
// route table below.
//
// # Who may be a recipient for a seat
//
// The hub INDEXES a watch and decides nothing about it; the socket decides it
// ([watching]), the way the `work_inbox` question decides a read of the same
// seat's inbox — its holder, whoever leads it, the admin grant — through the
// same three-valued chart seam, and re-decides it on every credential re-check.
// The one seat-routed kind, `inbox_changed`, says whose inbox moved, when and
// why; an index anyone could write to would be a way to follow somebody else's
// inbox that the question itself refuses.
//
// AND IT IS RESOLVED BEFORE IT IS DECIDED, through [iam.OwnerOf] and the same
// identity directory that question reads: a frame is pushed to the name a
// person's notices are kept under — their seat, when the directory binds them
// to one — so a watch naming their login is installed on that seat, and the
// directory is asked only once the watch has been decided as far as it can
// be without the record.
package stream

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
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

// QueueDepth is how many frames one client may have waiting.
//
// Drop-OLDEST past it, never block. A slow tab must not stall the publish path
// or any other tab, and it must not be disconnected either: the reconnect flow
// refetches a snapshot, so dropping is recoverable and a disconnect is a
// visible failure for a reader who did nothing wrong.
//
// Oldest rather than newest, because what a dashboard shows is the CURRENT
// state: the newest frame is the one that makes the screen right, and
// dropping it to keep an older one would leave the tab further behind than
// doing nothing.
const QueueDepth = 512

// The push kinds. Frozen against RENAMING — the dashboard is the
// compatibility reference, so a renamed kind is a broken client, not a
// refactor. A NEW kind is additive: the client's dispatch ignores a kind it
// does not know, which is what lets the engine add one before the screen
// that renders it. Every one of them has an entry on [routes]; see
// TestEveryPushKindHasARoute.
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

	// KindInboxChanged says one seat's inbox moved, and it is the first
	// kind routed to an AUDIENCE rather than to everyone: it carries the
	// seat in [Envelope.Seat] and reaches only the clients that asked to
	// watch that seat (see [Hub.Watch]) and were allowed to (see
	// [watching]).
	//
	// IT IS HOW A PERSON LEARNS THEY HAVE WORK. The tracker's applier, on
	// every node, says whose inbox each committed batch moved, and each
	// node pushes that to its OWN sockets through [Service.InboxChanged] —
	// every node applies every record, so no node forwards to another. The
	// payload is an [InboxChange]: identifiers and a count, never content.
	KindInboxChanged = "inbox_changed"

	// KindIdentity tells ONE client whether this node could verify the
	// credential its socket was opened with, at the last revalidation
	// (see [RevalidateEvery]). A DIRECT kind, because it is a fact about
	// this socket and not about the company: a node that is perfectly
	// healthy can hold one tab whose session it cannot check while its
	// identity applier is behind, and the node-wide health frame would
	// report that node as fine. Direct is also what lets it reach a client
	// whose posture the hold itself has degraded.
	KindIdentity = "identity"

	// KindResult and KindError answer one query, correlated by the
	// client-minted id it was asked under.
	KindResult = "result"
	KindError  = "error"

	KindPong = "pong"
)

// routes is how each kind reaches the clients it is for. See [Route].
//
// BESIDE THE KINDS, and total over them: a kind declared above with no entry
// here has no route at all, which [RouteOf] answers as the invalid zero Route
// and TestEveryPushKindHasARoute fails the build over.
var routes = map[string]Route{
	// The company's own state. What one tab sees, every tab sees.
	KindSnapshot:  RouteDirect,
	KindEvent:     RouteBroadcast,
	KindAgents:    RouteBroadcast,
	KindSeats:     RouteBroadcast,
	KindSandboxes: RouteBroadcast,
	KindTokens:    RouteBroadcast,
	KindBudget:    RouteBroadcast,
	KindSchedules: RouteBroadcast,
	KindOrg:       RouteBroadcast,
	KindTools:     RouteBroadcast,
	KindHealth:    RouteBroadcast,

	// One seat's audience.
	KindInboxChanged: RouteSeat,

	// This socket's own identity, to this socket alone.
	KindIdentity: RouteDirect,

	// The answers, to the one client that asked.
	KindResult: RouteDirect,
	KindError:  RouteDirect,
	KindPong:   RouteDirect,
}

// needs is the grant a reader must carry to receive each kind that says
// something about the COMPANY.
//
// BESIDE THE ROUTES and total over the kinds together with [answers]: a route
// says who a frame is addressed to, and this says who may read it at all. The
// socket used to decide only that its caller was somebody, which was harmless
// while the only credentials were operator tokens and became a firehose the
// day a session cookie resolved: a person enrolled with `work:write` alone
// opened the socket and received every phase's prompt and response, which
// the `events` question beside it refuses without `audit:read`.
//
// EACH KIND TAKES THE GRANT ITS QUESTION TAKES, so no fact is reachable by
// choosing a channel: an event is the `events` query's row, so `audit:read`;
// everything else here is what the dashboard's `state:read` questions answer.
// A kind in neither table is received by NOBODY — see [Audience.Receives].
var needs = map[string]iam.Grant{
	KindSnapshot:     iam.GrantStateRead,
	KindEvent:        iam.GrantAuditRead,
	KindAgents:       iam.GrantStateRead,
	KindSeats:        iam.GrantStateRead,
	KindSandboxes:    iam.GrantStateRead,
	KindTokens:       iam.GrantStateRead,
	KindBudget:       iam.GrantStateRead,
	KindSchedules:    iam.GrantStateRead,
	KindOrg:          iam.GrantStateRead,
	KindTools:        iam.GrantStateRead,
	KindHealth:       iam.GrantStateRead,
	KindInboxChanged: iam.GrantStateRead,
}

// answers are the kinds that say nothing about the company, only about this
// socket's own exchange — which is why no grant stands in front of them. A
// query's result or refusal was already decided by the grant its question is
// registered under, and deciding it again here would be a second opinion that
// could only ever disagree; the identity frame and a pong carry nothing but
// the socket's own state.
var answers = map[string]bool{
	KindIdentity: true,
	KindResult:   true,
	KindError:    true,
	KindPong:     true,
}

// Audience is what one reader may receive, decided from the grants it
// carries.
//
// ITS ZERO VALUE RECEIVES NO COMPANY FACT AT ALL, which is the closed end and
// the only safe one: a reader nobody described is not one this node knows may
// see anything.
type Audience struct {
	grants []iam.Grant
}

// AudienceOf is the audience a principal's grants describe.
func AudienceOf(grants []iam.Grant) Audience {
	return Audience{grants: slices.Clone(grants)}
}

// Receives reports whether this audience may read a frame of kind.
func (a Audience) Receives(kind string) bool {
	if answers[kind] {
		return true
	}
	grant, gated := needs[kind]
	if !gated {
		return false
	}
	return iam.Principal{Grants: a.grants}.Can(grant)
}

// GrantFor is the grant a reader needs to receive kind, and whether kind is a
// company fact at all rather than an answer to a socket's own exchange.
func GrantFor(kind string) (iam.Grant, bool) {
	grant, gated := needs[kind]
	return grant, gated
}

// same reports whether two audiences carry the same grants, in any order.
func (a Audience) same(b Audience) bool {
	x, y := slices.Clone(a.grants), slices.Clone(b.grants)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// Envelope is one server-to-client frame.
//
// ID, What and Error are omitted on a push and present on a query answer, which
// is what lets one type carry both directions of the protocol without a client
// having to know which shape to expect from which kind.
//
// Error is an [httpjson.Code] rather than a string, and that is the whole of
// what makes "the socket and REST cannot call one refusal two things" a
// compile-time fact rather than a convention: a frame can only carry a value
// from the engine's one refusal vocabulary. The query-answer subset is named
// in socket.go.
type Envelope struct {
	Kind  string        `json:"kind"`
	Data  any           `json:"data,omitempty"`
	TS    string        `json:"ts,omitempty"`
	ID    int64         `json:"id,omitempty"`
	What  string        `json:"what,omitempty"`
	Error httpjson.Code `json:"error,omitempty"`

	// Seat names which seat a [RouteSeat] frame is about, and is the
	// address the fan-out routes on rather than a label beside it.
	//
	// ON THE WIRE, because a client watching a seat has to be able to tell
	// two seats' frames apart if it ever watches more than one — and a
	// routing key the receiver cannot read is a key only the sender can
	// debug. Omitted on every other kind, so no existing frame changes
	// shape.
	Seat string `json:"seat,omitempty"`

	// *Refused is a refusal on AUTHORITY's machine-readable half, on an
	// `unauthorized` error frame and nowhere else: the rule's reason, and
	// the grants any one of which would have admitted the caller —
	// FLATTENED into the frame beside `error`, under the keys the REST
	// envelope answers the same refusal with ([authz.DetailReason],
	// [authz.DetailGrants]). The socket used to drop both, so a question a
	// REST caller was refused with `{error, reason, grants}` came back over
	// the socket as a bare code, and the dashboard — whose only data
	// channel this is — could not tell a missing grant from a missing
	// relation. Nil on every other frame, which omits both keys.
	*Refused
}

// Refused is the reason and the grants an `unauthorized` frame carries.
//
// GRANTS IS NEVER OMITTED from a refusal that has one, because an EMPTY list is
// an answer — no capability would admit this caller, and what is missing is a
// relation the chart does not hold — where an absent key would read as a
// frame that did not say.
type Refused struct {
	Reason string   `json:"reason"`
	Grants []string `json:"grants"`
}

// NewRefused is the refusal a decision made, as the frame carries it.
func NewRefused(reason authz.Reason, grants []iam.Grant) *Refused {
	names := make([]string, 0, len(grants))
	for _, g := range grants {
		names = append(names, string(g))
	}
	return &Refused{Reason: string(reason), Grants: names}
}

// Push builds a broadcast envelope stamped now.
func Push(kind string, data any, now time.Time) Envelope {
	return Envelope{Kind: kind, Data: data, TS: now.UTC().Format(time.RFC3339Nano)}
}

// PushSeat builds an envelope addressed to one seat's watchers.
//
// Its own constructor rather than a field a caller sets on [Push]'s result,
// because the seat is the ADDRESS for a [RouteSeat] kind: a frame of such a
// kind that reached [Hub.Broadcast] without one would be dropped, and a
// constructor that cannot produce one is how that stops being a runtime
// discovery.
func PushSeat(kind, seat string, data any, now time.Time) Envelope {
	env := Push(kind, data, now)
	env.Seat = seat
	return env
}

// Client is one connected dashboard.
type Client struct {
	// out carries encoded frames to the socket's own writer goroutine.
	// Buffered to QueueDepth; a send that would block drops the oldest
	// instead.
	//
	// FRAMES RATHER THAN ENVELOPES, which is the whole of the fan-out
	// rewrite: the bytes are marshalled once per posture by the
	// broadcaster and shared, where every writer used to marshal the same
	// envelope again for itself.
	out chan *Frame

	mu      sync.Mutex
	closed  bool
	dropped int

	// posture is how this client is being served. Set at [NewClient] and
	// moved by [Client.SetPosture]; read under the lock because the hub
	// writes it (a node changing posture) while a broadcast reads it.
	posture FramePosture

	// identityHeld marks a client whose credential this node could not
	// CHECK at its last revalidation — the identity estate was unreadable,
	// or the node was behind it. It degrades THIS client whatever the
	// node's posture: the node may be perfectly healthy while one socket's
	// session is unverifiable, and serving that socket live would push the
	// company's state to somebody who may have been signed out.
	//
	// SEPARATE FROM posture rather than written into it, because the hub
	// owns posture and rewrites it on every health tick — an identity hold
	// stored there would be cleared five seconds later by a node that was
	// never the thing that was wrong.
	identityHeld bool

	// audience is what this client may receive. Set at [NewClient] from
	// the principal the socket was opened as, and moved by
	// [Client.SetAudience] when a revalidation finds the grants changed.
	audience Audience

	// seat is the seat this client asked to be a recipient for, empty
	// while it has asked for none. The hub's index is the authority on
	// which bucket the client is in; this is the same fact kept beside the
	// client so a broadcast can report it and an unregister can find the
	// bucket to leave.
	seat string

	// watches counts every [Hub.Watch] this client has been through, so a
	// decision taken about ONE watch can withdraw that watch and no later
	// one — see [Hub.UnwatchIf].
	watches uint64
}

// NewClient builds a client with an empty queue, served LIVE, that may
// receive what audience allows.
//
// Live rather than the zero posture, which is invalid by construction (see
// [FramePosture]): a client is a tab somebody just opened, and the node it
// reached is serving it unless something says otherwise. [Hub.Register] then
// moves it to the hub's current posture, which is the authority.
//
// THE AUDIENCE IS AN ARGUMENT rather than something set afterwards, so a
// client cannot exist for one instant on the hub's list with an audience
// nobody chose.
func NewClient(audience Audience) *Client {
	return &Client{out: make(chan *Frame, QueueDepth), posture: FrameLive,
		audience: audience}
}

// Receives reports whether this client may read a frame of kind.
func (c *Client) Receives(kind string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.audience.Receives(kind)
}

// Audience is what this client may currently receive.
func (c *Client) Audience() Audience {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.audience
}

// SetAudience moves this client to a, reporting whether that changed what it
// carries.
func (c *Client) SetAudience(a Audience) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := !c.audience.same(a)
	c.audience = a
	return changed
}

// Out is the channel a transport reads frames from. Closed by [Client.Close].
func (c *Client) Out() <-chan *Frame { return c.out }

// Posture is how this client is currently being served: the node's posture,
// degraded further while the client's own identity is held.
func (c *Client) Posture() FramePosture {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.identityHeld {
		return FrameDegraded
	}
	return c.posture
}

// HoldIdentity degrades this client until [Client.ReleaseIdentity], reporting
// whether the hold is new. See the identityHeld field for why it is not a
// posture.
func (c *Client) HoldIdentity() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.identityHeld
	c.identityHeld = true
	return !was
}

// ReleaseIdentity lifts a hold, reporting whether there was one.
func (c *Client) ReleaseIdentity() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.identityHeld
	c.identityHeld = false
	return was
}

// SetPosture moves this client to p, reporting whether it took.
//
// AN INVALID POSTURE IS REFUSED AND THE CURRENT ONE KEPT. The zero value is
// the one a caller reaches by forgetting to decide, and the two ways to serve
// it are opposite — everything, or nothing — so taking it would either leak a
// degraded node's state or blank a healthy dashboard, with nothing on either
// path to say which happened.
func (c *Client) SetPosture(p FramePosture) bool {
	if !p.Valid() {
		log.Warn("stream_invalid_frame_posture", "posture", string(p),
			"hint", "the client keeps the posture it had; a frame posture is "+
				"live or degraded, and the zero value is not a posture")
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.posture = p
	return true
}

// Seat is the seat this client is a recipient for, or empty.
func (c *Client) Seat() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seat
}

// Dropped reports how many frames this client has lost to backpressure.
//
// Reported rather than merely counted: a tab that is behind is a tab showing
// something other than the truth, and the number is the only evidence of it
// that survives the drop.
func (c *Client) Dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// Reply encodes an envelope for THIS client alone and queues it.
//
// The [RouteDirect] path: a snapshot, a pong, a query's result or refusal.
// One client, so one encode, and nothing is shared — which is exactly why it
// is a different method from the broadcast path rather than a one-client call
// into it.
func (c *Client) Reply(env Envelope) {
	if !c.Receives(env.Kind) {
		// A WIRING FAULT, never a decision made here: every path that
		// replies with a company fact builds it for this client's own
		// audience first. Logged loudly because the alternative to
		// dropping it is sending it to somebody who may not read it.
		log.Error("stream_reply_withheld", "kind", env.Kind,
			"hint", "build the frame from the client's own audience; this "+
				"client does not carry the grant the kind needs")
		return
	}
	if !c.Posture().Delivers(env.Kind) {
		return
	}
	frame, err := EncodeFrame(env)
	if err != nil {
		// A frame that cannot be encoded is this server's bug, not the
		// client's. Dropping it keeps the socket alive for every other
		// kind rather than tearing down a working dashboard over one
		// malformed answer.
		log.Error("stream_encode_failed", "kind", env.Kind, "error", err)
		return
	}
	c.send(frame)
}

// send queues a frame, dropping the oldest if the queue is full.
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
func (c *Client) send(frame *Frame) {
	if frame == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	for {
		select {
		case c.out <- frame:
			return
		default:
		}
		// Full. Take one off the front and try again. The take can only
		// fail if the transport's reader emptied the queue in between,
		// and then the next send succeeds — so the loop always makes
		// progress. Looping rather than dropping the NEW frame is what
		// keeps the newest state on screen.
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

// Hub is the set of connected clients, and the index that routes a frame to
// the ones it is for.
type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}

	// bySeat is the routing index: which clients asked to be recipients
	// for which seat. A SECOND MAP rather than a filter over clients,
	// because a per-seat frame on a company with fifty tabs open would
	// otherwise walk fifty clients to find the one watching, on every
	// inbox movement — and the walk's cost is paid by the publish path.
	//
	// A seat with no watchers holds NO entry: an empty bucket left behind
	// by the last watcher leaving is a map that grows with the number of
	// seats anyone has ever looked at.
	bySeat map[string]map[*Client]struct{}

	// posture is the node's current frame posture, applied to every client
	// registered from now on and to every client already registered when
	// it moves. The node is what is degraded, not the tab — but the value
	// is carried per client all the same, because a client registers at an
	// arbitrary moment (including during a change) and a frame has to be
	// decided by one value read once, not by a second read that can move
	// between a fan-out's start and one client's send.
	posture FramePosture
}

// NewHub builds an empty hub, serving LIVE until told otherwise.
func NewHub() *Hub {
	return &Hub{
		clients: map[*Client]struct{}{},
		bySeat:  map[string]map[*Client]struct{}{},
		posture: FrameLive,
	}
}

// Register adds a client to the fan-out, at the hub's current posture.
//
// BEFORE its snapshot is sent, deliberately. The window between registering and
// snapshotting delivers frames describing state the snapshot also carries —
// which is harmless, because the client dedupes streamed envelopes against the
// snapshot by event id. Registering AFTER would instead lose everything
// published in that window, which nothing recovers.
func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	posture := h.posture
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	c.SetPosture(posture)
}

// Unregister removes a client and closes it.
func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	_, present := h.clients[c]
	delete(h.clients, c)
	h.unwatchLocked(c)
	h.mu.Unlock()
	if present {
		if dropped := c.Dropped(); dropped > 0 {
			log.Info("stream_client_left_behind", "dropped", dropped,
				"hint", "the tab could not keep up and lost frames; its "+
					"reconnect refetches a snapshot")
		}
	}
	c.Close()
}

// Watch makes c a recipient for seat, replacing whatever it watched before.
//
// ONE SEAT AT A TIME, because a tab keeps one inbox current — the dashboard
// watches the viewer's own seat for as long as the tab is open — and a client
// that accumulated every seat it was ever pointed at would keep receiving
// frames nobody is reading. An empty seat clears the subscription.
//
// A client that is not registered is not indexed: the index is a subset of the
// fan-out, and a bucket holding a client the hub has already let go of would
// keep it alive and keep sending to its closed queue.
func (h *Hub) Watch(c *Client, seat string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, present := h.clients[c]; !present {
		return
	}
	h.unwatchLocked(c)
	c.mu.Lock()
	c.watches++
	c.mu.Unlock()
	if seat == "" {
		return
	}
	c.mu.Lock()
	c.seat = seat
	c.mu.Unlock()
	watchers, ok := h.bySeat[seat]
	if !ok {
		watchers = map[*Client]struct{}{}
		h.bySeat[seat] = watchers
	}
	watchers[c] = struct{}{}
}

// Watching is the seat c watches and which watch that is, for a decision about
// it taken outside the hub's lock — see [Hub.UnwatchIf].
func (h *Hub) Watching(c *Client) (seat string, watch uint64) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seat, c.watches
}

// UnwatchIf withdraws c's watch only if it is still the watch [Hub.Watching]
// named, reporting whether it withdrew anything.
//
// A COMPARE-AND-CLEAR, because the decision to withdraw is taken on another
// goroutine: a credential re-check reads the watch, asks the authority table
// with no lock held, and by the time a refusal comes back the socket's own read
// loop may have installed an allowed watch for a DIFFERENT seat — which is
// exactly when the old one starts being refused, a rebind or a rename moving
// the viewer's seat. An unconditional clear there withdrew the new, allowed
// watch and told the tab it was refused, and the dashboard retries only a
// refusal that says "unavailable", so that tab heard nothing until its next
// socket.
func (h *Hub) UnwatchIf(c *Client, watch uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.mu.Lock()
	current, seat := c.watches, c.seat
	c.mu.Unlock()
	if current != watch || seat == "" {
		return false
	}
	h.unwatchLocked(c)
	c.mu.Lock()
	c.watches++
	c.mu.Unlock()
	return true
}

// unwatchLocked drops c from whatever seat bucket it is in. Caller holds the
// write lock.
func (h *Hub) unwatchLocked(c *Client) {
	c.mu.Lock()
	seat := c.seat
	c.seat = ""
	c.mu.Unlock()
	if seat == "" {
		return
	}
	watchers := h.bySeat[seat]
	delete(watchers, c)
	if len(watchers) == 0 {
		delete(h.bySeat, seat)
	}
}

// Watchers reports how many clients are recipients for one seat.
func (h *Hub) Watchers(seat string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.bySeat[seat])
}

// SetPosture moves the hub and every client on it to p.
//
// The NODE is what changes posture, so this is where a change lands: one call
// per health tick rather than a read per client per frame, which would put a
// coordination round trip on the publish path.
func (h *Hub) SetPosture(p FramePosture) {
	if !p.Valid() {
		log.Warn("stream_invalid_frame_posture", "posture", string(p),
			"hint", "the hub keeps the posture it had")
		return
	}
	h.mu.Lock()
	h.posture = p
	targets := slices.Collect(maps.Keys(h.clients))
	h.mu.Unlock()

	for _, c := range targets {
		c.SetPosture(p)
	}
}

// Posture is the posture the hub is serving new clients at.
func (h *Hub) Posture() FramePosture {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.posture
}

// Clients reports how many are connected.
func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Broadcast encodes an envelope ONCE PER POSTURE and hands the bytes to every
// client the kind's route names.
//
// # Once per posture, not once per client
//
// A client's posture is the only thing that decides its bytes, so two clients
// in the same posture must receive the byte-identical frame from one encode.
// With N dashboards open this used to be N marshals of identical JSON — on N
// goroutines, for every push — because the envelope travelled to each writer
// and each writer encoded it. The cache below is an ARRAY indexed by
// [FramePosture.index], filled lazily, so a company with every tab in one
// posture pays exactly one encode and the second slot is never touched.
//
// # The route decides the audience
//
// [RouteBroadcast] is every client; [RouteSeat] is the clients watching
// [Envelope.Seat] and nobody else. [RouteDirect] is REFUSED here: a result
// carries the correlation id of one client's question, and fanning it out
// would answer every other tab's question with it. A kind with no route at all
// is refused for the same reason — there is no safe default, and "everyone" is
// the unsafe one.
//
// Never blocks on any client: each client's own queue absorbs the difference,
// and one that has fallen behind loses its oldest frame rather than holding up
// the publish path.
func (h *Hub) Broadcast(env Envelope) {
	route := RouteOf(env.Kind)
	switch {
	case !route.Valid():
		log.Error("stream_unrouted_kind", "kind", env.Kind,
			"hint", "add the kind to internal/api/stream's route table; a push "+
				"with no route is dropped rather than sent to every client")
		return
	case route == RouteDirect:
		log.Error("stream_direct_kind_broadcast", "kind", env.Kind,
			"hint", "an answer belongs to the client that asked; use Client.Reply")
		return
	case route == RouteSeat && env.Seat == "":
		log.Error("stream_seat_frame_without_a_seat", "kind", env.Kind,
			"hint", "build it with PushSeat; a seat-routed frame with no seat "+
				"has no audience and is never fanned out to everyone")
		return
	}

	h.mu.RLock()
	var targets []*Client
	if route == RouteSeat {
		targets = slices.Collect(maps.Keys(h.bySeat[env.Seat]))
	} else {
		targets = slices.Collect(maps.Keys(h.clients))
	}
	h.mu.RUnlock()

	var cache [framePostures]*Frame
	var encoded [framePostures]bool
	for _, c := range targets {
		// WHO MAY READ IT before how it is encoded: a client that does
		// not carry the kind's grant is not in its audience whatever the
		// route says. See [needs].
		if !c.Receives(env.Kind) {
			continue
		}
		posture := c.Posture()
		slot := posture.index()
		if slot < 0 || !posture.Delivers(env.Kind) {
			continue
		}
		if !encoded[slot] {
			// MARKED BEFORE THE RESULT IS STORED, so an envelope that
			// cannot be encoded costs one failed marshal for the whole
			// posture rather than one per client. A nil frame is then
			// the posture's answer and [Client.send] ignores it.
			encoded[slot] = true
			frame, err := EncodeFrame(env)
			if err != nil {
				log.Error("stream_encode_failed", "kind", env.Kind, "error", err)
			} else {
				cache[slot] = frame
			}
		}
		c.send(cache[slot])
	}
}

// Close disconnects every client.
func (h *Hub) Close() {
	h.mu.Lock()
	targets := slices.Collect(maps.Keys(h.clients))
	h.clients = map[*Client]struct{}{}
	h.bySeat = map[string]map[*Client]struct{}{}
	h.mu.Unlock()

	for _, c := range targets {
		c.Close()
	}
}
