package seat

import (
	"context"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// --- internal state -------------------------------------------------------

// heldSeat is a seat this node holds. Every field is guarded by Host.mu.
type heldSeat struct {
	lease coord.Lease

	// renewedAt is the time of the last SUCCESSFUL renew.
	//
	// The deadline that makes "keep the seat through a store blip" bounded
	// rather than forever. Without it, a store that stays unreachable
	// leaves this node holding a seat whose row lapsed long ago — and a
	// peer that took it over is then running the same agent concurrently.
	renewedAt time.Time

	// establishing is set while the acquire hook is still running.
	//
	// The seat enters the held set BEFORE the hook, so the heartbeat renews
	// its lease throughout: an acquire spawns MCP children and recovers an
	// interrupted sandbox run, which can outlast a TTL, and a lease that
	// lapsed mid-acquire would let a peer claim a seat this node is halfway
	// through building. But it must admit NOTHING until the hook returns —
	// the hook attaches the consumer LAST precisely because a seat that
	// starts receiving work before its children are up runs its first turn
	// with an empty tool surface.
	establishing bool
}

// undeadSeat is a seat whose teardown could not be proven, and what is being
// done about it.
//
// Held OUT of the held set so nothing new starts on it, renewed so no peer
// can take it, and RETRIED ON EVERY HEARTBEAT — the usual causes (a consumer
// mid-delivery, an MCP child that has not finished dying) are transient, and
// without a retry the very first one stranded the seat for the life of the
// process.
type undeadSeat struct {
	held *heldSeat

	// reason is what the release was FOR. A retry is a continuation of the
	// same release, so the hook must see the same reason — a drain that
	// failed once is still a drain, and telling the hook otherwise would
	// change how it tears the seat down.
	reason ReleaseReason

	// since is when the seat went undead. This is the number to alert on:
	// existence is normal for a second, duration never is.
	since     time.Time
	attempts  int
	lastAlarm time.Time
}

// seatLock is one seat's mutex, plus how many callers are queued on it so a
// prune never drops a lock somebody is about to take.
type seatLock struct {
	mu      sync.Mutex
	waiters int // guarded by Host.mu
}

// --- the host -------------------------------------------------------------

// Config builds a [Host]. Everything unset takes the measured default.
type Config struct {
	// Backend is the lease store. Required.
	Backend coord.Backend

	// Owner is this process INCARNATION — {node_id}:{random}, minted fresh
	// at boot. Required.
	//
	// A live lease is renewable by its own owner string, so two processes
	// sharing an identity would both hold the same seat at the same epoch.
	// The stable node id goes in NodeID, where restart-stability is what
	// you actually want.
	Owner string

	// NodeID is the stable node id: the presence resource, the placement
	// hint, and the identity a peer's placement selector matches on.
	// Required.
	NodeID string

	// Seats returns the company's seats and where each may run. Read FRESH
	// each sweep — the org changes under a live config apply, and a
	// snapshot taken at construction would keep claiming seats that no
	// longer exist. Required.
	//
	// Every seat's placement is needed, not just this node's: the fair
	// share is computed per placement GROUP, so a node has to know how the
	// seats it cannot run are distributed to know how many of the ones it
	// can run are its own.
	Seats func() []placement.Seat

	// Profile is what this node IS — its roles and labels. The zero value
	// is the all-roles, no-labels default, which is the single-process
	// deployment. Its ID is forced to NodeID: the presence row is keyed by
	// the node id, so a profile naming a different node would make this
	// node invisible to itself.
	//
	// Advertised to peers on the presence lease EVERY heartbeat rather than
	// once at claim, so a label or role change lands within one TTL of the
	// restart that made it.
	Profile placement.NodeProfile

	// Status is this node's live state, re-read on every heartbeat and
	// advertised to peers on the presence lease.
	//
	// Nil publishes none, which is a real answer rather than a zero: a
	// node whose engine is not co-located has no in-flight count to
	// report, and a peer reading 0 for it would draw an idle row for a
	// process that is simply not saying.
	//
	// It takes a context because answering may mean reading a store, and
	// this runs on the path that renews presence: the beat bounds it to
	// [StatusBudgetRatio] of an interval and publishes nothing if it
	// overruns, so a slow control plane costs a column rather than the
	// node's seats.
	Status func(context.Context) coord.NodeStatus

	// Hooks is what the engine does with a seat. Nil is legal.
	Hooks Hooks

	TTL               time.Duration
	HeartbeatInterval time.Duration
	SweepInterval     time.Duration
	ClaimLimit        int
	ReleaseLimit      int
	AcquireBackoff    time.Duration

	// Protocol is this build's seat protocol. Zero takes
	// coord.ProtocolVersion, which is the only value a real deployment
	// should ever use; a test pins it to stage a mixed-version fleet.
	Protocol int

	// Clock is this node's clock, for measuring how long ago a renew
	// succeeded. Nil means time.Now. It is NEVER used to decide whether a
	// lease has expired — that is the store's clock, and a node comparing
	// its own wall clock to a deadline hands two nodes the same seat the
	// first time an NTP step separates them.
	Clock func() time.Time
}

// Host claims, holds and releases the seats this node runs.
type Host struct {
	backend  coord.Backend
	owner    string
	nodeID   string
	seats    func() []placement.Seat
	profile  placement.NodeProfile
	status   func(context.Context) coord.NodeStatus
	hooks    Hooks
	clock    func() time.Time
	protocol int

	ttl            time.Duration
	heartbeat      time.Duration
	sweepEvery     time.Duration
	claimLimit     int
	releaseLimit   int
	acquireBackoff time.Duration

	// sweepMu and beatMu serialise the two passes against themselves. A
	// single-threaded scheduler gives this for free; here the loops are
	// goroutines and a caller may drive either pass directly, so two
	// concurrent sweeps could each compute room from a snapshot the other
	// is about to invalidate. Lock order is sweepMu|beatMu -> seat lock ->
	// mu, never the reverse.
	sweepMu sync.Mutex
	beatMu  sync.Mutex

	mu                sync.Mutex
	held              map[string]*heldSeat
	undead            map[string]*undeadSeat
	seatLocks         map[string]*seatLock
	unprovenAdmission map[string]struct{}

	// acquireBackoffs is the deadline before which a seat is not re-claimed
	// HERE. Negative stickiness, the mirror of the positive kind: peers are
	// unaffected and may well succeed where this node did not.
	acquireBackoffs map[string]time.Time

	// liveProfiles is the last successfully-read fleet membership, reused
	// when the store blips. Profiles rather than a count, because placement
	// needs to know which peers are eligible for which seats, not merely
	// how many there are.
	liveProfiles []placement.NodeProfile

	// unmannedRoles are the roles no live node performs, as of the last
	// read — the edge detector behind fleet_role_unmanned.
	unmannedRoles map[placement.NodeRole]struct{}

	nodeLease *coord.Lease
	last      *SweepResult
	running   bool
	draining  bool

	// lastBeat is when the heartbeat goroutine last proved it was turning.
	// It is what the watchdog reads; see [Host.Beat].
	lastBeat time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New builds a host, defaulting every unset knob to its measured constant.
func New(cfg Config) (*Host, error) {
	switch {
	case cfg.Backend == nil:
		return nil, fmt.Errorf("%w: backend is required", ErrInvalidConfig)
	case cfg.Owner == "":
		return nil, fmt.Errorf("%w: owner is required", ErrInvalidConfig)
	case cfg.NodeID == "":
		return nil, fmt.Errorf("%w: node id is required", ErrInvalidConfig)
	case cfg.Seats == nil:
		return nil, fmt.Errorf("%w: seats is required", ErrInvalidConfig)
	}

	profile := cfg.Profile
	profile.ID = cfg.NodeID
	ttl := orDuration(cfg.TTL, SeatLeaseTTL)

	h := &Host{
		backend:      cfg.Backend,
		owner:        cfg.Owner,
		nodeID:       cfg.NodeID,
		seats:        cfg.Seats,
		profile:      profile,
		status:       cfg.Status,
		hooks:        cfg.Hooks,
		clock:        cfg.Clock,
		protocol:     orInt(cfg.Protocol, coord.ProtocolVersion),
		ttl:          ttl,
		heartbeat:    orDuration(cfg.HeartbeatInterval, ttl/HeartbeatRatio),
		sweepEvery:   orDuration(cfg.SweepInterval, SweepInterval),
		claimLimit:   orInt(cfg.ClaimLimit, ClaimLimitPerSweep),
		releaseLimit: orInt(cfg.ReleaseLimit, ReleaseLimitPerSweep),
		// One TTL — THIS host's, not the shipped constant. Tying the two
		// together is the whole justification for the number: a deployment
		// that shortens the lease would otherwise keep a 45-second silence
		// on a seat its peers can retry in ten.
		acquireBackoff:    orDuration(cfg.AcquireBackoff, ttl),
		held:              map[string]*heldSeat{},
		undead:            map[string]*undeadSeat{},
		seatLocks:         map[string]*seatLock{},
		unprovenAdmission: map[string]struct{}{},
		acquireBackoffs:   map[string]time.Time{},
		unmannedRoles:     map[placement.NodeRole]struct{}{},
	}
	if h.clock == nil {
		h.clock = time.Now
	}
	return h, nil
}

func orDuration(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}

func orInt(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func (h *Host) now() time.Time { return h.clock() }

// NodeID is the stable node id this host registers presence under.
func (h *Host) NodeID() string { return h.nodeID }

// Owner is this process incarnation — the owner string every lease carries.
func (h *Host) Owner() string { return h.owner }

// lockSeat takes this seat's lock, returning the release.
//
// ONE LOCK PER SEAT, HELD ACROSS A WHOLE ACQUIRE OR RELEASE. The heartbeat
// and the sweep are independent goroutines with no ordering between them,
// and both hooks are long: an acquire attaches a consumer and spawns MCP
// children, a release tears them down. Without this, a heartbeat that
// detects a lost lease can interleave with a sweep that just re-claimed the
// same seat — and the release then tears down the consumer the claim just
// created, leaving a seat that is owned in the lease table and dead in this
// process, with nothing to notice.
func (h *Host) lockSeat(handle string) func() {
	h.mu.Lock()
	l := h.seatLocks[handle]
	if l == nil {
		l = &seatLock{}
		h.seatLocks[handle] = l
	}
	l.waiters++
	h.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		h.mu.Lock()
		l.waiters--
		h.mu.Unlock()
	}
}

// --- introspection --------------------------------------------------------

// MayStart reports the epoch a new turn may start under.
//
// FRESHNESS, NOT MEMBERSHIP. "Do I hold this seat?" is a question about a
// local snapshot refreshed on a 15 s heartbeat against a 45 s TTL, so the
// honest answer can be a full TTL stale — precisely the window an ownership
// check exists to close, which means a membership check cannot meet its own
// exit criterion.
//
// What IS provable: a successful renew at time t bought exclusivity through
// t+ttl. So admission asks how long ago that renew was and admits only
// inside one heartbeat interval. Every turn that starts is then certified
// owned for at least ttl-heartbeat (30 s at the shipped constants), and the
// first failed renew stops NEW turns immediately while the store-unavailable
// grace still keeps the seat so in-flight work can finish.
//
// This is an OPTIMIZATION, not the safety property. Correctness comes from
// epoch-fenced writes: the returned epoch is the fencing token, and a write
// without it is a write a zombie can also make.
func (h *Host) MayStart(handle string) (int64, bool) {
	h.mu.Lock()
	held := h.held[handle]
	if held == nil || held.establishing {
		h.mu.Unlock()
		return 0, false
	}
	elapsed := h.now().Sub(held.renewedAt)
	epoch := held.lease.Epoch
	h.mu.Unlock()

	if elapsed > h.heartbeat {
		log.Info("seat_admission_stale", "seat", handle,
			"seconds_since_renew", elapsed.Seconds(), "heartbeat_seconds", h.heartbeat.Seconds())
		return 0, false
	}
	return epoch, true
}

// EpochFor is the fencing token for a seat, or false if not held here.
//
// Unlike [Host.MayStart] this does NOT check freshness: it is for
// fencing a write that belongs to work already under way, where refusing
// would abandon a turn mid-flight for no gain — the fence in the write
// itself is what makes the write safe.
func (h *Host) EpochFor(handle string) (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	held := h.held[handle]
	if held == nil {
		return 0, false
	}
	return held.lease.Epoch, true
}

// NoteDeliveryDeferred records that this seat's consumer stopped for want of
// proof.
//
// Called by whoever acted on a MayStart refusal by DEFERRING a delivery —
// which quiesces the attachment (see queue.OutcomeDefer). Without this the
// seat is PERMANENTLY DEAF ON A PERFECTLY HEALTHY NODE, and it takes no
// failure at all to get there: MayStart refuses once the last renew is older
// than one heartbeat interval, and consecutive renews are exactly one
// heartbeat apart PLUS the duration of the pass, so every cycle has a real
// window where a healthy, renewing seat answers false. A batch landing in
// that window defers, the queue quiesces, and nothing ever resumes it: the
// admission signal is edge-triggered, the deferral never entered the set, so
// the next successful renew reports "still admitted", short-circuits, and
// never calls the resume hook.
//
// Deliberately NOT inside MayStart. The in-turn fence calls that too, where
// a refusal abandons a turn and stops no consumer — marking there would
// leave the set claiming a seat is quiesced when it is still happily
// consuming, and the next real store outage would then short-circuit the
// call that stops it. The state means "the consumer is stopped pending
// proof", so only the path that stops one may set it.
//
// No hook fires here: the consumer is already stopping, which is what got us
// called. This records the edge so the RESUME can fire on the next
// successful renew.
func (h *Host) NoteDeliveryDeferred(handle string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, held := h.held[handle]
	_, dead := h.undead[handle]
	if held || dead {
		h.unprovenAdmission[handle] = struct{}{}
	}
}

// Held is the seats whose leases this node holds, sorted.
//
// It EXCLUDES the undead by design — nothing new starts on a seat whose
// teardown was never proven — and INCLUDES a seat whose acquire hook is
// still running, because that lease is genuinely held and genuinely renewed.
// "May a turn start here?" is [Host.MayStart], never this.
func (h *Host) Held() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Sorted(maps.Keys(h.held))
}

// CompanySeats is the seat set this host currently sees, sorted by handle.
//
// Distinct from [Host.Held], and the distinction is the first thing to
// check when a node is claiming nothing: Held answers what this node WON, and
// this answers whether the seat is in the company at all. A node reading a
// stale seat set — bound to the epoch it booted on rather than the one it is
// serving — looks identical to a node losing every race, and only these two
// together tell them apart.
func (h *Host) CompanySeats() []placement.Seat {
	out := slices.Clone(h.seats())
	slices.SortFunc(out, func(a, b placement.Seat) int {
		return strings.Compare(a.Handle, b.Handle)
	})
	return out
}

// Unproven is the seats still leased because their teardown could not be
// proven, sorted.
//
// Operationally the most important number on this object: each one is a seat
// this process may still be consuming, holding a lease no peer can take.
func (h *Host) Unproven() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Sorted(maps.Keys(h.undead))
}

// UnprovenAges is how long each unproven seat has been stranded, for
// /health.
//
// Existence is normal for a moment — a release that fails once and succeeds
// on the next heartbeat is a working system. Duration never is, so this is
// what an alert should read.
func (h *Host) UnprovenAges() map[string]time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	out := make(map[string]time.Duration, len(h.undead))
	for handle, entry := range h.undead {
		out[handle] = now.Sub(entry.since)
	}
	return out
}

// LastSweep is the most recent placement pass, if there has been one.
func (h *Host) LastSweep() (SweepResult, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last == nil {
		return SweepResult{}, false
	}
	return *h.last, true
}

// Beat implements [Pulse]: when the heartbeat goroutine last proved it was
// turning, and whether it is still expected to turn at all.
//
// Both in one call, deliberately. Read separately they race, and a host that
// has STOPPED read as one that is WEDGED is exactly the suicide timer the
// watchdog must never arm.
func (h *Host) Beat() (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastBeat, h.running
}

func (h *Host) stampBeat() {
	h.mu.Lock()
	h.lastBeat = h.now()
	h.mu.Unlock()
}

// --- lifecycle ------------------------------------------------------------

// Start announces this node, takes a first share of the seats, and spins the
// heartbeat and sweep loops.
//
// Presence is announced BEFORE the first sweep: capacity divides by the live
// node count, and a node that has not registered itself would compute a
// share that excludes itself. The first sweep runs synchronously, so boot is
// not eventually-consistent — a node is useful the moment it reports
// started.
//
// It reports no error because none of its work has a failure a caller could
// act on: an unreachable store means this node claims nothing yet and
// retries on its own loops, which is what it would do with any error a
// caller handed back to it.
func (h *Host) Start(ctx context.Context) {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return
	}
	h.running = true
	h.draining = false
	h.lastBeat = h.now()
	loopCtx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	h.mu.Unlock()

	h.renewNodePresence(ctx)
	h.Sweep(ctx)

	h.wg.Go(func() { h.heartbeatLoop(loopCtx) })
	h.wg.Go(func() { h.sweepLoop(loopCtx) })

	log.InfoContext(ctx, "seat_host_started", "node", h.nodeID, "owner", h.owner, "held", len(h.Held()))
}

// BeginDrain stops claiming but keeps renewing what is already held.
//
// The first half of a graceful shutdown: readiness flips off, no new seats
// are taken, and the seats in hand keep their leases alive so their turns
// can finish. [Host.ReleaseAll] is the second half.
//
// Presence is dropped immediately. A draining node that keeps its presence
// lease stays in every peer's divisor, so they compute a share that reserves
// capacity for a node which will never claim again — and this node's seats
// stay dark for the whole drain plus the takeover ramp. With 3 nodes and 10
// seats the two healthy peers each compute ceil(10/3)=4, and two of the
// draining node's seats are claimable by nobody for the entire window.
func (h *Host) BeginDrain(ctx context.Context) {
	h.mu.Lock()
	h.draining = true
	held := len(h.held)
	h.mu.Unlock()
	h.releaseNodePresence(ctx)
	log.InfoContext(ctx, "seat_host_draining", "node", h.nodeID, "held", held)
}

// ResumeClaiming undoes [Host.BeginDrain]: claim again, and count again.
//
// The posture path needs this — a node that shed its seats on config
// divergence and then converged must rejoin the fleet rather than sit out
// until it restarts.
func (h *Host) ResumeClaiming(ctx context.Context) {
	h.mu.Lock()
	if !h.draining {
		h.mu.Unlock()
		return
	}
	h.draining = false
	h.mu.Unlock()
	h.renewNodePresence(ctx)
	log.InfoContext(ctx, "seat_host_resumed", "node", h.nodeID)
}

// Draining reports whether this node has stopped claiming.
func (h *Host) Draining() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.draining
}

// Stop halts the loops, hands every seat back, and gives up presence.
func (h *Host) Stop(ctx context.Context) {
	h.mu.Lock()
	if !h.running {
		h.mu.Unlock()
		return
	}
	h.running = false
	cancel := h.cancel
	h.cancel = nil
	h.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	h.wg.Wait()

	// A BUDGET OF ITS OWN, for the reason Node.Drain gives: Stop is reached
	// on a shutdown path whose context is routinely already cancelled, and
	// a give-back that inherits it releases nothing — every seat then sits
	// dark for a full TTL instead of being taken over at once, and this
	// node's presence lingers so peers keep reserving capacity for it.
	releaseCtx, cancel2 := context.WithTimeout(
		context.WithoutCancel(ctx), SeatLeaseTTL/HeartbeatRatio)
	defer cancel2()
	h.ReleaseAll(releaseCtx, ReasonDrain)
	h.releaseNodePresence(releaseCtx)

	stranded := h.Unproven()
	if len(stranded) > 0 {
		log.ErrorContext(ctx, "seat_host_stopped_with_unproven_seats", "node", h.nodeID, "seats", stranded,
			"hint", "these seats' teardown was never proven, so their leases were held rather "+
				"than released; they lapse at the TTL and a peer picks them up then")
	}

	h.mu.Lock()
	clear(h.undead)
	clear(h.unprovenAdmission)
	clear(h.acquireBackoffs)
	clear(h.seatLocks)
	h.mu.Unlock()
	log.InfoContext(ctx, "seat_host_stopped", "node", h.nodeID)
}

// ReleaseAll hands every seat back — each one the moment IT goes idle.
//
// Concurrently, not one after another, and the difference is the whole point
// of a graceful drain. A voluntary release waits for that seat's in-flight
// turn under a bounded timeout; run in sequence, a node holding a dozen
// seats pays that timeout a dozen times over, and the eleventh seat stays
// dark for the whole procession even though it went idle first. Released
// together, each seat leaves as soon as its own turn finishes and the drain
// costs one timeout rather than N.
//
// Deliberately uncapped, unlike claiming. The claim-rate limit is sized by
// the cost of an MCP SPAWN on the node taking a seat on; letting go costs a
// teardown, and the peers picking these up are throttled by their own claim
// limits anyway — so the only thing a release cap would add is a longer
// drain.
//
// Failures are per seat: a teardown that cannot be proven keeps THAT seat's
// lease and says nothing about the others, so one stuck consumer must not
// strand eleven healthy seats.
func (h *Host) ReleaseAll(ctx context.Context, reason ReleaseReason) {
	handles := h.Held()
	if len(handles) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, handle := range handles {
		wg.Go(func() {
			h.Release(ctx, handle, reason)
		})
	}
	wg.Wait()
}

// Release tears one seat down and gives up its lease, reporting whether the
// lease was PROVEN released.
//
// False means one of three things and the caller must not read it as "the
// seat moved": the seat was not held here, teardown could not be proven (so
// the lease is deliberately kept — see [Host.Unproven]), or the store
// could not be reached to give the row back, in which case the seat is torn
// down locally and the row simply lapses on its own.
func (h *Host) Release(ctx context.Context, handle string, reason ReleaseReason) bool {
	unlock := h.lockSeat(handle)
	defer unlock()
	return h.releaseLocked(ctx, handle, reason)
}

// releaseLocked tears the seat down, then gives up the lease — in that
// order. The caller holds this seat's lock.
//
// FAIL-CLOSED. If the hook cannot prove teardown finished, the lease is NOT
// released: the seat moves to the undead set, where nothing new starts on it
// but the heartbeat keeps renewing it, so no peer can take a seat this
// process may still be consuming. That is the whole point of a lease —
// releasing one while still serving the seat hands a peer permission to run
// the agent concurrently, which is the single failure ownership exists to
// prevent.
func (h *Host) releaseLocked(ctx context.Context, handle string, reason ReleaseReason) bool {
	h.mu.Lock()
	entry := h.held[handle]
	delete(h.held, handle)
	// Whatever happens next, this seat's admission is no longer a live
	// question here: the attachment goes with it, so a stale flag would
	// suppress the resume after a future re-claim.
	delete(h.unprovenAdmission, handle)
	h.mu.Unlock()

	if entry == nil {
		return false
	}
	return h.finishRelease(ctx, handle, entry, reason)
}

// finishRelease runs the teardown and gives up the lease, parking the seat as
// undead when teardown cannot be proven. The caller holds this seat's lock
// and has already taken the seat out of the held set.
//
// Undead is a STATE, not a grave. The heartbeat retries the teardown on
// every tick and releases the lease the moment one succeeds, because the
// usual causes are transient. Without that retry the first failure stranded
// the seat for the life of the process: out of the held set so this node
// never ran it, leased so no peer could, counted against this node's
// capacity forever, and announced exactly once.
func (h *Host) finishRelease(ctx context.Context, handle string, entry *heldSeat, reason ReleaseReason) bool {
	if err := h.notifyRelease(ctx, handle, entry.lease, reason); err != nil {
		now := h.now()
		h.mu.Lock()
		h.undead[handle] = &undeadSeat{held: entry, reason: reason, since: now, attempts: 1, lastAlarm: now}
		h.mu.Unlock()
		log.ErrorContext(ctx, "seat_release_unproven", "seat", handle, "epoch", entry.lease.Epoch,
			"reason", reason.String(), "error", err,
			"hint", "keeping the lease and renewing it: this node may still be consuming the "+
				"seat, and releasing now would let a peer run the same agent concurrently. "+
				"Teardown is retried every heartbeat")
		return false
	}
	released, err := h.backend.Release(ctx, entry.lease.Resource, h.owner, entry.lease.Epoch)
	if err != nil {
		// The seat IS torn down locally; the row simply lapses on its own.
		// Nothing here is worth failing a drain.
		log.WarnContext(ctx, "seat_release_unavailable", "seat", handle, "error", err)
		return false
	}
	return released
}

// retryUndeadTeardown tries again to close a seat whose teardown was never
// proven, reporting whether the seat was finally released.
//
// Called once per heartbeat, which is the whole rate limit it needs: the
// hook is the expensive part, and a seat that could not close 15 s ago is
// not helped by being asked again in 15 ms.
func (h *Host) retryUndeadTeardown(ctx context.Context, handle string) bool {
	unlock := h.lockSeat(handle)
	defer unlock()

	h.mu.Lock()
	entry := h.undead[handle]
	if entry == nil {
		h.mu.Unlock()
		return false
	}
	entry.attempts++
	lease, reason, attempts, since := entry.held.lease, entry.reason, entry.attempts, entry.since
	h.mu.Unlock()

	now := h.now()
	if err := h.notifyRelease(ctx, handle, lease, reason); err != nil {
		h.alarmUndead(handle, err, now)
		return false
	}

	h.mu.Lock()
	delete(h.undead, handle)
	// The seat is genuinely closed now, so a future re-claim must start
	// with a clean admission state — a leftover flag would suppress the
	// resume after the next store blip.
	delete(h.unprovenAdmission, handle)
	h.mu.Unlock()

	log.InfoContext(ctx, "seat_release_recovered", "seat", handle, "epoch", lease.Epoch,
		"attempts", attempts, "stranded_seconds", now.Sub(since).Seconds())

	if _, err := h.backend.Release(ctx, lease.Resource, h.owner, lease.Epoch); err != nil {
		// Torn down locally either way; the row lapses on its own.
		log.WarnContext(ctx, "seat_release_unavailable", "seat", handle, "error", err)
	}
	return true
}

// alarmUndead re-raises a stranded seat's alarm, at most once per interval.
//
// The failure itself is not news — it is the same failure as last heartbeat
// — so logging it every tick would bury the fleet's other signals under one
// seat. What IS news is that it is STILL happening, which is why the elapsed
// time is the payload.
func (h *Host) alarmUndead(handle string, cause error, now time.Time) {
	h.mu.Lock()
	entry := h.undead[handle]
	if entry == nil || now.Sub(entry.lastAlarm) < UndeadAlarmInterval {
		h.mu.Unlock()
		return
	}
	entry.lastAlarm = now
	epoch, reason, attempts, since := entry.held.lease.Epoch, entry.reason, entry.attempts, entry.since
	h.mu.Unlock()

	log.Error("seat_still_unproven", "seat", handle, "epoch", epoch, "reason", reason.String(),
		"attempts", attempts, "stranded_seconds", now.Sub(since).Seconds(), "error", cause,
		"hint", "this seat has been out of service since it failed to tear down: this node "+
			"will not run it and no peer can claim it. Restarting this process frees it — "+
			"weigh that against the healthy seats the restart would also move")
}

// --- hook plumbing --------------------------------------------------------

func (h *Host) notifyAcquire(ctx context.Context, handle string, lease coord.Lease) error {
	if h.hooks == nil {
		return nil
	}
	return callHook("on_acquire", handle, func() error { return h.hooks.OnAcquire(ctx, handle, lease) })
}

// notifyRelease runs the teardown hook, letting the caller see what
// happened.
//
// Errors are NOT swallowed. A release hook that fails leaves the seat's
// consumer and MCP children alive in this process while the lease goes to a
// peer — two live consumers on one inbox, the exact state ownership exists
// to prevent.
func (h *Host) notifyRelease(ctx context.Context, handle string, lease coord.Lease, reason ReleaseReason) error {
	if h.hooks == nil {
		return nil
	}
	return callHook("on_release", handle, func() error { return h.hooks.OnRelease(ctx, handle, lease, reason) })
}

// noteAdmission reports a change in whether this seat's ownership is
// provable.
//
// Edge-triggered: a store that is unreachable for an hour produces one call,
// not one per heartbeat, so the consumer is stopped once and resumed once. A
// hook failure is logged and swallowed — it cannot be allowed to abort the
// heartbeat, which is what keeps every OTHER seat on this node alive.
func (h *Host) noteAdmission(ctx context.Context, handle string, admitted bool) {
	h.mu.Lock()
	_, wasUnproven := h.unprovenAdmission[handle]
	switch {
	case admitted && !wasUnproven, !admitted && wasUnproven:
		h.mu.Unlock()
		return
	case admitted:
		delete(h.unprovenAdmission, handle)
	default:
		h.unprovenAdmission[handle] = struct{}{}
	}
	h.mu.Unlock()

	log.InfoContext(ctx, "seat_admission_changed", "seat", handle, "admitted", admitted)
	if h.hooks == nil {
		return
	}
	err := callHook("on_admission", handle, func() error { return h.hooks.OnAdmission(ctx, handle, admitted) })
	if err != nil {
		log.ErrorContext(ctx, "seat_admission_hook_failed", "seat", handle, "admitted", admitted, "error", err)
	}
}

// callHook runs one engine callback, converting a panic into an error.
//
// Go's default for a panicking callback is to take the process down with it,
// which is strictly worse than what it replaces: hook failures are isolated
// PER SEAT precisely so one broken seat cannot stop the heartbeat that keeps
// every other seat's lease alive. A panic is also no more proof of teardown
// than an error is, so on the release path it lands in exactly the right
// place — the seat goes undead and the teardown is retried.
func callHook(name, handle string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// LOGGED AS WELL AS RETURNED. The error reaches the caller,
			// which is what makes the seat go undead and the teardown
			// retry — but it carries a message, not a stack, and the
			// panic is in somebody else's hook.
			log.Error("seat_hook_panicked", "hook", name, "seat", handle,
				"panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("seat hook %s panicked for %q: %v", name, handle, r)
		}
	}()
	return fn()
}

// safely runs one loop tick, turning a panic into a log line.
//
// A tick that takes the process down with it trades one broken pass for
// every seat on this node, so each tick is isolated from the next.
func (h *Host) safely(event string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Error(event, "node", h.nodeID, "panic", r,
				"stack", string(debug.Stack()))
		}
	}()
	fn()
}
