// Package seat is which seats this node runs, and how it comes to run them.
//
// A seat is an agent's chair in the org: its turns, its per-role MCP
// children and its inbox consumer all belong to whoever holds
// seat:{handle}. This package is the placement half — claiming, holding and
// letting go. What a node DOES with an owned seat is the engine's business,
// and it arrives here as [Hooks].
//
// # The policy is deliberately dumb
//
// A cleverer one is a standing temptation, so the reasons are worth stating:
//
//   - Greedy claim up to a fair share. Capacity is ceil(seats / live nodes),
//     live nodes being the node:* presence leases of nodes that actually run
//     seats. No membership service, no gossip, no coordinator — every node
//     computes the same number from the same table and stops there. Two nodes
//     racing for the last seat is resolved by the lease, not by the
//     arithmetic. With role placement in play the share is computed per
//     placement GROUP and summed, because one fleet-wide ratio strands pinned
//     seats; see [placement.Compute].
//   - Converge in BOTH directions. Claiming alone only converges for a fleet
//     that SHRINKS: a node that booted alone holds every seat, and a peer
//     joining later computes a share it can never reach because the seats it
//     should take are held by a node with no reason to let go. Scaling out
//     would then do nothing at all until something died.
//   - preferred ORDERS the attempt and never gates it. A seat whose hint
//     names this node is tried first, so a rolling deploy tends to land seats
//     back where their MCP children and caches are already warm. Treating a
//     matching hint as a reason to WAIT would strand every seat a dead node
//     used to hold, because the hint outlives the node that set it.
//   - Claims and give-backs are rate-limited per sweep. The cost of a
//     takeover is not the lease — measured at 4.9 ms to attach a consumer —
//     it is spawning that seat's stdio MCP children. A node absorbing a dead
//     peer's twenty seats at once would fork twenty subprocess trees in one
//     tick.
//
// # Losing a seat is as important as gaining one
//
// And the two failure modes are not the same. Renew reporting false means
// the lease is definitively gone (lapsed, moved, superseded) and the seat
// must be dropped now. An ERROR means the STORE could not be reached, which
// says nothing about ownership — the row is untouched and still held — so
// the node keeps its seats, stops admitting new work, and retries until the
// TTL genuinely runs out. Conflating them tears a healthy node's whole
// company down over a two-second store blip, during which no peer could have
// claimed the seats anyway. Go's (value, error) is that distinction; see the
// coord package doc.
//
// Three more properties, each of which replaced something that looked
// reasonable and was not:
//
//   - Admission is FRESHNESS, not membership. [Host.MayStart] answers
//     "may a new turn start on this seat?" from how long ago the last
//     successful renew was, not from whether the handle is in a local map.
//     That map is refreshed on a heartbeat against a TTL three times longer,
//     so a membership check can be a full TTL stale — exactly the window an
//     ownership check exists to close.
//   - Release has two modes, because loss and drain are opposites. Voluntary
//     still owns the lease and can take its time; fenced has neither time nor
//     exclusivity, so in-flight work is abandoned and nothing is republished.
//   - Release fails CLOSED. A teardown that cannot be PROVEN keeps the lease
//     and keeps renewing it rather than handing the seat on. A lease held a
//     little too long costs latency; a lease released too early costs
//     correctness.
//
// # The node that neither works nor dies
//
// Every failure above assumes a node either works or stops. The one that is
// neither is a process whose seat heartbeat has stalled while the process
// stays alive: its leases lapse and peers take its seats, while its queue
// client — running on goroutines the stall never touched — stays attached and
// holds their mail. Nothing can be signalled out of that, because the code
// that would handle the signal is the code that is stuck. [Watchdog] converts
// it into "gone";
package seat

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

var log = logging.Get("seat.host")

// The measured constants. Their provenance is docs/concepts/scaling.md
// § Where the constants come from, and the harness that re-measures them is
// the queue package's broker-behaviour suite.
const (
	// SeatLeaseTTL is how long a seat stays claimed without a heartbeat.
	//
	// Measured, and the measurement moved where the constraint lives. The
	// broker imposes no floor: a cleanly-closed consumer releases its
	// messages in ~9 ms and a successor attaches in ~5 ms, so a peer is
	// productive the instant it claims. What bounds the TTL is heartbeat
	// reliability — 45 s is three heartbeat intervals, which tolerates two
	// consecutive missed renewals (a GC pause, a store blip, a scheduling
	// hiccup) with a full interval left to recover in.
	//
	// Shorter would drop healthy nodes' seats on ordinary jitter, and each
	// spurious handoff costs a real MCP respawn. Longer is time a dead
	// node's seats sit dark, since nothing can claim them until the TTL
	// runs out.
	SeatLeaseTTL = 45 * time.Second

	// HeartbeatRatio is how many heartbeats fit in a lease — the standard
	// lease ratio, for the standard reason: one third is the largest
	// interval that still leaves room for two failures.
	//
	// A RATIO rather than a duration, because the interval has to follow
	// THIS host's TTL rather than the shipped one. A deployment that
	// shortened its lease to ten seconds while the interval stayed at
	// fifteen would renew every seat strictly after it had already
	// expired: every heartbeat fails, every seat is lost, and the fleet
	// hands its seats around forever with every node reading healthy.
	// AcquireBackoff below is tied to the same TTL for the same reason.
	HeartbeatRatio = 3

	// HeartbeatInterval is the interval for the DEFAULT TTL. A host with a
	// configured TTL derives its own.
	HeartbeatInterval = SeatLeaseTTL / HeartbeatRatio

	// StatusBudgetRatio is the share of ONE heartbeat interval the status
	// hook may spend before the beat gives up on it.
	//
	// The hook is allowed to read a store — this node's config posture
	// lives in the control plane — and it runs on the path that renews
	// this node's PRESENCE lease, which is what keeps it counted in the
	// fleet. A display column must never be able to delay that. One fifth
	// of the interval (3 s at the shipped TTL) is far more than two
	// indexed reads need and still leaves four fifths of the beat for the
	// renewal itself; a hook that overruns publishes nothing, which reads
	// as "did not say" rather than as an idle node.
	//
	// A RATIO for the same reason [HeartbeatRatio] is one: a deployment
	// that shortens its TTL shortens this with it, instead of leaving a
	// fixed budget that swallows the whole beat.
	StatusBudgetRatio = 5

	// SweepInterval is how often placement is re-evaluated.
	//
	// Distinct from the heartbeat because they answer different questions:
	// the heartbeat keeps what this node has, the sweep looks for what it
	// should take. Five seconds means a dead peer's seats are fully
	// absorbed within ~TTL plus a few sweeps, without polling the store
	// hard enough to matter.
	SweepInterval = 5 * time.Second

	// ClaimLimitPerSweep is how many seats one sweep may newly claim.
	//
	// The limiter is MCP spawn, not the lease. Four per five-second sweep
	// absorbs twenty seats in ~25 s — comfortably inside the window where a
	// dead peer's seats were going to be dark anyway — while never forking
	// more than four subprocess trees in one tick.
	ClaimLimitPerSweep = 4

	// ReleaseLimitPerSweep is how many seats one sweep may hand back to
	// rebalance.
	//
	// The mirror of the claim limit, sized the same way and for the same
	// reason: giving a seat up is an MCP teardown and, on the node that
	// takes it, an MCP spawn. Two rather than four because a release is the
	// more disruptive half — it interrupts a live agent at a turn boundary,
	// while a claim only starts one — and because the fleet has no deadline
	// to meet here. Nothing is dark during a rebalance; the seats are
	// served the whole time, just by the wrong node.
	ReleaseLimitPerSweep = 2

	// AcquireBackoff is how long a seat whose acquire pipeline failed is
	// not re-attempted HERE.
	//
	// A failed acquire is almost never transient at the seat level — a bad
	// MCP command, a credential the role's config resolves to nothing, a
	// sandbox template that no longer exists. Retrying it on the next
	// five-second sweep spins: claim, spawn, fail, release, repeat, forever,
	// at the cost of an MCP fork each time, and the seat is dark throughout
	// either way.
	//
	// Backing off for a TTL gives a peer a clear run at it (its own claim
	// order is unaffected) and, if every node fails the same way, reduces
	// the noise from twelve attempts a minute to one. Deliberately not
	// permanent: the cause is often config, and config changes.
	//
	// It is ONE TTL, not the number 45: a host built with a shorter TTL
	// derives its backoff from that TTL rather than from this constant, so
	// the two cannot drift. This is the value at the shipped TTL.
	AcquireBackoff = SeatLeaseTTL

	// UndeadAlarmInterval is how often a seat stranded by an unproven
	// teardown re-raises its alarm.
	//
	// The teardown is retried on every heartbeat, so nothing here changes
	// what the host DOES — this is purely how long a stranded seat is
	// allowed to be quiet. It used to log once, at the moment it happened,
	// and then never again: a seat could be out of service for a week with
	// the only evidence a single line that had long since rotated out.
	//
	// Twenty heartbeats. Frequent enough that the alarm outlives log
	// rotation and that a stranded seat is still visible to whoever comes
	// on shift next; rare enough that one stuck seat cannot fill a log with
	// its own retries.
	UndeadAlarmInterval = HeartbeatInterval * 20
)

// ErrInvalidConfig reports a host that cannot be built. Nothing branches on
// it beyond refusing to boot.
var ErrInvalidConfig = errors.New("seat: invalid host configuration")

// unmannedHints say what an operator LOSES when no live node performs a
// role. Every symptom is an absence — nothing fires, nothing is received,
// nothing errors — which is why they need saying out loud.
var unmannedHints = map[placement.NodeRole]string{
	placement.RoleIngress: "no live node serves the HTTP API, so no webhook from any " +
		"integration reaches this company and the dashboard is down. " +
		"Give a node the 'ingress' role.",
	placement.RoleSeats: "no live node runs agents, so every trigger queues up unread. " +
		"Give a node the 'seats' role.",
	placement.RoleWorkers: "no live node runs the company-wide duties, so nothing fires on " +
		"a schedule, no sandbox run is ever collected, and the retention " +
		"sweeps do not run. Give a node the 'workers' role.",
}

// --- release reasons ------------------------------------------------------

// ReleaseReason is why a seat is being given up. The two families are
// opposites.
//
// VOLUNTARY — this node still holds the lease and is choosing to let go.
// There is time: quiesce, let the in-flight handler finish under a bounded
// wait, then detach and release.
//
// FENCED — the lease is gone or must be treated as gone. There is NO time
// and no exclusivity: a peer may already be running this seat, so in-flight
// work is abandoned rather than finished, and nothing is republished —
// republishing hands the peer a second copy of work it is already doing, and
// sends those messages to the topic tail while the successor replays its
// prefetched siblings from the head, reordering the conversation.
type ReleaseReason string

const (
	// ReasonDrain is a graceful shutdown or a capacity rebalance.
	// Voluntary.
	ReasonDrain ReleaseReason = "drain"

	// ReasonRoleGone is a decommissioned role. Voluntary, but it must not
	// defer at all: the events are for a role that no longer exists.
	ReasonRoleGone ReleaseReason = "role_gone"

	// ReasonPlacement is this node no longer matching the seat's
	// placement — the selector changed under a live apply, or this node's
	// labels did. Voluntary: the lease is still held, so the in-flight turn
	// finishes and an eligible peer picks the seat up. Distinct from
	// ReasonDrain only so the log says which of the two happened; a
	// rebalance and a pin change look identical otherwise, and one of them
	// is an operator action they will want to see land.
	ReasonPlacement ReleaseReason = "placement"

	// ReasonLeaseLost is a renew that reported false, or a TTL grace that
	// expired. Fenced.
	ReasonLeaseLost ReleaseReason = "lease_lost"

	// ReasonAcquireFailed is an acquire pipeline that failed partway.
	// Fenced — the seat was never fully established, so the hook must
	// tolerate partial state.
	ReasonAcquireFailed ReleaseReason = "acquire_failed"

	// ReasonPosture is a config posture that went shed or stuck. Fenced:
	// this node must stop serving the seat now, not when its turn happens
	// to end.
	ReasonPosture ReleaseReason = "posture"
)

// Fenced reports whether this release has lost exclusivity — whether a peer
// may already be running the seat. It is what decides whether in-flight work
// is finished or abandoned.
func (r ReleaseReason) Fenced() bool {
	switch r {
	case ReasonLeaseLost, ReasonAcquireFailed, ReasonPosture:
		return true
	default:
		return false
	}
}

// String renders the reason for logs.
func (r ReleaseReason) String() string { return string(r) }

// --- hooks ----------------------------------------------------------------

// Hooks is what the engine does when a seat arrives, leaves, or has its
// ownership stop being provable. The host owns placement; everything a seat
// IS lives behind these three calls.
//
// A nil Hooks is legal and does nothing, which is the placement-only host a
// test or a diagnostic wants. Implement whichever methods matter and use
// [HookFuncs] for the rest.
type Hooks interface {
	// OnAcquire establishes the seat, and ATTACHES ITS CONSUMER LAST:
	// agent instance, budget cap, per-role MCP children, interrupted
	// sandbox-run recovery, and only THEN the inbox and control
	// subscriptions. A seat that starts receiving work before its MCP
	// children are up runs its first turn with an empty tool surface.
	//
	// The lease carries the epoch every write made on this seat's behalf
	// must be fenced with. Returning an error gives the seat straight back
	// (ReasonAcquireFailed) and backs this node off it for one TTL, because
	// a takeover that failed would otherwise read as owned to the whole
	// fleet while nothing runs it.
	OnAcquire(ctx context.Context, handle string, lease coord.Lease) error

	// OnRelease tears the seat down. The reason decides whether in-flight
	// work is finished or abandoned — see [ReleaseReason].
	//
	// It must be IDEMPOTENT AND TOLERANT OF PARTIAL STATE: a failed acquire
	// releases the same seat, so the hook can be handed one whose MCP
	// children are half-spawned and whose consumer was never attached.
	//
	// ANY error means teardown could not be PROVEN, and the host then keeps
	// the lease and keeps renewing it rather than handing a seat to a peer
	// while still serving it. There is no second error kind for "expected"
	// failure: an unexpected failure is no more proof of teardown than an
	// expected one, so both land in the same place.
	OnRelease(ctx context.Context, handle string, lease coord.Lease, reason ReleaseReason) error

	// OnAdmission reports that ownership of a HELD seat became unprovable,
	// or provable again. Only for seats this node keeps — a seat it loses
	// goes through OnRelease instead.
	//
	// It exists because the lease grace and the consumer are two different
	// clocks. A store blip freezes the last-renew stamp, so MayStart stops
	// admitting turns within one heartbeat while the seat itself is
	// correctly kept (the row is untouched; shedding on a blip would tear a
	// healthy company down). The consumer, meanwhile, is still attached and
	// still being handed deliveries, each of which is refused. Without a
	// signal on the way back up, a delivery that arrived during the blip
	// leaves the attachment stopped for good: the node owns the seat, is
	// attached to it, and reads nothing from it ever again.
	//
	// Fires only on the EDGE, so a store that is down for an hour produces
	// one call, not one per heartbeat. Errors are logged and swallowed —
	// this runs inside the heartbeat, which is what keeps every OTHER seat
	// on this node alive.
	OnAdmission(ctx context.Context, handle string, admitted bool) error
}

// HookFuncs adapts plain functions to [Hooks]. A nil field is a hook that
// does nothing, so a caller that only cares about acquire and release writes
// only those two.
type HookFuncs struct {
	Acquire   func(ctx context.Context, handle string, lease coord.Lease) error
	Release   func(ctx context.Context, handle string, lease coord.Lease, reason ReleaseReason) error
	Admission func(ctx context.Context, handle string, admitted bool) error
}

// OnAcquire implements [Hooks].
func (h HookFuncs) OnAcquire(ctx context.Context, handle string, lease coord.Lease) error {
	if h.Acquire == nil {
		return nil
	}
	return h.Acquire(ctx, handle, lease)
}

// OnRelease implements [Hooks].
func (h HookFuncs) OnRelease(ctx context.Context, handle string, lease coord.Lease, reason ReleaseReason) error {
	if h.Release == nil {
		return nil
	}
	return h.Release(ctx, handle, lease, reason)
}

// OnAdmission implements [Hooks].
func (h HookFuncs) OnAdmission(ctx context.Context, handle string, admitted bool) error {
	if h.Admission == nil {
		return nil
	}
	return h.Admission(ctx, handle, admitted)
}

var _ Hooks = HookFuncs{}

// --- results --------------------------------------------------------------

// SweepResult is what one placement pass did, for logs, tests and /health.
type SweepResult struct {
	// Held is how many seats this node runs after the pass. It excludes
	// the undead by design — nothing new starts on a seat whose teardown
	// was never proven.
	Held int
	// Capacity is this node's fair share, summed over the placement groups
	// it is eligible for.
	Capacity int
	// LiveNodes is how many live nodes run seats at all — the denominator.
	LiveNodes int
	// Claimed are the seats this pass newly established. A seat is counted
	// only once its acquire hook succeeded.
	Claimed []string
	// Lost are the seats this node stopped holding from this pass onward:
	// the ones the pass itself gave up, plus any a heartbeat has lost
	// since. Both writers APPEND — a heartbeat that replaced the list
	// erased whatever the pass had shed, so a node that gave back two
	// seats and then lost a third reported only the third.
	Lost []string
	// Unplaceable are the seats whose placement matches no live
	// seat-running node. Nothing this node can act on — a pin to a node
	// that is down, a label nobody carries — but it is the one placement
	// failure that is otherwise invisible: the seat is simply not served,
	// and every node in the fleet reports a perfectly healthy sweep.
	Unplaceable []string
	// BlockedByProtocol is the fleet's protocol floor when an
	// older-protocol peer holds leases and this node is therefore refusing
	// to claim; zero when nothing is blocking (no protocol is zero — see
	// coord.ProtocolVersion). Without it, a node stalled by the
	// mixed-version gate is indistinguishable from one whose peers simply
	// hold every seat.
	BlockedByProtocol int
	// Withheld says this node declined to claim because it is not ready to
	// run new seats — see [Config.Ready]. Distinct from every other reason
	// a pass claims nothing, and the one that is otherwise invisible: a
	// node at capacity, a node whose peers hold everything and a node
	// waiting on its own projection all report an identical empty sweep.
	Withheld bool
}

// Blocked reports whether the mixed-version gate is what stopped this pass
// from claiming.
func (r SweepResult) Blocked() bool { return r.BlockedByProtocol > 0 }

// clone returns a SweepResult that shares no backing array with the
// original.
//
// It is what makes storing a pass's record safe: the value the caller gets
// back and the value the host keeps must not alias, or a heartbeat appending
// to the host's copy writes into a slice the caller is still reading. That is
// a data race the race detector only finds when a heartbeat and a sweep
// overlap — which needs a store failure to provoke.
func (r SweepResult) clone() SweepResult {
	r.Claimed = slices.Clone(r.Claimed)
	r.Lost = slices.Clone(r.Lost)
	r.Unplaceable = slices.Clone(r.Unplaceable)
	return r
}
