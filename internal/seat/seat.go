// Package seat is which seats this node runs, and how it comes to run them.
//
// A seat is an agent's chair in the org: its turns, its per-role MCP
// children and its inbox consumer all belong to whoever holds seat:{id} — the
// lease named by the agent id derived from the seat's handle (ADR-0013), the
// id its mailbox is named by too ([placement.Seat.ID]). The handle is what
// every log line, hook and local map calls the seat. This package is the
// placement half — claiming, holding and letting go. What a node DOES with an
// owned seat is the engine's business, and it arrives here as [Hooks].
//
// # The policy is deliberately dumb
//
// A cleverer one is a standing temptation, so the reasons are worth stating:
//
//   - Greedy claim up to a fair share. Capacity is ceil(seats / live nodes),
//     live nodes being the node:* presence leases of nodes that run seats
//     and have not withdrawn from placement because they cannot serve them.
//     No membership service, no gossip, no coordinator — every node
//     computes the same number from the same table and stops there. Two nodes
//     racing for the last seat is resolved by the lease, not by the
//     arithmetic. With role placement in play the share is computed per
//     placement GROUP, because one fleet-wide ratio strands pinned seats,
//     and it bounds that group alone: a node holds at most its share OF EACH
//     group it matches, claiming and shedding against each separately,
//     because a total it could spend on any of them lets unpinned seats
//     crowd out a pinned one. Seats it cannot give back are charged first,
//     against the least constrained groups, through the same per-group
//     numbers — never a second, pooled bound. See [placement.Compute] and
//     [placement.Plan.Room].
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
//   - A change to the seats themselves sweeps AT ONCE ([Host.Resweep]).
//     The sweep's tick is sized for watching the fleet, and the seat list
//     moves only when a configuration is applied: a role a revision added,
//     and the whole company on a node that booted without one, sat
//     unclaimed for up to [SweepInterval] waiting for a tick that had
//     nothing to learn but what the apply already knew. At most one such
//     pass per interval, so the rate limit above still bounds a node to
//     two passes' claims in any interval however many applies arrive.
//   - And so does the claim gate OPENING ([Config.Ready]), asked for by the
//     gate's owner, since the host cannot see the gate change. A node that
//     booted behind its logs claimed nothing at [Host.Start] and then
//     waited for the tick with every seat it could run unclaimed. An asked
//     pass the gate withheld, giving nothing back either, changed nothing,
//     so it does not count against the one ask an interval honours.
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
// Four more properties, each of which replaced something that looked
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
//   - A seat still being taken is CUT, never waited for under its lock. Its
//     acquire hook has no total and runs under the seat's own lock, so a lease
//     lost and a drain end the hook's context rather than taking that lock,
//     and the seat is never established once cut. Waiting froze the heartbeat
//     until the [Watchdog] ended the process, and held a drain for every
//     acquisition in flight; a drain now waits only for the give-back of what
//     it cut ([Host.AwaitCut]).
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
//
// # The presence status is the node's live core alone
//
// What grows with a company's configuration (its MCP, model and runner rows)
// rides the node's status record, which the engine writes beside the
// presence, so a beat re-sends a few hundred bytes whatever a company
// declares ([Host.RenewPeriod] is the pace that record keeps). A placement
// input never rides the status: a beat that gives up on the hook drops it
// whole, and every peer would read the node as not saying.
//
// # A refusal to start reaches the sandbox's claims through a gate of its own
//
// [Host.MayStart] refuses a turn on a seat still establishing, and that
// refusal carries to every claim of the seat's runs the sandbox coordinator
// makes, an apply's recovery and a deadline's claim included, which do not
// ask MayStart: they pass a gate of their own that keeps a seat's recovery
// exclusive with every claim of its runs (internal/sandbox's recoverygate.go).
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
	// The broker imposes no floor (a closed consumer releases in ~9 ms, a
	// successor attaches in ~5 ms); heartbeat reliability bounds the TTL: 45 s
	// is three intervals, tolerating two consecutive missed renewals with a
	// full interval left. Shorter drops healthy seats on ordinary jitter,
	// each handoff costing an MCP respawn; longer leaves a dead node's seats
	// dark until the TTL runs out.
	SeatLeaseTTL = 45 * time.Second

	// HeartbeatRatio is how many heartbeats fit in a lease — the standard
	// lease ratio, for the standard reason: one third is the largest
	// interval that still leaves room for two failures.
	//
	// A RATIO, because the interval must follow THIS host's TTL: a ten second
	// lease with a fifteen second interval renews every seat after it expired,
	// so the fleet hands seats around forever with every node reading healthy.
	// AcquireBackoff is tied to the same TTL.
	HeartbeatRatio = 3

	// HeartbeatInterval is the interval for the DEFAULT TTL. A host with a
	// configured TTL derives its own.
	HeartbeatInterval = SeatLeaseTTL / HeartbeatRatio

	// StatusBudgetRatio is the share of ONE heartbeat interval the status
	// hook may spend before the beat gives up on it.
	//
	// The hook may read a store (the config posture lives in the control
	// plane) on the path that renews the PRESENCE lease, which a display
	// column must never delay. One fifth (3 s at the shipped TTL) is far more
	// than two indexed reads need and leaves four fifths for the renewal; a
	// hook past it publishes nothing, read as "did not say", not idle.
	//
	// A RATIO for the reason [HeartbeatRatio] is one: a fixed budget would
	// swallow the whole beat of a shortened TTL.
	StatusBudgetRatio = 5

	// SweepInterval is how often placement is re-evaluated.
	//
	// Distinct from the heartbeat because they answer different questions:
	// the heartbeat keeps what this node has, the sweep looks for what it
	// should take. Five seconds means a dead peer's seats are fully
	// absorbed within ~TTL plus a few sweeps, without polling the store
	// hard enough to matter. It is the cadence for what changes in the
	// FLEET; a change to the seats themselves asks for a pass at once
	// ([Host.Resweep]), and so does the claim gate opening ([Config.Ready]).
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
	// The mirror of the claim limit: a release is an MCP teardown and, on the
	// taking node, a spawn. Two rather than four because a release interrupts
	// a live agent at a turn boundary and nothing is dark during a rebalance
	// (the wrong node serves meanwhile), so there is no deadline.
	ReleaseLimitPerSweep = 2

	// AcquireBackoff is how long a seat whose acquire pipeline failed is
	// not re-attempted HERE.
	//
	// A failed acquire (bad MCP command, an unresolved credential) is almost
	// never transient, and retrying on the next sweep spins: claim, spawn,
	// fail, release, with an MCP fork each time. A TTL of backoff gives a
	// peer a clear run at it and cuts the noise from twelve attempts a minute
	// to one. Not permanent: config changes.
	//
	// ONE TTL, not the number 45: a host with a shorter TTL derives its
	// backoff from it. This is the value at the shipped TTL.
	AcquireBackoff = SeatLeaseTTL

	// UndeadAlarmInterval is how often a seat stranded by an unproven
	// teardown re-raises its alarm.
	//
	// The teardown is retried every heartbeat; this is only how long a
	// stranded seat may be quiet, so its evidence outlives log rotation.
	//
	// Five minutes (twenty heartbeats at the shipped TTL), a DURATION rather
	// than a heartbeat count because both reasons are about time: frequent
	// enough to be seen by whoever comes on shift next, rare enough that one
	// stuck seat cannot fill a log (a count of twenty at the 15 s floor
	// would fire every 100 s).
	UndeadAlarmInterval = HeartbeatInterval * 20
)

// ErrInvalidConfig reports a host that cannot be built. Nothing branches on
// it beyond refusing to boot.
var ErrInvalidConfig = errors.New("seat: invalid host configuration")

// unmannedHints say what an operator LOSES when no live node performs a
// role. Every symptom is an absence — nothing fires, nothing is received,
// nothing errors — which is why they need saying out loud.
var unmannedHints = map[placement.NodeRole]string{
	placement.RoleData: "no live node holds the company's durable state, so nothing " +
		"can read or write Worklet, Doclet or Chatlet, and every node " +
		"without the role is waiting on one that does. Give a node the 'data' role.",
	placement.RoleGitlet: "no live node serves git, so nobody can clone, fetch or push the " +
		"company's repositories. Give a node the 'gitlet' role (it needs 'ingress').",
	placement.RoleIngress: "no live node serves the HTTP API, so no webhook from any " +
		"integration reaches this company and the dashboard is down. " +
		"Give a node the 'ingress' role.",
	placement.RoleSeats: "no live node runs agents, so every trigger queues up unread. " +
		"Give a node the 'seats' role.",
	placement.RoleWorkers: "no live node runs the company-wide duties, so nothing fires on " +
		"a schedule, no executor's launch is ever collected, and the retention " +
		"sweeps do not run. Give a node the 'workers' role.",
}

// --- release reasons ------------------------------------------------------

// ReleaseReason is why a seat is being given up. The two families are
// opposites.
//
// VOLUNTARY: the lease is still held. Quiesce, let the in-flight handler
// finish under a bounded wait, then detach and release.
//
// FENCED: the lease is gone or treated as gone, so NO time and no
// exclusivity: in-flight work is abandoned and nothing republished, which
// would give the peer a second copy and reorder the conversation.
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

	// ReasonUnserviceable is this node being unable to serve its seats'
	// work at all — in the engine, a node that can no longer say where
	// the estate its seats read and write is served. See
	// [Config.Serviceable].
	//
	// VOLUNTARY, unlike ReasonPosture, and the difference is what is
	// actually lost. A shedding posture means this node may be running a
	// company revision the fleet has moved off, so what it is doing right
	// now is suspect and must stop at once. This means its reach is gone
	// while its lease is perfectly good — so the turn in flight is better
	// finished (or failed on its own calls) than abandoned, and the seat
	// leaves the moment it goes idle.
	ReasonUnserviceable ReleaseReason = "unserviceable"

	// ReasonUnprepared is a seat this node already holds that could not be
	// made ready for something the company brought in AFTER the seat was
	// taken — the code sandbox's control topic and run recovery, which a
	// seat acquired before the node ran a sandbox never had.
	//
	// VOLUNTARY, for the reason ReasonUnserviceable is: the lease is good
	// and the turn in flight runs against a node that is still correct, so
	// it finishes; the seat goes back so that its next acquisition, here or
	// on a peer, runs the whole preparation that failed — exactly as an
	// acquisition failing the same step is refused.
	ReasonUnprepared ReleaseReason = "unprepared"
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
	// OnAcquire establishes the seat, and ATTACHES ITS CONSUMER LAST (agent,
	// budget cap, MCP children, sandbox-run recovery, THEN the inbox and
	// control subscriptions): earlier, a first turn has no MCP tools.
	//
	// The lease carries the epoch writes are fenced with. An error gives the
	// seat back (ReasonAcquireFailed) and backs this node off for one TTL.
	//
	// ctx is CUT when the lease is lost or a drain begins mid-hook, so a hook
	// with no total should stop on it; the heartbeat never waits for one
	// that does not, a drain only within [Host.AwaitCut]'s bound. After a cut
	// the seat is given back as never established (ReasonAcquireFailed),
	// backed off after a lost lease, not a drain. ctx also ends on return.
	OnAcquire(ctx context.Context, seat placement.Seat, lease coord.Lease) error

	// OnEstablished reports that a seat OnAcquire returned for is
	// ESTABLISHED: from this call on, [Host.MayStart] admits turns on it.
	//
	// The one transition into admission nothing else announces: the acquire
	// hook runs while the seat refuses every turn, and the next renew is a
	// heartbeat away and no admission EDGE (see OnAdmission). This is it.
	//
	// Called once per acquisition, after OnAcquire returned nil, under the
	// seat's lock. It cannot fail the acquisition; a failure is its own to
	// retry.
	OnEstablished(ctx context.Context, seat placement.Seat, lease coord.Lease)

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
	OnRelease(ctx context.Context, seat placement.Seat, lease coord.Lease, reason ReleaseReason) error

	// OnAdmission reports that ownership of a HELD seat became unprovable,
	// or provable again. Only for seats this node keeps — a seat it loses
	// goes through OnRelease instead.
	//
	// The lease grace and the consumer are two clocks. A store blip stops
	// MayStart admitting within one heartbeat while the seat is kept
	// (shedding would tear a healthy company down), refusing every delivery.
	// Without a signal on the way back up, a delivery during the blip leaves
	// the attachment stopped for good.
	//
	// Fires only on the EDGE. Errors are logged and swallowed: this runs
	// inside the heartbeat, which keeps every OTHER seat alive.
	OnAdmission(ctx context.Context, seat placement.Seat, admitted bool) error
}

// HookFuncs adapts plain functions to [Hooks]. A nil field is a hook that
// does nothing, so a caller that only cares about acquire and release writes
// only those two.
type HookFuncs struct {
	Acquire     func(ctx context.Context, seat placement.Seat, lease coord.Lease) error
	Established func(ctx context.Context, seat placement.Seat, lease coord.Lease)
	Release     func(ctx context.Context, seat placement.Seat, lease coord.Lease, reason ReleaseReason) error
	Admission   func(ctx context.Context, seat placement.Seat, admitted bool) error
}

// OnAcquire implements [Hooks].
func (h HookFuncs) OnAcquire(ctx context.Context, seat placement.Seat, lease coord.Lease) error {
	if h.Acquire == nil {
		return nil
	}
	return h.Acquire(ctx, seat, lease)
}

// OnEstablished implements [Hooks].
func (h HookFuncs) OnEstablished(ctx context.Context, seat placement.Seat, lease coord.Lease) {
	if h.Established == nil {
		return
	}
	h.Established(ctx, seat, lease)
}

// OnRelease implements [Hooks].
func (h HookFuncs) OnRelease(ctx context.Context, seat placement.Seat, lease coord.Lease, reason ReleaseReason) error {
	if h.Release == nil {
		return nil
	}
	return h.Release(ctx, seat, lease, reason)
}

// OnAdmission implements [Hooks].
func (h HookFuncs) OnAdmission(ctx context.Context, seat placement.Seat, admitted bool) error {
	if h.Admission == nil {
		return nil
	}
	return h.Admission(ctx, seat, admitted)
}

var _ Hooks = HookFuncs{}

// --- results --------------------------------------------------------------

// SweepResult is what one placement pass did, for logs, tests and /health.
type SweepResult struct {
	// Held is how many seats this node runs after the pass. It excludes
	// the undead by design — nothing new starts on a seat whose teardown
	// was never proven.
	Held int
	// Capacity is this node's fair share: the sum of its shares of the
	// placement groups it is eligible for. Each share bounds only its own
	// group, so this is how many seats the node may hold in all, never how
	// many of any one group. Zero while it is withdrawn.
	Capacity int
	// LiveNodes is how many live nodes are placing seats — running seats,
	// and not withdrawn — the denominator.
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
	// Unplaceable are the seats whose placement matches no live node
	// placing seats, read off the same per-group shares the claims are
	// bounded by. Nothing this node can act on — a pin to a node that is
	// down or withdrawn, a label nobody carries — but it is the one
	// placement failure that is otherwise invisible: the seat is simply
	// not served, and every node in the fleet reports a perfectly healthy
	// sweep.
	Unplaceable []string
	// BlockedByProtocol is the fleet's protocol floor when an
	// older-protocol peer holds a presence or seat lease and this node is
	// therefore refusing to claim; zero when nothing is blocking (no protocol is zero — see
	// coord.ProtocolVersion). Without it, a node stalled by the
	// mixed-version gate is indistinguishable from one whose peers simply
	// hold every seat.
	BlockedByProtocol int
	// Withheld says this node declined to claim because it is not ready to
	// run new seats — see [Config.Ready]. Distinct from every other reason
	// a pass claims nothing, and the one that is otherwise invisible: a
	// node at capacity, a node whose peers hold everything and a node
	// waiting for its own copy to catch up with its logs all report an
	// identical empty sweep.
	Withheld bool
	// FleetFull says this node had room and tried no claim, because the
	// live seat-running nodes' own counts say they hold every seat any of
	// them may run ([Host.fleetHoldsEverySeat]). The ordinary state of a
	// node whose share did not come out even, and distinct from Withheld:
	// this node is ready, and there is simply nothing to take.
	FleetFull bool
}

// Blocked reports whether the mixed-version gate is what stopped this pass
// from claiming.
func (r SweepResult) Blocked() bool { return r.BlockedByProtocol > 0 }

// idle reports whether this pass changed nothing because the claim gate
// withheld it: it claimed nothing and gave nothing back, so it spent none of
// the rate the per-sweep limits bound ([Host.sweepLoop]).
func (r SweepResult) idle() bool {
	return r.Withheld && len(r.Claimed) == 0 && len(r.Lost) == 0
}

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
