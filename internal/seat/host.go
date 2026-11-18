package seat

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// --- internal state -------------------------------------------------------

// heldSeat is a seat this node holds. Every field is guarded by Host.mu.
type heldSeat struct {
	// seat is what this node is holding: the id every durable name for it
	// is built from, and the handle everything else calls it. CARRIED, so
	// a release can name the lease and detach the mailbox on the paths
	// where the company view is gone — see [placement.Seat.ID].
	seat placement.Seat

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
	// interrupted executor, which can outlast a TTL, and a lease that
	// lapsed mid-acquire would let a peer claim a seat this node is halfway
	// through building. But it must admit NOTHING until the hook returns —
	// the hook attaches the consumer LAST precisely because a seat that
	// starts receiving work before its children are up runs its first turn
	// with an empty tool surface.
	establishing bool

	// cut ends the acquire hook's context while the seat is establishing: set
	// as the seat enters the held set, and cleared once it is established.
	//
	// THE HOOK HAS NO TOTAL, and it runs under the seat's own lock, so nothing
	// that has to end the seat may wait for that lock while it runs: a lease
	// the heartbeat finds lost, a drain giving the node's seats back. They cut
	// the hook instead ([heldSeat.cutLocked]) and never take the lock; the claim
	// that is running the hook gives the seat back once the hook returns
	// ([Host.tryClaim]). A hook that ignores its context is not shortened by a
	// cut, and whatever it returns is discarded: a cut seat is never
	// established. The heartbeat never waits for it; a drain waits for its
	// give-back only as long as the drain's own bound ([Host.AwaitCut]).
	cut context.CancelFunc

	// cutBy is why the seat was cut, and empty while it is not. A cut seat is
	// no longer held to what acts on a seat as held — [Host.Held],
	// [Host.HeldSeats], [Host.Provable] and the heartbeat's renews leave it
	// out — because its lease is lost or is being given back. What counts the
	// LEASES this node has in the store still counts it until its give-back,
	// because the lease is still there: the room a pass claims against
	// ([Host.roomLocked]), the count this node advertises to its peers
	// ([Host.placedCount]), and the epoch a write made on its behalf is fenced
	// with ([Host.EpochFor], [Host.Fence]), which a hook still writing after
	// its cut is held to.
	cutBy cutCause

	// settled is closed once the claim that took the seat has established it
	// or given it back: set as the seat enters the held set, and closed by
	// that claim alone ([Host.tryClaim]).
	//
	// FOR A DRAIN, WHICH CUTS RATHER THAN WAITS, and still has to know when
	// what it cut is gone ([Host.AwaitCut]): a cut seat is out of [Host.Held],
	// so the drain's [Host.ReleaseAll] never reaches it, and its give-back —
	// the seat's teardown and its lease — would otherwise run on beside the
	// subsystems a stop tears down after the drain.
	settled chan struct{}
}

// cutCause is why an establishing seat's acquire hook was cut.
type cutCause string

const (
	// cutDrained is a drain begun while the seat was being taken: the node is
	// leaving, so the seat is given back and the node does not back off it.
	cutDrained cutCause = "drained"

	// cutLeaseLost is a lease the heartbeat found lost — a renew that answered
	// false, or a store unreachable past the TTL — while the seat was being
	// taken. A peer may hold it already, so the node backs off it as it does
	// any lease it lost.
	cutLeaseLost cutCause = "lease_lost"
)

// errSeatCut is the failure a claim records for a seat cut while it was being
// taken whose acquire hook returned nil anyway: the seat is not established
// ([Host.tryClaim]).
var errSeatCut = errors.New("seat: cut while it was being taken")

// Valid reports whether c is a cause this build knows.
func (c cutCause) Valid() bool {
	switch c {
	case cutDrained, cutLeaseLost:
		return true
	default:
		return false
	}
}

// cutLocked cuts an establishing seat for cause, reporting the hook's cancel
// for the caller to call once it has let Host.mu go, or nil when the seat is not
// establishing or was cut already: the first cut's cause stands. The caller
// holds Host.mu and never the seat's lock.
//
// THE CAUSE IS RECORDED WHATEVER THE CANCEL IS, so a cut seat is never
// established ([Host.tryClaim]) even for an entry that carries none.
func (e *heldSeat) cutLocked(cause cutCause) context.CancelFunc {
	if !e.establishing || e.cutBy != "" {
		return nil
	}
	e.cutBy = cause
	return e.cut
}

// isCut reports whether the seat was cut while it was being taken. The caller
// holds Host.mu.
func (e *heldSeat) isCut() bool { return e.cutBy != "" }

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
	// Nil publishes none; a peer reads that as "not saying", not zero (see
	// coord.StatusFromMeta). The beat bounds the hook to [StatusBudgetRatio]
	// of an interval, so a slow control plane costs a column, not the seats.
	//
	// THE ERROR IS THE HOOK'S OWN ANSWER to "did I answer?": the beat
	// publishes nothing when it is not nil, so a hook cut off by its context
	// returns that error. The beat's clock after the fact cannot tell a
	// cut-short hook from one descheduled past the deadline.
	Status func(context.Context) (coord.NodeStatus, error)

	// GitBackground is what this node's git cache gives its background
	// work NOW — its maintenance budget — which the host writes on EVERY
	// beat beside the profile, under [coord.GitBackgroundKey], before the
	// status hook runs and never through it. Nil on a node without the gitlet
	// role, whose row then carries no such key.
	//
	// NOT A STATUS FIELD: a status the beat gave up on is dropped whole,
	// and this is what every Gitlet node places a repository's maintenance
	// by (internal/estate, Router.Maintainer), not a display column. It
	// must not block — read memory, never a store — so the beat writes it
	// unbounded.
	GitBackground func() int64

	// PresenceClaimed is told the epoch of each presence this host claims at
	// an epoch it did not hold the moment before: its first claim, and the
	// claim after a lapse or after a drain it resumed from — never a renewal,
	// which keeps its epoch. Called on the beat that claimed, once the claim
	// is recorded, so [Host.PresenceEpoch] already answers it; it must not
	// block. What a loop that files records under this node's presence epoch
	// (its status record, internal/engine's nodestatusrecord.go) writes on at
	// once, rather than on its own schedule: until it does, every reader
	// counts nothing of this node's. Nil tells nobody.
	PresenceClaimed func(epoch int64)

	// RoleNeeded reports whether the COMPANY needs a role at all right now —
	// false for git where the company hosts no code. Nil: every role is
	// needed (every role but gitlet always is).
	//
	// Asked by the unmanned-role check on every sweep, so it must not block:
	// read the current epoch, never a store. Without it, adding git to the
	// vocabulary would warn fleet_role_unmanned on every fleet that does not
	// host code and declared its roles.
	RoleNeeded func(placement.NodeRole) bool

	// Hooks is what the engine does with a seat. Nil is legal.
	Hooks Hooks

	// Ready reports whether this node may take on NEW seats right now.
	// Nil always admits.
	//
	// Not the Seats list (an empty one DECOMMISSIONS every seat held), nor the
	// acquire hook (refusing there claims then releases). It gates only the
	// claim half: a node not ready keeps, renews and SHEDS what it holds and
	// takes nothing new. It runs on the sweep path, [Host.Start]'s first pass
	// included, and must never wait without a bound.
	//
	// THE HOST CANNOT SEE IT CHANGE: its owner asks for a pass when it opens
	// ([Host.Resweep]); otherwise the next tick, up to [SweepInterval].
	Ready func(ctx context.Context) bool

	// Serviceable reports whether this node may KEEP the seats it holds,
	// and names what stopped it when the answer is no.
	//
	// Unlike Ready (work not yet taken; a BEHIND copy catches up), it fires
	// only where the node cannot serve work in hand at all: in the engine,
	// it can no longer say where its seats' estate is served. A WRONG
	// domain copy is not such a case; calls on it go to another data node.
	//
	// VOLUNTARY, not fenced: the lease is still renewed, the in-flight turn
	// finishes and the seat leaves when idle.
	//
	// Nil keeps every seat. It runs on the sweep path and must not block.
	Serviceable func(ctx context.Context) (bool, string)

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
	// succeeded, on both of its readings ([elapsed]). Nil means time.Now. It
	// is NEVER used to decide whether a lease has expired: that is the
	// store's clock, and a node comparing its own wall clock to a deadline
	// hands two nodes the same seat the first time an NTP step separates
	// them.
	Clock func() time.Time
}

// Host claims, holds and releases the seats this node runs.
type Host struct {
	backend coord.Backend
	owner   string
	nodeID  string
	seats   func() []placement.Seat
	ready   func(ctx context.Context) bool
	// serviceable is Config.Serviceable — whether held seats may stay.
	serviceable func(ctx context.Context) (bool, string)
	profile     placement.NodeProfile
	status      func(context.Context) (coord.NodeStatus, error)
	// gitBackground is Config.GitBackground; nil on a node without the gitlet
	// role.
	gitBackground func() int64
	// presenceClaimed is Config.PresenceClaimed; nil tells nobody.
	presenceClaimed func(epoch int64)
	// roleNeeded is Config.RoleNeeded; nil needs every role.
	roleNeeded func(placement.NodeRole) bool
	hooks      Hooks
	clock      func() time.Time
	protocol   int

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

	// presenceRenewedAt is when the last SUCCESSFUL claim of nodeLease was
	// asked for — what [Host.HoldsPresence] measures, for the reason
	// heldSeat.renewedAt gives: a lease this node can no longer renew is
	// held only until its TTL runs out, whatever this node can see.
	presenceRenewedAt time.Time

	last     *SweepResult
	running  bool
	draining bool

	// drainStop is the allowance of the stop the drain is part of, as
	// [Host.BeginDrain] was handed it, and nil while the node is not draining
	// or its drain carried none.
	//
	// FOR THE GIVE-BACKS THE DRAIN CAUSES AND DOES NOT MAKE ITSELF: a seat
	// the drain cut while it was being taken is given back by the claim that
	// took it, and a lease a claim took as the drain began is handed straight
	// back by that claim ([Host.tryClaim]). Those claims run on the pass's
	// context, which carries no stop, so on their own each would begin an
	// allowance of its own beside the stop's, and a stop against an
	// unreachable store would spend one allowance more for every one of them.
	drainStop *StopBudget

	// withdrawn is whether this node has stepped out of placement because it
	// cannot serve its seats (see [Host.setWithdrawn]), as of the last sweep.
	withdrawn bool

	// placeable are the seats some live node may run, as of the last sweep:
	// the company's seats less the unplaceable ones. The count this node
	// advertises counts only its leases on these ([Host.placedCount]).
	placeable map[string]struct{}

	// resweep is the running sweep loop's ask for a pass before its next
	// tick ([Host.Resweep]), buffered by one so an ask that finds one
	// pending adds nothing to it. MADE BY EACH [Host.Start] and nil while
	// the host is not running, so an ask made then is dropped rather than
	// kept for a loop that does not exist yet.
	resweep chan struct{}

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
		backend:         cfg.Backend,
		owner:           cfg.Owner,
		nodeID:          cfg.NodeID,
		seats:           cfg.Seats,
		ready:           cfg.Ready,
		serviceable:     cfg.Serviceable,
		profile:         profile,
		status:          cfg.Status,
		gitBackground:   cfg.GitBackground,
		presenceClaimed: cfg.PresenceClaimed,
		roleNeeded:      cfg.RoleNeeded,
		hooks:           cfg.Hooks,
		clock:           cfg.Clock,
		protocol:        orInt(cfg.Protocol, coord.ProtocolVersion),
		ttl:             ttl,
		heartbeat:       orDuration(cfg.HeartbeatInterval, ttl/HeartbeatRatio),
		sweepEvery:      orDuration(cfg.SweepInterval, SweepInterval),
		claimLimit:      orInt(cfg.ClaimLimit, ClaimLimitPerSweep),
		releaseLimit:    orInt(cfg.ReleaseLimit, ReleaseLimitPerSweep),
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

// elapsed is how long before now then was, for the age of a renew: the larger
// of the two readings an instant read from the clock carries.
//
// BOTH, BECAUSE EACH IS WRONG ONE WAY. The monotonic reading ignores a wall
// clock stepped back, but it STOPS while the host sleeps (macOS, Linux) and
// the store's TTL does not: alone, a node that slept past its TTL would wake
// still admitting turns on a lease a peer may hold. The wall reading counts
// the sleep, and a step back shortens it. A sleep and a step back each hold
// back one reading only, so the larger is never younger than the renew; a
// step forward costs a refused admission or an early drop, never a seat held
// past its lease.
func elapsed(now, then time.Time) time.Duration {
	return max(now.Sub(then), now.Round(0).Sub(then.Round(0)))
}

// NodeID is the stable node id this host registers presence under.
func (h *Host) NodeID() string { return h.nodeID }

// Owner is this process incarnation — the owner string every lease carries.
func (h *Host) Owner() string { return h.owner }

// lockSeat takes this seat's lock, returning the release.
//
// ONE LOCK PER SEAT, HELD ACROSS A WHOLE ACQUIRE OR RELEASE. The heartbeat
// and the sweep are unordered goroutines and both hooks are long. Without
// this, a heartbeat that detects a lost lease can interleave with a sweep
// that just re-claimed the seat, and the release tears down the consumer the
// claim just created: owned in the lease table, dead in this process.
//
// NOTHING THAT ENDS A SEAT STILL BEING TAKEN WAITS FOR IT. An acquire hook has
// no total, so the heartbeat finding the lease lost and a drain cut that hook
// through its context instead of taking this lock ([heldSeat.cut]), and the
// claim holding the lock gives the seat back.
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

// WithHeldSeat runs fn with the lease this node holds a seat under, under that
// seat's own lock, and reports whether the seat was held.
//
// UNDER THE LOCK every acquisition and release of the seat runs under: a
// caller preparing seats this node ALREADY holds cannot race the seat's own
// acquisition hook or a release tearing down what it attached. A seat claimed
// while the caller waits is prepared by its hook and seen as held once that
// returns; one released meanwhile is not held, and fn does not run.
//
// fn must not call back into the host for the same seat ([Host.Release] takes
// this lock); give a seat back on fn's failure after WithHeldSeat returns.
func (h *Host) WithHeldSeat(handle string, fn func(lease coord.Lease) error) (bool, error) {
	unlock := h.lockSeat(handle)
	defer unlock()
	h.mu.Lock()
	held := h.held[handle]
	var lease coord.Lease
	if held != nil {
		lease = held.lease
	}
	h.mu.Unlock()
	if held == nil {
		return false, nil
	}
	return true, fn(lease)
}

// --- introspection --------------------------------------------------------

// MayStart reports the epoch a new turn may start under.
//
// FRESHNESS, NOT MEMBERSHIP: a local "do I hold it?" snapshot can be a full
// TTL stale. A renew at t bought exclusivity through t+ttl, so admission
// requires that renew inside one heartbeat, certifying a started turn owned
// for at least ttl-heartbeat. The first failed renew stops NEW turns; the
// store-unavailable grace keeps the seat for in-flight work. Renews go out a
// beat inside the window ([Host.renewPeriod]); widening it would admit on an
// older proof. An OPTIMIZATION: correctness comes from epoch-fenced writes.
func (h *Host) MayStart(handle string) (int64, bool) {
	h.mu.Lock()
	held := h.held[handle]
	if held == nil || held.establishing {
		h.mu.Unlock()
		return 0, false
	}
	age := elapsed(h.now(), held.renewedAt)
	epoch := held.lease.Epoch
	h.mu.Unlock()

	if age > h.heartbeat {
		log.Info("seat_admission_stale", "seat", handle,
			"seconds_since_renew", age.Seconds(), "heartbeat_seconds", h.heartbeat.Seconds())
		return 0, false
	}
	return epoch, true
}

// Provable reports whether this node can still prove it holds a seat's lease:
// the seat is held (establishing included) and its last renew was under one
// TTL ago by this node's own clock.
//
// NOT [Host.MayStart]: acting as the seat for one INSTANT (starting a process
// with its credentials) needs only that the lease is live NOW, as it is while
// establishing. A lease past its TTL may be a peer's, so every stamp it
// measures from is read before the store call it records ([Host.Sweep],
// [Host.Heartbeat]). A SEAT CUT WHILE BEING TAKEN is not provable
// ([heldSeat.cut]).
func (h *Host) Provable(handle string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	held := h.held[handle]
	return held != nil && !held.isCut() && elapsed(h.now(), held.renewedAt) < h.ttl
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
// Called by whoever acted on a MayStart refusal by DEFERRING a delivery,
// which quiesces the attachment (see queue.OutcomeDefer). Without this the
// seat is PERMANENTLY DEAF: a store blip or late pass leaves the last renew
// older than the admission window, a batch defers, and nothing resumes the
// queue: the admission signal is edge-triggered and the deferral never
// entered the set, so the next renew reports "still admitted" and never
// calls the resume hook. Not inside MayStart: the in-turn fence calls that
// too, where a refusal stops no consumer. No hook fires here; this records
// the edge so the RESUME fires on the next renew.
func (h *Host) NoteDeliveryDeferred(handle string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, held := h.held[handle]
	_, dead := h.undead[handle]
	if held || dead {
		h.unprovenAdmission[handle] = struct{}{}
	}
}

// TTL is the lease TTL THIS host is running — the EFFECTIVE one, which is
// whatever [New] resolved: a value adopted from a lease bucket a peer created,
// the deployment's own configuration, or [SeatLeaseTTL] when it set none.
//
// THE ADOPTED CASE IS WHAT THIS EXISTS FOR: it is not the locally configured
// value, so timing derived from the local file is wrong on a fleet mismatch.
//
// Exported because every budget that races a lease must derive from the lease
// it races, and those callers live elsewhere: the stop allowance a drain in
// [internal/node] begins through [WithinStop], and the watchdog threshold in
// [internal/engine]. Written against the constant they would follow the
// shipped number, the drift [HeartbeatRatio] exists to name.
func (h *Host) TTL() time.Duration { return h.ttl }

// HeartbeatInterval is the window this host admits a seat within after its last
// renew — the EFFECTIVE interval [New] resolved — and so the longest a held
// seat goes between two renews when nothing is late: the host sends each a beat
// inside it ([Host.renewPeriod]), so that it is answered before the one it
// replaces stops admitting. It is therefore also how long a [Host.MayStart]
// refusal for want of a fresh renew can last on a seat it still holds: the next
// renew is what re-proves it. A caller waiting for MayStart to admit a seat
// again waits this long, rather than re-asking on a clock of its own.
func (h *Host) HeartbeatInterval() time.Duration { return h.heartbeat }

// RenewPeriod is how often the heartbeat loop renews this node's leases, its
// presence among them: a beat inside the heartbeat ([Host.renewPeriod]). What a
// loop that publishes beside the presence keeps pace with, so what it writes is
// as fresh as the presence it is read with.
func (h *Host) RenewPeriod() time.Duration { return h.renewPeriod() }

// Held is the seats whose leases this node holds, sorted.
//
// It EXCLUDES the undead by design — nothing new starts on a seat whose
// teardown was never proven — and INCLUDES a seat whose acquire hook is
// still running, because that lease is genuinely held and genuinely renewed.
// "May a turn start here?" is [Host.MayStart], never this.
//
// UNLESS THAT SEAT WAS CUT ([heldSeat.cut]): a lease lost or a drain begun
// while its hook ran. Its lease is gone or being given back by the claim that
// took it, so nothing may act on it as held — not the memory cycle publishing
// it, not a drain's [Host.ReleaseAll], which would wait on the seat's lock for
// the hook the cut is ending, and not a sandbox apply preparing it.
func (h *Host) Held() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.heldLocked()
}

// heldLocked is [Host.Held] for a caller holding Host.mu.
func (h *Host) heldLocked() []string {
	out := make([]string, 0, len(h.held))
	for _, handle := range slices.Sorted(maps.Keys(h.held)) {
		if !h.held[handle].isCut() {
			out = append(out, handle)
		}
	}
	return out
}

// NeedsRole reports whether the company, as this host reads it, needs a node
// role at all right now ([Config.RoleNeeded]; every role where none was
// given) — the question the unmanned-role check asks before it says a role is
// missing.
//
// EXPORTED as the one place the answer is read, by the check and by whoever
// has to prove the engine's answer reached it: the predicate is handed down
// through the node, and a link dropped on the way warns fleet_role_unmanned
// for git on every fleet that declares its roles and hosts no code — exactly
// what the predicate was added to stop — while every test of the check itself
// still passes.
func (h *Host) NeedsRole(role placement.NodeRole) bool {
	return h.roleNeeded == nil || h.roleNeeded(role)
}

// HeldSeats is [Host.Held] as the seats themselves, in the same order: a seat
// cut while it was being taken is left out of both.
//
// For a caller that has to NAME something durable about a held seat — its
// mailbox, its lease — where the handle is only its label. It reads what the
// host recorded at the claim rather than the company's current seat list, so
// it still answers for a seat the running revision has since removed, which
// is exactly the seat a drain has to quiesce.
func (h *Host) HeldSeats() []placement.Seat {
	h.mu.Lock()
	defer h.mu.Unlock()
	handles := h.heldLocked()
	out := make([]placement.Seat, 0, len(handles))
	for _, handle := range handles {
		out = append(out, h.held[handle].seat)
	}
	return out
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

// UnprovenAges is how long each unproven seat has been stranded, served as
// /health's `unproven_seconds`.
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

// Start spins the heartbeat loop, announces this node, takes a first share of
// the seats, and spins the sweep loop.
//
// THE HEARTBEAT FIRST: the first pass runs acquire hooks inline, which can
// outlast a TTL, so it must renew held seats ([heldSeat]) and the watchdog
// must not read the pass as a wedged beat.
//
// Presence is announced BEFORE the first sweep, or capacity divides by a node
// count excluding this one. A closed claim gate ([Config.Ready]) claims nothing
// until its owner asks for a pass ([Host.Resweep]); a caller needing seats held
// waits. A DRAIN BEGUN BEFORE START HOLDS THROUGH IT: Start leaves the flag
// alone ([Host.Stop] ends it), so that pass claims and announces nothing.
func (h *Host) Start(ctx context.Context) {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return
	}
	h.running = true
	h.lastBeat = h.now()
	loopCtx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	asks := make(chan struct{}, 1)
	h.resweep = asks
	h.mu.Unlock()

	h.wg.Go(func() { h.heartbeatLoop(loopCtx) })
	h.renewNodePresence(ctx)
	h.Sweep(ctx)
	h.wg.Go(func() { h.sweepLoop(loopCtx, asks) })

	log.InfoContext(ctx, "seat_host_started", "node", h.nodeID, "owner", h.owner, "held", len(h.Held()))
}

// BeginDrain stops claiming but keeps renewing what is already held.
//
// The first half of a graceful shutdown: no new seats, held leases stay
// alive so turns finish. [Host.ReleaseAll] is the second.
//
// A SEAT STILL BEING TAKEN IS CUT ([heldSeat.cut]) under the flag's lock, so
// none establishes after the drain: its hook's context ends and the claim
// gives it back ([ReasonAcquireFailed]) without backoff, within
// [Host.drainStop]; [Host.AwaitCut] learns that is done. Waiting would block
// on the seat's lock. Presence is dropped at once, or peers keep reserving
// capacity for a node that never claims again.
func (h *Host) BeginDrain(ctx context.Context) {
	h.mu.Lock()
	h.draining = true
	h.drainStop = stopBudgetOf(ctx)
	var cuts []context.CancelFunc
	for _, entry := range h.held {
		if cut := entry.cutLocked(cutDrained); cut != nil {
			cuts = append(cuts, cut)
		}
	}
	held := len(h.heldLocked())
	h.mu.Unlock()
	for _, cut := range cuts {
		cut()
	}
	h.releaseNodePresence(ctx)
	log.InfoContext(ctx, "seat_host_draining", "node", h.nodeID, "held", held)
}

// AwaitCut waits, for as long as ctx allows, until every claim whose seat was
// cut while it was being taken ([heldSeat.cut]) has given that seat back, and
// reports how many had not when ctx ended.
//
// THE DRAIN'S OTHER HALF OF A CUT. A cut seat leaves [Host.Held] at once, so
// [Host.ReleaseAll] never reaches it; without this a drain would report done
// while a claim was still giving a seat back, beside whatever the stop tears
// down next. A cut hook that reads its context returns promptly, its
// give-back bounded by [Host.drainStop]; one that ignores it is waited for up
// to ctx.
func (h *Host) AwaitCut(ctx context.Context) int {
	h.mu.Lock()
	var settling []chan struct{}
	for _, entry := range h.held {
		if entry.isCut() && entry.settled != nil {
			settling = append(settling, entry.settled)
		}
	}
	h.mu.Unlock()
	for i, settled := range settling {
		select {
		case <-settled:
		case <-ctx.Done():
			left := 0
			for _, rest := range settling[i:] {
				select {
				case <-rest:
				default:
					left++
				}
			}
			return left
		}
	}
	return 0
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
	h.drainStop = nil
	h.mu.Unlock()
	h.renewNodePresence(ctx)
	log.InfoContext(ctx, "seat_host_resumed", "node", h.nodeID)
}

// HoldsPresence reports whether this node holds its presence lease now: it is
// not draining, and its last successful claim of the lease was inside the
// lease's TTL.
//
// THE RENEW'S AGE, NOT THE LEASE'S EXISTENCE. A store that stops answering
// leaves the last lease in hand while the row lapses, and peers then read this
// node as gone (out of their divisor, unasked by the estate's router). A claim
// at t proves the row through t+ttl, so that is how long this answers yes, the
// bound a seat's grace is held to ([heldSeat.renewedAt]). A readiness probe
// reads it: a node its fleet cannot see is not doing its work.
func (h *Host) HoldsPresence() bool {
	_, held := h.PresenceEpoch()
	return held
}

// PresenceEpoch is the epoch of the presence lease this node holds now, and
// false whenever [Host.HoldsPresence] would answer no.
//
// THE INCARNATION A PEER COUNTS THIS NODE BY. A lapsed lease is claimed again
// at a new epoch, so a record filed under the presence epoch (an MCP listing,
// internal/coord's mcplisting.go) stops counting when it lapses, and a node
// seeing its epoch move knows a sweep may have removed what it filed.
func (h *Host) PresenceEpoch() (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.nodeLease == nil || h.draining || elapsed(h.now(), h.presenceRenewedAt) >= h.ttl {
		return 0, false
	}
	return h.nodeLease.Epoch, true
}

// Draining reports whether this node has stopped claiming.
func (h *Host) Draining() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.draining
}

// Stop halts the loops, hands every seat back, and gives up presence.
//
// AND ENDS A DRAIN, a host's that never ran included, so a host stopped can be
// started again undrained:
// [Host.Start] leaves the flag as it finds it, which is what lets a drain
// begun before it hold through its first pass.
func (h *Host) Stop(ctx context.Context) {
	h.mu.Lock()
	if !h.running {
		// A HOST THAT NEVER RAN still ends a drain begun before it, so it
		// too starts undrained.
		h.draining = false
		h.drainStop = nil
		h.mu.Unlock()
		return
	}
	h.running = false
	h.resweep = nil
	cancel := h.cancel
	h.cancel = nil
	h.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	h.wg.Wait()

	// NOT THE CALLER'S CONTEXT (see Node.Drain): a shutdown context is
	// routinely already cancelled, and a give-back inheriting it releases
	// nothing, leaving every seat dark for a TTL and the presence lingering.
	//
	// ON THE STOP'S ONE ALLOWANCE ([WithinStop]): the engine's, or one begun
	// from THIS HOST'S TTL, never [SeatLeaseTTL] ([HeartbeatRatio] says why:
	// a ten second lease would spend fifteen giving seats back). Every
	// give-back below is a [StopStep] of it.
	releaseCtx := WithinStop(context.WithoutCancel(ctx), h.ttl)
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
	h.draining = false
	h.drainStop = nil
	h.mu.Unlock()
	log.InfoContext(ctx, "seat_host_stopped", "node", h.nodeID)
}

// ReleaseAll hands every seat back, all at once.
//
// Concurrently: a teardown takes time (MCP shutdown ladder, memory flush), so
// in sequence the last seat stays dark for the whole procession. Together the
// drain costs the slowest teardown, and the give-backs share a stop's one
// allowance ([StopBudget]).
//
// Uncapped, unlike claiming: that limit is sized by an MCP SPAWN on the taking
// node, peers are throttled by their own, and a cap would only lengthen the
// drain. Failures are per seat: an unproven teardown keeps THAT seat's lease
// and strands no healthy one.
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
	if err := h.notifyRelease(ctx, entry.seat, entry.lease, reason); err != nil {
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
	// THE GIVE-BACK IS ONE STEP OF A STOP, when a stop is what released the
	// seat ([StopStep]); the teardown above is not, because a teardown that
	// cannot be proven keeps the lease, and cutting it short for time would
	// keep it for a TTL.
	stepCtx, done := StopStep(ctx)
	defer done()
	released, err := h.backend.Release(stepCtx, entry.lease.Resource, h.owner, entry.lease.Epoch)
	if err != nil {
		// The seat IS torn down locally; the row simply lapses on its own.
		// Nothing here is worth failing a drain.
		log.WarnContext(ctx, "seat_release_unavailable", "seat", handle, "error", err)
		return false
	}
	if released {
		// THE COUNT THIS NODE ADVERTISES, corrected now rather than at the
		// next heartbeat: a peer with room decides from the fleet's counts
		// whether anything is free ([Host.fleetHoldsEverySeat]), and while
		// this node's row still counted the seat it gave back, every peer
		// read the fleet as full and the seat sat unclaimed. A no-op while
		// draining, which drops presence instead.
		h.renewNodePresence(stepCtx)
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
	seat, lease := entry.held.seat, entry.held.lease
	reason, attempts, since := entry.reason, entry.attempts, entry.since
	h.mu.Unlock()

	now := h.now()
	if err := h.notifyRelease(ctx, seat, lease, reason); err != nil {
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

func (h *Host) notifyAcquire(ctx context.Context, seat placement.Seat, lease coord.Lease) error {
	if h.hooks == nil {
		return nil
	}
	return callHook("on_acquire", seat.Handle, func() error { return h.hooks.OnAcquire(ctx, seat, lease) })
}

// notifyEstablished reports a seat established. A panic in the hook is logged
// and contained, like every hook's: the seat is already serving, and the
// sweep that established it is what keeps every other seat on this node.
func (h *Host) notifyEstablished(ctx context.Context, seat placement.Seat, lease coord.Lease) {
	if h.hooks == nil {
		return
	}
	if err := callHook("on_established", seat.Handle, func() error {
		h.hooks.OnEstablished(ctx, seat, lease)
		return nil
	}); err != nil {
		log.ErrorContext(ctx, "seat_established_hook_failed", "seat", seat.Handle, "error", err)
	}
}

// notifyRelease runs the teardown hook, letting the caller see what
// happened.
//
// Errors are NOT swallowed. A release hook that fails leaves the seat's
// consumer and MCP children alive in this process while the lease goes to a
// peer — two live consumers on one inbox, the exact state ownership exists
// to prevent.
func (h *Host) notifyRelease(ctx context.Context, seat placement.Seat, lease coord.Lease, reason ReleaseReason) error {
	if h.hooks == nil {
		return nil
	}
	return callHook("on_release", seat.Handle, func() error { return h.hooks.OnRelease(ctx, seat, lease, reason) })
}

// noteAdmission reports a change in whether this seat's ownership is
// provable.
//
// Edge-triggered: a store that is unreachable for an hour produces one call,
// not one per heartbeat, so the consumer is stopped once and resumed once. A
// hook failure is logged and swallowed — it cannot be allowed to abort the
// heartbeat, which is what keeps every OTHER seat on this node alive.
func (h *Host) noteAdmission(ctx context.Context, seat placement.Seat, admitted bool) {
	handle := seat.Handle
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
	err := callHook("on_admission", handle, func() error { return h.hooks.OnAdmission(ctx, seat, admitted) })
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
			contained := panics.Recovered(panics.ScopeSeat, r)
			// LOGGED AS WELL AS RETURNED. The error reaches the caller,
			// which is what makes the seat go undead and the teardown
			// retry — but it carries a message, not a stack, and the
			// panic is in somebody else's hook.
			log.Error("seat_hook_panicked", "hook", name, "seat", handle,
				"panic", r, "stack", contained.Stack)
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
			contained := panics.Recovered(panics.ScopeSeat, r)
			log.Error(event, "node", h.nodeID, "panic", r,
				"stack", contained.Stack)
		}
	}()
	fn()
}
