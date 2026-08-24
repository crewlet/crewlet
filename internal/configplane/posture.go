// Package configplane is the control plane: how a fleet agrees on which
// company configuration it is running, and what a node does when it does not
// have the current one.
//
// Two facts drive everything here. An append-only ACTIVATION POINTER names
// the current epoch, and every node writes a per-node APPLY STATUS saying
// what happened when it tried to reach that epoch. A node reads both and
// decides a posture.
//
// The pointer is append-only rather than a mutable "current revision"
// because re-activating an UNCHANGED revision is the documented
// credential-rotation gesture. A pointer keyed on revision id cannot express
// "the same configuration, resolved again", so it would silently rebuild
// nothing on exactly the operation an operator performs to pick up a rotated
// secret.
package configplane

import (
	"math/rand/v2"
	"time"
)

// Posture is what a node does about the gap between the epoch it has applied
// and the epoch the pointer names.
type Posture string

const (
	// PostureServe — this node has the current epoch. Normal operation.
	PostureServe Posture = "serve"

	// PostureWait — behind, but that is ordinary propagation. Keep
	// serving the previous configuration and keep polling.
	PostureWait Posture = "wait"

	// PostureShed — confirmed behind while healthy peers have the epoch.
	// Give the work up so it reaches a node that can do it.
	PostureShed Posture = "shed"

	// PostureStuck — shedding, and out of retries: this node is the
	// anomaly rather than the revision. Fails readiness.
	PostureStuck Posture = "stuck"

	// PostureIsolated — confirmed behind and NO peer managed the epoch
	// either. The revision is the problem, not this node, so keep serving
	// what rollback preserved and raise an alarm.
	PostureIsolated Posture = "isolated"
)

// ApplyStatus is a node's own outcome for an epoch. Three-valued, and the
// third value is the one that matters.
type ApplyStatus string

const (
	// StatusOK — applied and serving.
	StatusOK ApplyStatus = "ok"

	// StatusError — the apply failed and rolled back cleanly. The node
	// still serves the PRIOR epoch correctly, so it is safe to route to.
	StatusError ApplyStatus = "error"

	// StatusDegraded — the apply failed AFTER a restart-required
	// subsystem had already been mutated, so rollback could not restore
	// it. MCP children cannot be un-respawned. NEVER counts as converged,
	// and never counts as a healthy peer.
	StatusDegraded ApplyStatus = "degraded"
)

// Converged reports whether a status means the node is serving the epoch it
// reported. Degraded deliberately does not, even though the node is running.
func (s ApplyStatus) Converged() bool { return s == StatusOK }

// Timing constants for the reconcile loop. These are measured-by-argument
// rather than arbitrary; see the comments at each.
const (
	// ReconcileInterval is how often a node polls the activation pointer.
	ReconcileInterval = 15 * time.Second

	// ReconcileJitter spreads polls across the fleet so an activation
	// storm cannot become an apply storm. It applies to the INTERVAL
	// only, never to the apply itself.
	ReconcileJitter = 0.2

	// LagGraceTicks is how many reconcile intervals a node may be behind
	// before the lag counts as confirmed rather than as propagation. Three
	// ticks is roughly 45 seconds — long enough that a normal rollout
	// never trips it, short enough that a real divergence is noticed.
	LagGraceTicks = 3

	// MaxApplyAttempts bounds re-applying ONE epoch. Per epoch, not per
	// node lifetime: re-activating a fixed revision resets the budget, so
	// the runbook's fix actually works.
	MaxApplyAttempts = 3

	// PeerStatusFreshness bounds how old a peer's status may be and still
	// count. Four reconcile intervals.
	//
	// Without this bound a scaled-in pod's ghost "ok" row makes a
	// diverged survivor believe a healthy peer exists, so it SHEDS to a
	// node that no longer exists instead of reporting ISOLATED — the
	// company goes dark where it should have gone degraded.
	PeerStatusFreshness = 4 * ReconcileInterval
)

// FleetView is everything a node needs to choose a posture. Deliberately a
// plain value: DecidePosture is pure, so the rule can be exhaustively tested
// without a store, a clock, or a fleet.
type FleetView struct {
	// TargetEpoch is the epoch the activation pointer names.
	TargetEpoch int64
	// AppliedEpoch is the epoch this node is actually serving.
	AppliedEpoch int64
	// SelfStatus is this node's own last reported outcome.
	SelfStatus ApplyStatus
	// TicksBehind is how many reconcile ticks this node has been behind.
	TicksBehind int
	// Attempts is how many times this node has tried the target epoch.
	Attempts int
	// PeersOK counts peers with FRESH status reporting the target epoch
	// applied cleanly.
	PeersOK int
	// PeersReported counts peers with FRESH status of any kind for the
	// target epoch.
	PeersReported int
}

// DecidePosture decides what a node does about its lag.
//
// The rule that matters, and the one that is easy to get backwards: the
// target is the store's pointer, but LAG ALONE IS NEVER A REASON TO SHED.
// Every successful rollout produces lag — the first node to apply advances
// the pointer and every peer is behind until it polls. Shedding on that
// makes the fastest node the cause of a fleet-wide outage, and the faster it
// is, the longer the outage.
//
// So lag must be CONFIRMED before it means anything: either this node tried
// the epoch and failed, or it has been behind longer than propagation could
// explain. Only then does peer health pick the action — and when no peer
// managed the epoch either, the honest conclusion is that the revision is
// bad rather than this node, so it keeps serving what rollback preserved.
func DecidePosture(v FleetView) Posture {
	if v.AppliedEpoch >= v.TargetEpoch {
		return PostureServe
	}

	triedAndFailed := v.SelfStatus == StatusError || v.SelfStatus == StatusDegraded
	if !triedAndFailed && v.TicksBehind < LagGraceTicks {
		// Ordinary propagation. Never shed here.
		return PostureWait
	}

	if v.PeersOK > 0 {
		// Peers have this epoch, so the work CAN go to them. Only here
		// is giving it up an improvement — and exhausted retries mean
		// this node is the anomaly rather than the revision, which is
		// exactly what stuck reports.
		if v.Attempts >= MaxApplyAttempts {
			return PostureStuck
		}
		return PostureShed
	}

	if v.PeersReported > 0 || triedAndFailed {
		// Everyone who tried failed — and on a single-node deployment
		// "everyone" is this node.
		//
		// This deliberately OUTRANKS exhausted attempts. Shedding exists
		// to move work to a healthy peer; with no healthy peer it is not
		// shedding, it is stopping. Every node in a fleet that cannot
		// apply a revision reaches this state at the same moment, so
		// ranking stuck first would take the whole company dark about 45
		// seconds after a bad activation — the exact outcome this branch
		// prevents. A lone node arrives by a shorter path still: with no
		// peer to report anything, its own failure is the only evidence
		// there will ever be.
		//
		// The retry bound is untouched: the attempt counter gates
		// re-applying directly, so a node stops restarting its
		// subsystems after MaxApplyAttempts whatever posture it reports.
		return PostureIsolated
	}

	// Behind longer than propagation explains, but this node has not
	// attempted the epoch and no peer has reported either way. That is
	// silence, not evidence — keep polling.
	return PostureWait
}

// ServesTraffic reports whether a posture should keep answering inbound
// work. Only shed and stuck stop.
func (p Posture) ServesTraffic() bool {
	return p != PostureShed && p != PostureStuck
}

// Ready reports whether a posture should pass a readiness probe.
//
// Wait and isolated deliberately stay ready. Every rollout produces lag, so
// failing readiness on it makes the fastest node cause a fleet outage; and
// an isolated node is serving a rolled-back configuration correctly, which
// is the best available answer when the revision itself is bad.
func (p Posture) Ready() bool {
	return p != PostureShed && p != PostureStuck
}

// ReconcileDelay returns one jittered reconcile interval.
//
// Jitter applies to the interval, never to the apply: an activation storm
// must not become an apply storm, but a node that has decided to converge
// should not then dawdle.
func ReconcileDelay() time.Duration {
	spread := float64(ReconcileInterval) * ReconcileJitter
	delta := (rand.Float64()*2 - 1) * spread
	d := time.Duration(float64(ReconcileInterval) + delta)
	return max(d, time.Second)
}
