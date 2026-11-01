// Package configplane is the control plane: how a fleet agrees on which
// company configuration it is running, and what a node does when it does not
// have the current one.
//
// Two facts drive everything here, and their split is ADR-0009: WHICH
// revision is current is fleet-wide, and APPLYING it is a per-node act. An
// append-only ACTIVATION POINTER names the current epoch, and every node
// writes a per-node APPLY STATUS saying what happened when it tried to reach
// that epoch. A node reads both and decides a posture.
//
// The pointer is append-only rather than a mutable "current revision"
// because re-activating an UNCHANGED revision is the documented
// credential-rotation gesture. A pointer keyed on revision id cannot express
// "the same configuration, resolved again", so it would silently rebuild
// nothing on exactly the operation an operator performs to pick up a rotated
// secret.
//
// # Three facts about a company's configuration
//
// What a node knows about its company is three facts, and each has one
// writer. ADR-0057 is the record; this is the authority.
//
//   - The FLEET's revision is the pointer. It is the company: what every
//     config read answers ("GET /config" and its ETag, the revision list's
//     `is_active`, every write's base) whenever the pointer can be read, on
//     every node alike, whatever that node runs. A config write is built on
//     it and lands only while the pointer still names it
//     ([coord.ActivationRequest.Expect]); a write that read no pointer lands
//     only while there is none ([coord.ActivationRequest.ExpectAbsent]).
//     Only `crewlet run -import-company` replaces it unconditionally, as a
//     choice made at that node's clock. Each activation says whether it
//     RESTORES a company the fleet lost or CHOOSES one
//     ([coord.ActivationRequest.Restore]): a peer that remembers the fleet
//     moving past a restored revision replaces it, and never a choice, since
//     instants come from writers' clocks and a choice whose writer's clock ran
//     behind reads older than it is.
//   - The BOOT row is what this node starts on ([store.Configs.Boot]).
//     Online, only the reconciler moves it, and only to the revision the
//     engine has just APPLIED ([store.Configs.Serve]), so a node never boots
//     on a revision it refused and never moves it out of pointer order.
//     It keeps the instant of the activation it applied
//     ([store.Revision.ServedAt]), which the chart is stamped with at the next
//     boot, apart from the fleet's latest activation of the same revision.
//     Offline, a command chooses it as an OFFER built on what the node knew
//     the company to be ([store.Configs.Offer]) — and on an offer an earlier
//     start sent and never confirmed, which the fleet may run
//     ([store.Offer.Unconfirmed]) — and the next boot publishes the offer only
//     over one of those, pinned to the very activation it read. No config
//     answer is read from it. What each node boots on IS reported per node in
//     the fleet view ([coord.NodeApply.Boots]), as a fact of its own rather
//     than as the revision the node serves: moving this row is a local,
//     best-effort write, so until a tick repairs one that failed — and every
//     tick tries, converged or out of retries — the node runs a revision it
//     would not restart on, and whoever keeps what that node needs in order
//     to start has to read this one.
//   - The FLEET'S ROW is the newest pointer revision this node's reconciler
//     opened, runnable here or not ([store.Configs.SawFleet]). It is the
//     company as this node knows it when the pointer is gone
//     ([store.Configs.Restorable]): a node that refused R and runs P restores
//     R, because every node that could run R does, and P would roll them back.
//     Every row keeps the fleet's latest activation of its revision
//     ([store.Revision.ActivatedAt]), the boot row included: a restore asks
//     for it, and the retention sweep dates the row by it.
//
// They used to be one flag, and each reader took it for the fact it needed:
// a peer that fetched a revision marked it as what it boots on before
// validating it, and was refused at its next start; a write marked itself
// local as the pointer moved, and a reconciler a moment behind walked it back;
// an offline command marked a revision "now", and the next boot published it
// over a fleet that had moved on.
//
// No reader writes. A GET answers the pointer's revision from this node's
// history or, where it holds none yet, from the body the pointer carries
// ([RevisionOf]), and the reconciler's next tick keeps the copy.
package configplane

import (
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
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

	// LagGrace is how long a node may be behind before the lag counts as
	// confirmed rather than as propagation. Three reconcile intervals,
	// 45 seconds — long enough that a poll plus a normal apply never trips
	// it, short enough that a real divergence is noticed.
	//
	// A DURATION RATHER THAN A COUNT OF TICKS, because the case it exists
	// for is the one in which ticks stop: an apply that hangs holds the
	// tick that started it, so a counter of ticks never reaches its
	// threshold, and a node wedged on its first attempt would read as
	// ordinary propagation for as long as the wedge lasted.
	LagGrace = 3 * ReconcileInterval

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
	// SelfStatus is this node's own outcome for the target epoch: the
	// outcome of an attempt that has CONCLUDED, and StatusOK while none
	// has failed.
	//
	// An attempt still in flight is not an outcome. Read as one, every
	// successful apply spent its whole duration as a failure — a lone node
	// reported isolated, a node beside current peers reported shed, and a
	// client polling /health could not tell that from a revision that
	// genuinely does not apply. A node mid-apply is doing exactly what
	// propagation means, and what it reports is wait.
	SelfStatus ApplyStatus
	// BehindFor is how long this node has been behind on THIS target:
	// since it first saw the target epoch while serving an older one.
	// Zero while it is not behind.
	//
	// Per epoch, like Attempts. A lag already waited out on an earlier
	// target says nothing about this one, and counted against it, every
	// node still applying a corrected revision read as confirmed lag the
	// moment a faster peer had applied it. The one exception is a node
	// that has not yet been able to look at the target because an attempt
	// at an older one is still running: it is behind by as long as that
	// attempt has held it, which is what keeps a hung apply confirmed lag
	// across every activation made while it hangs.
	BehindFor time.Duration
	// Attempts is how many attempts at the target epoch have concluded in
	// failure. Counted when an attempt ENDS, never when it starts, for the
	// reason SelfStatus gives: the third attempt in flight is not three
	// failures, and counting it as one reported stuck — out of rotation —
	// for the whole duration of an apply that then succeeded. None at a
	// target this node has not attempted yet: an earlier target's
	// exhausted budget is not a failure at this one.
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
// So lag must be CONFIRMED before it means anything: either an attempt of this
// node's at the epoch concluded in failure, or it has been behind on that
// epoch longer than propagation could explain. An attempt still running is
// neither. Only then does peer health pick the action — and when no peer
// managed the epoch either, the honest conclusion is that the revision is
// bad rather than this node, so it keeps serving what rollback preserved.
func DecidePosture(v FleetView) Posture {
	if v.AppliedEpoch >= v.TargetEpoch {
		return PostureServe
	}

	if !v.LagConfirmed() {
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

	if v.PeersReported > 0 || v.triedAndFailed() {
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

// LagConfirmed reports whether this node's lag on the target epoch is
// CONFIRMED rather than ordinary propagation: an attempt of its own at the
// epoch concluded in failure, or it has been behind on it for [LagGrace] or
// longer. An attempt still running is neither, and a node that is not behind
// has no lag to confirm.
//
// THE RULE [DecidePosture] JUDGES BY, stated once, because a second reader
// needs the same line drawn: the engine's irreversible judgements against the
// active revision decline on a node that has not applied the pointer's epoch,
// and say so at WARN only where that is a fault rather than the seconds every
// activation and every boot spends applying it. A copy of the rule there is
// the one that would drift — and a node the posture reads as waiting while its
// purge duty reports it behind is two answers to one question.
func (v FleetView) LagConfirmed() bool {
	return v.AppliedEpoch < v.TargetEpoch && (v.triedAndFailed() || v.BehindFor >= LagGrace)
}

// triedAndFailed is whether this node's own attempt at the target concluded in
// failure — see [FleetView.SelfStatus].
func (v FleetView) triedAndFailed() bool {
	return v.SelfStatus == StatusError || v.SelfStatus == StatusDegraded
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
	return max(backoff.Jitter(ReconcileInterval, ReconcileJitter), time.Second)
}
