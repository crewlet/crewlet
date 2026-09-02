// Package node wires the fleet layer into something runnable: a broker
// client, a coordination backend, and a seat host that owns which seats this
// process runs.
//
// It is deliberately thin. Everything interesting already lives in the
// packages it composes — the queue contract, the lease algebra, the placement
// math — and this is where they meet the one behaviour none of them can
// express alone: WHEN A SEAT IS ACQUIRED, ITS MAILBOX IS ATTACHED, and when
// it is released the mailbox is let go, in an order that never leaves a seat
// consuming work it no longer owns.
//
// That ordering is the whole point, and it is why the fleet suite exercises
// this package rather than the seat host directly.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// TurnFunc runs one seat's turn for a batch of triggering events.
//
// It returns the queue outcome directly, so the decision a turn reaches about
// its delivery — done, failed, or "I have lost this seat" — travels back to
// the broker unchanged rather than being re-derived here.
type TurnFunc func(ctx context.Context, handle string, evs []*events.Event) queue.Result

// Config configures a node.
type Config struct {
	// Queue is this node's broker client. Required.
	Queue queue.EventQueue

	// Coord is the coordination backend. Required.
	Coord coord.Backend

	// NodeID is the stable node identity; Owner is this process
	// incarnation. Both required — see seat.Config for why they differ.
	NodeID string
	Owner  string

	// Seats returns the company's seats, read fresh each sweep.
	Seats func() []placement.Seat

	// Profile is what this node is: its roles and labels.
	Profile placement.NodeProfile

	// Status is what this node is DOING, advertised to peers on the
	// presence heartbeat. Nil publishes none, which reads as "did not
	// say" rather than as an idle node.
	//
	// The beat bounds it: see [seat.Config].Status.
	Status func(context.Context) coord.NodeStatus

	// Turn runs a seat's turn. Required.
	Turn TurnFunc

	// SeatReady runs as this node takes a seat, BEFORE its mailbox is
	// attached, and a failure refuses the seat.
	//
	// Before, because of the ordering OnAcquire exists to enforce: whatever
	// the first turn will need must be ready before the first event can
	// arrive. A seat mid-detached-run is the case that makes it matter —
	// its runs have to be recovered and its mail parked before anything
	// can deliver, or the first message starts a second turn beside a
	// coding job that is still going.
	SeatReady func(ctx context.Context, handle string, lease coord.Lease) error

	// SeatDone runs after the mailbox is detached. It never fails a
	// release: the seat is already gone from this node, and its durable
	// state belongs to the store rather than to this process.
	SeatDone func(ctx context.Context, handle string)

	// BatchOptions tunes inbox coalescing. Nil uses the defaults.
	BatchOptions *queue.BatchOptions

	// MaxConcurrent bounds how many turns this process runs at once
	// (Tier A `node.max_concurrent`). Zero takes [DefaultMaxConcurrent];
	// there is no unbounded — see [gate].
	MaxConcurrent int

	// LeaseTTL, HeartbeatInterval and SweepInterval override the seat
	// host's timings. Zero uses its defaults, which are the measured
	// production values; a test shrinks them so a fleet settles in
	// milliseconds rather than in tens of seconds.
	//
	// The TTL is not free to choose independently of the coordination
	// backend: a store whose expiry is bucket-wide can only honour one,
	// and a backend that silently accepted a different one would be lying
	// about when a lease expires. Configure both from the same value.
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	SweepInterval     time.Duration
}

// Node is one process's participation in a company.
type Node struct {
	cfg  Config
	log  *slog.Logger
	host *seat.Host

	// turns is the per-node concurrency gate every turn passes through.
	turns *gate

	// attached records which seats this node currently consumes, so a
	// release detaches exactly what an acquire attached. Guarded because
	// the seat host calls hooks from its own goroutines.
	mu       sync.Mutex
	attached map[string]struct{}
}

// New builds a node.
func New(cfg Config) (*Node, error) {
	var errs []error
	if cfg.Queue == nil {
		errs = append(errs, errors.New("node: Queue is required"))
	}
	if cfg.Turn == nil {
		errs = append(errs, errors.New("node: Turn is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	n := &Node{
		cfg:      cfg,
		log:      logging.Get("node").With("node_id", cfg.NodeID),
		attached: map[string]struct{}{},
		turns:    newGate(cfg.MaxConcurrent),
	}

	host, err := seat.New(seat.Config{
		Backend:           cfg.Coord,
		Owner:             cfg.Owner,
		NodeID:            cfg.NodeID,
		Seats:             cfg.Seats,
		Profile:           cfg.Profile,
		Status:            cfg.Status,
		Hooks:             n,
		TTL:               cfg.LeaseTTL,
		HeartbeatInterval: cfg.HeartbeatInterval,
		SweepInterval:     cfg.SweepInterval,
	})
	if err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	n.host = host
	return n, nil
}

// Host exposes the seat host, for callers that need to inspect ownership or
// drive a sweep directly.
func (n *Node) Host() *seat.Host { return n.host }

// ID is the STABLE node identity, the same across restarts. It is the
// stickiness hint a duty claim carries, so a restarted node tends to get its
// own duties back.
func (n *Node) ID() string { return n.cfg.NodeID }

// Owner is this process INCARNATION, which is what a lease is held under.
//
// Distinct from ID, and the distinction is load-bearing: two processes sharing
// an owner string would both believe they hold the same duty at the same
// epoch, which is the one way a fleet singleton becomes two.
func (n *Node) Owner() string { return n.cfg.Owner }

// Start begins claiming seats and consuming their mail.
//
// The mailboxes come up BEFORE the claiming, and that ordering is the point —
// see [Node.ensureMailboxes].
func (n *Node) Start(ctx context.Context) error {
	if err := n.cfg.Queue.Start(ctx); err != nil {
		return fmt.Errorf("node: start queue: %w", err)
	}
	n.EnsureMailboxes(ctx)
	n.host.Start(ctx)
	return nil
}

// EnsureMailboxes creates the durable subscription behind every agent seat in
// the company — not just the ones this node claims.
//
// A DURABLE SUBSCRIPTION IS A SEAT'S MAILBOX: it exists without a consumer and
// retains what is published while nothing is attached. Its absence is not an
// error anybody sees, because publishing to a topic no subscription covers
// DROPS THE EVENT SILENTLY. The queue contract documents EnsureSubscription
// for exactly this and nothing in the engine ever called it, so a seat's mail
// existed only from the moment some node happened to attach a consumer to it.
//
// That is a whole class of quiet loss. A company's seats are claimed a few at
// a time across successive sweeps, so every webhook, notification and
// scheduled trigger aimed at a seat this fleet had not reached yet went
// nowhere — during boot, during a rollout, and permanently for any seat no
// live node's placement matches.
//
// EVERY seat, not this node's share: a mailbox is a fact about the company,
// and the node that ends up serving a seat may not be this one. Creating one
// is idempotent, so every node doing it costs a no-op.
//
// Best effort, per seat. A subscription that cannot be created is logged and
// the rest still get theirs — the alternative is a node that refuses to start
// because one topic was unreachable, which loses strictly more mail.
//
// Exported so a config apply can run it again: a revision that ADDS a role
// adds a seat, and that seat's mail is dropped until somebody makes it a
// mailbox.
func (n *Node) EnsureMailboxes(ctx context.Context) {
	created := 0
	for _, seat := range n.cfg.Seats() {
		inbox, group := topics.AgentInbox(seat.Handle), topics.AgentInboxGroup(seat.Handle)
		if inbox == "" || group == "" {
			continue
		}
		made, err := n.cfg.Queue.EnsureSubscription(ctx, inbox, group)
		if errors.Is(err, queue.ErrNotLive) {
			// The queue is not up yet, which is not a fault: the boot
			// apply runs before Start and Start does this again a moment
			// later. Returning rather than continuing, because every
			// remaining seat would report the same thing — one honest
			// line beats seven identical warnings about a state that is
			// about to resolve itself.
			n.log.Debug("seat_mailboxes_deferred",
				"detail", "the broker client is not started yet; the node's "+
					"own start creates these")
			return
		}
		if err != nil {
			n.log.Warn("seat_mailbox_unavailable", "handle", seat.Handle, "error", err,
				"detail", "mail published to this seat before it is claimed is "+
					"dropped rather than retained")
			continue
		}
		if made {
			created++
		}
	}
	n.log.Info("seat_mailboxes_ready", "seats", len(n.cfg.Seats()), "created", created)
}

// Stop gives up every seat and stops consuming.
func (n *Node) Stop(ctx context.Context) {
	// The seat host's own stop releases every held seat, and each release
	// detaches that seat's mailbox through OnRelease — so by the time this
	// returns the node consumes nothing.
	//
	// It does NOT stop the queue. A node is GIVEN its broker client; the
	// caller that opened it decides when it closes, and on the merged
	// topology that client is shared with an API process which outlives
	// the engine's shutdown. Stopping it here took the API's broker down
	// with the engine — and did so through a layer that had no way to know
	// it was not the owner.
	n.host.Stop(ctx)
}

// drainLogInterval is how often a drain says how much work is left.
//
// It is a REPORTING cadence, not a deadline — see Drain for why there is no
// deadline — so it is sized for a human watching a deploy: often enough that
// the console does not look hung, rare enough that a ten-minute turn does not
// print a hundred lines.
const drainLogInterval = 10 * time.Second

// Drain performs this node's graceful departure and returns once its seats
// are handed back.
//
// The three steps are one operation because no layer below can do them alone:
// the seat host knows about leases but not about consumers, and the queue
// knows about consumers but not about who owns a seat.
//
//  1. Stop claiming and give up presence, so peers stop reserving capacity
//     for a node that will never claim again.
//  2. Quiesce every held seat: no NEW work is taken, and the seats keep
//     renewing so the turns already running can finish.
//  3. Once no handler is running, release the seats — mailbox intact, lease
//     given back, so a peer can take over immediately rather than waiting
//     out a TTL.
//
// Step 3 waits INDEFINITELY, bounded only by ctx. That is deliberate: a turn
// parked mid-LLM-round is
// making progress a timer cannot see, and cutting it off buys a faster deploy
// by abandoning work that was nearly done. The hard deadline belongs to
// whatever supervises the process — a container runtime's kill grace, an
// operator's second interrupt — which already has one and can see things this
// process cannot. Callers that want a bound pass a ctx with a deadline.
//
// Drain does not stop the node. A drained node still renews presence-free and
// can be told to claim again (the config-plane shed-then-converge path);
// Stop is what ends it.
func (n *Node) Drain(ctx context.Context) {
	n.host.BeginDrain(ctx)

	// THE GATE FIRST, before the mailboxes quiesce. Quiescing stops NEW
	// deliveries, but a turn already delivered and parked behind a slot is
	// past that point — and it has not called a model or fired a side
	// effect, so it is the one kind of in-flight work that costs nothing
	// to abandon. Without this, a backlog admitted before the quiesce runs
	// full execute → review turns one after another through a
	// shutdown that waits for them indefinitely.
	n.turns.close()

	// Quiescing is what makes the wait terminate on a busy seat. Without
	// it the mailbox keeps feeding this node work for as long as its peers
	// keep publishing, and "wait until nothing is running" never comes
	// true. It is also the reversible verb — a drain that turns out to be
	// a shed can be undone, which PauseDelivery could not offer.
	for _, handle := range n.host.Held() {
		if err := n.OnAdmission(ctx, handle, false); err != nil {
			n.log.Warn("drain_quiesce_failed", "handle", handle, "error", err)
		}
	}

	for {
		remaining, err := n.cfg.Queue.WaitForHandlers(ctx, drainLogInterval)
		if err != nil {
			n.log.Warn("drain_wait_failed", "error", err)
			break
		}
		if remaining == 0 {
			break
		}
		if ctx.Err() != nil {
			n.log.Warn("drain_abandoned", "in_flight", remaining, "error", ctx.Err())
			break
		}
		n.log.Info("drain_in_progress", "in_flight", remaining)
	}

	n.host.ReleaseAll(ctx, seat.ReasonDrain)
	n.log.Info("drain_complete", "still_held", len(n.host.Held()))
}

// --- seat.Hooks -----------------------------------------------------------

// OnAcquire attaches the seat's mailbox — LAST, after everything the first
// turn will need is ready.
//
// The order matters and is the reason this hook exists at all: a seat that
// receives work before its runtime is up runs its first turn against a
// half-built engine. Attaching last means the mailbox is the final thing to
// open, so the first event to arrive meets a seat that can serve it.
func (n *Node) OnAcquire(ctx context.Context, handle string, lease coord.Lease) error {
	inbox, group := topics.AgentInbox(handle), topics.AgentInboxGroup(handle)
	if inbox == "" {
		// An unroutable handle. Refusing here rather than attaching to a
		// subject nobody publishes to is what keeps the failure loud.
		return fmt.Errorf("node: seat %q has no inbox subject", handle)
	}

	if n.cfg.SeatReady != nil {
		if err := n.cfg.SeatReady(ctx, handle, lease); err != nil {
			// The seat is REFUSED rather than attached anyway: a seat
			// whose in-flight runs could not be recovered would take new
			// work beside a coding job nothing is tracking.
			return fmt.Errorf("node: preparing seat %q: %w", handle, err)
		}
	}

	opts := n.cfg.BatchOptions
	if opts == nil {
		opts = queue.DefaultBatchOptions()
	}
	err := n.cfg.Queue.SubscribeBatch(ctx, inbox, group,
		func(ctx context.Context, evs []*events.Event) queue.Result {
			return n.runTurn(ctx, handle, evs)
		},
		conversationKey,
		opts,
	)
	if err != nil {
		return fmt.Errorf("node: attach seat %q: %w", handle, err)
	}

	n.mu.Lock()
	n.attached[handle] = struct{}{}
	n.mu.Unlock()
	n.log.Info("seat_attached", "handle", handle, "epoch", lease.Epoch)
	return nil
}

// runTurn gates every turn on still owning the seat.
//
// The check is FRESHNESS, not membership: it certifies that a successful
// renew is recent enough that this node's ownership still holds. It is an
// optimization — correctness lives in the epoch-fenced writes a turn makes —
// but it is the optimization that stops a node starting minutes of work on a
// seat it has already lost.
//
// Losing the seat DEFERS the delivery rather than failing it: the events are
// healthy, and a successor is already entitled to them.
func (n *Node) runTurn(ctx context.Context, handle string, evs []*events.Event) queue.Result {
	if _, ok := n.host.MayStart(handle); !ok {
		n.host.NoteDeliveryDeferred(handle)
		return queue.Defer("seat is not owned here")
	}
	// THE SLOT AFTER THE OWNERSHIP CHECK and before the turn, which is
	// before its first model round-trip. Ordered this way because a turn
	// on a seat this node has lost must not sit in the queue for a slot
	// it has no business taking — the successor is already entitled to
	// the delivery.
	release, ok := n.turns.acquire(ctx)
	if !ok {
		// DRAINING, or the delivery's own context ended. Deferred, not
		// failed: the events are healthy and nothing has been done with
		// them, so leaving them unacked hands them straight on rather
		// than waiting out a redelivery timer.
		n.host.NoteDeliveryDeferred(handle)
		return queue.Defer("node is draining, so this turn was not started")
	}
	defer release()
	return n.cfg.Turn(ctx, handle, evs)
}

// OnRelease detaches the seat's mailbox.
//
// Reporting an error means the teardown could NOT be proven, and the seat
// host keeps the lease in response: a seat this process may still be
// consuming must not be handed to a peer.
func (n *Node) OnRelease(ctx context.Context, handle string, _ coord.Lease, reason seat.ReleaseReason) error {
	inbox, group := topics.AgentInbox(handle), topics.AgentInboxGroup(handle)
	if inbox == "" {
		return nil
	}
	// A queue that is NOT LIVE has torn the mailbox down already: it
	// consumes nothing, so the detach this hook exists to prove is proven.
	// Reading it as a failure would keep the lease on the one path where
	// this node is trying to hand the seat back — a drain — and strand the
	// seat for a full TTL. Through the contract's sentinel, never a
	// backend's own: nothing above internal/queue may know which is running.
	if _, err := n.cfg.Queue.Detach(ctx, inbox, group); err != nil && !errors.Is(err, queue.ErrNotLive) {
		return fmt.Errorf("node: detach seat %q: %w", handle, err)
	}

	n.mu.Lock()
	delete(n.attached, handle)
	n.mu.Unlock()
	if n.cfg.SeatDone != nil {
		n.cfg.SeatDone(ctx, handle)
	}
	n.log.Info("seat_detached", "handle", handle, "reason", reason.String())
	return nil
}

// OnAdmission quiesces or resumes a seat's mailbox as ownership becomes
// uncertain and then certain again.
//
// This is the half that makes a store blip survivable. A node that cannot
// prove ownership stops taking NEW work but keeps the seat; when a renew
// succeeds again the consumer must be told to resume, or the seat stays
// owned, attached, and permanently deaf.
func (n *Node) OnAdmission(ctx context.Context, handle string, admitted bool) error {
	inbox, group := topics.AgentInbox(handle), topics.AgentInboxGroup(handle)
	if inbox == "" {
		return nil
	}
	var err error
	if admitted {
		_, err = n.cfg.Queue.Unquiesce(ctx, inbox, group)
	} else {
		_, err = n.cfg.Queue.Quiesce(ctx, inbox, group)
	}
	// Same reading as OnRelease: a queue that is not live delivers nothing,
	// so an admission change against one has already taken effect.
	if err != nil && !errors.Is(err, queue.ErrNotLive) {
		return fmt.Errorf("node: set admission for %q: %w", handle, err)
	}
	n.log.Debug("seat_admission", "handle", handle, "admitted", admitted)
	return nil
}

// ResumeClaiming undoes [Node.Drain] for a node that is staying.
//
// The posture path's other half: a node that shed on config divergence and
// then converged must serve again rather than sit out until it restarts. It
// re-opens the concurrency gate, re-admits the seats the drain quiesced and
// tells the host to claim again — in that order, so no delivery arrives at a
// gate that is still shut.
//
// It is not the inverse of [Node.Stop]. Stop releases the seats; this is for
// a node that still holds them.
func (n *Node) ResumeClaiming(ctx context.Context) {
	n.turns.open()
	for _, handle := range n.host.Held() {
		if err := n.OnAdmission(ctx, handle, true); err != nil {
			n.log.Warn("resume_admit_failed", "handle", handle, "error", err)
		}
	}
	n.host.ResumeClaiming(ctx)
}

// InFlightTurns is how many turns hold a concurrency slot right now.
func (n *Node) InFlightTurns() int { return n.turns.inFlight() }

// Attached reports the seats this node currently consumes. Diagnostics and
// the fleet suite's "attached to exactly what I own" assertion read it.
func (n *Node) Attached() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.attached))
	for h := range n.attached {
		out = append(out, h)
	}
	return out
}

// conversationKey partitions a seat's inbox by conversation.
//
// Events that cannot name a conversation key on their own conversation key
// UNIQUELY, on their own id — which means they are never coalesced with
// anything. That is the honest default: merging two unrelated triggers into
// one digest turn loses one of them.
func conversationKey(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	if k, ok := ev.Payload["conversation_key"].(string); ok && k != "" {
		return k
	}
	return "event:" + ev.ID.String()
}
