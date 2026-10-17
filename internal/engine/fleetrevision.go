package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/maintenance"
)

// A JUDGEMENT THAT CANNOT BE UNDONE, MADE AGAINST THE ACTIVE REVISION, IS MADE
// ONLY ON A NODE THAT SERVES THE REVISION THE FLEET'S ACTIVATION POINTER NAMES.
//
// Two such judgements exist, and each acts for the whole fleet:
//
//   - the MAILBOX RETIREMENT judges a seat absent from the revision and deletes
//     its mail ([Engine.activeSeatHandles]);
//   - the CODE-RETENTION PURGE judges a repository deleted longer than the
//     revision's code.deleted_retention_days and removes it ([codeRetentionDuty]).
//
// A node that has not applied the pointer's epoch — propagation, a refused
// apply, a node stuck on an older revision — judges against a revision the
// fleet has replaced. For traffic that is the posture's business, and lag
// alone never sheds (configplane). These two cannot wait for a posture: a
// seat the fleet has just added back would lose its mail, and a revision that
// lengthens the retention to save a deleted repository would be defeated by a
// duty holder that failed to apply it. Neither has an undo.
//
// STRONGER THAN [Reconciler.Admits], on purpose: the posture admits a node
// still converging and fails open on what it cannot read, which is right for
// work that a later apply corrects and wrong for work nothing corrects. What
// declining costs here is a judgement made later — a retirement a sweep later;
// a purge a reconcile interval later on a node still applying the revision,
// and one tick of its duty later on a node that failed it — by this node once
// it catches up or by whichever node holds the duty and serves the fleet's
// revision, against a grace and a retention measured in hours and days.
//
// ONE HELPER, because the rule is written once: two copies of "does this node
// serve the fleet's revision" are two places for the reads below to be
// reordered or for one of the three answers to be collapsed.
//
// # Behind is not yet a fault
//
// EVERY node is behind the pointer for a moment at every activation — the
// nudge reaches it in milliseconds and its apply takes seconds — and at every
// boot, until its reconciler is built and has ticked once. Neither is a fault,
// and a reader that reported either as one would raise a warning on every
// deploy and every configuration write, which teaches whoever reads it to
// ignore the one a node that failed the fleet's apply raises. So the third
// answer says WHICH it is, by the posture's own rule
// ([configplane.FleetView.LagConfirmed], over [applyProgress.selfView]): a lag
// CONFIRMED — an attempt of this node's at the epoch concluded in failure, or
// it has been behind on it longer than propagation explains — or ordinary
// propagation, which every reader treats as "not yet", quietly.

// errBehindFleet is a node that does not serve the activation epoch the fleet's
// pointer names, and how far it has got: whether it has a reconciler at all,
// whether that reconciler has read the activation pointer, the epoch it serves,
// and whether its lag is confirmed or still propagation.
type errBehindFleet struct {
	applied, target int64
	// reconciler is whether this node has a reconciler yet, and read
	// whether its tick has read the activation pointer at least once. Both
	// are false on a node still booting: `crewlet run` builds the
	// reconciler after the engine, ticks it once before it serves, and
	// exits if it cannot build one — so no running node lacks one for
	// longer than its boot.
	reconciler, read bool
	// confirmed is whether the reconciler confirms the lag
	// ([configplane.FleetView.LagConfirmed]). Never true of a node that has
	// not read the pointer: what it has not seen it has neither tried nor
	// been behind on, and the posture reads such a node as waiting — so a
	// node with no reconciler yet reads as one whose reconciler has not
	// ticked, which is what it is.
	confirmed bool
}

func (err *errBehindFleet) Error() string {
	switch {
	case !err.reconciler:
		return fmt.Sprintf("engine: this node runs no reconciler, so it cannot tell whether it "+
			"serves activation epoch %d", err.target)
	case !err.read:
		return fmt.Sprintf("engine: this node's reconciler has not read the activation pointer yet, "+
			"so it cannot tell whether it serves activation epoch %d", err.target)
	}
	return fmt.Sprintf("engine: this node serves activation epoch %d and the fleet is on %d",
		err.applied, err.target)
}

// reconciled is whether this node's reconciler has read the activation
// pointer, so its progress can say how far behind it is.
func (err *errBehindFleet) reconciled() bool { return err.reconciler && err.read }

// Is makes a lag that is not confirmed [maintenance.ErrRevisionPropagating],
// the roster's quiet answer.
func (err *errBehindFleet) Is(target error) bool {
	return target == maintenance.ErrRevisionPropagating && !err.confirmed
}

// servedFleetRevision is the company this node serves, where it serves the
// revision the fleet's activation pointer names — THREE ANSWERS, never two:
//
//   - the company and a nil error: this node has applied the pointer's epoch,
//     and the company is that revision's or a later one;
//   - [maintenance.ErrNoActiveRevision]: nothing has been activated. NOT
//     CURRENT for this rule, unlike the reconciler's posture, which judges
//     traffic and reads an unconfigured deployment as converged: a node boots
//     on a company file, or its store's company, before its first reconcile
//     tick activates anything, and an irreversible judgement against a
//     revision nobody activated is exactly what this exists to refuse. A
//     fresh deployment meets it for the seconds before its first activation;
//   - any other error: UNKNOWN or BEHIND — the pointer could not be read, or
//     this node does not serve the pointer's epoch ([errBehindFleet]: it runs
//     no reconciler yet, its reconciler has not read the pointer yet, or it
//     serves an older epoch, naming both) — and that last says whether the
//     lag is CONFIRMED or still propagation ([errBehindFleet.confirmed],
//     [maintenance.ErrRevisionPropagating]), which every reader must tell
//     apart: a node is behind for a moment at every activation and every
//     boot.
//
// The three reads are ordered so the company can only be NEWER than the epoch
// it is checked against, never older: the pointer first, the applied epoch
// next, the company last. An apply installs its company before the reconciler
// records its epoch, so a company read after a matching epoch is that
// revision's or a later one — and the pointer only ever moves forward, so a
// later revision is only ever a truer one. Which is also why a node AHEAD of
// the pointer it read serves it: the pointer moved after it was read and this
// node had already applied what it moved to, the posture's own reading of
// that race.
func (e *Engine) servedFleetRevision(ctx context.Context) (*Company, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil, fmt.Errorf("engine: this node has no fleet store to read the activation pointer from")
	}
	target, found, err := e.backends.Fleet.Target(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: read the activation pointer: %w", err)
	}
	if !found {
		return nil, maintenance.ErrNoActiveRevision
	}
	r := e.reconciler.Load()
	if r == nil {
		return nil, &errBehindFleet{target: target.Epoch}
	}
	// ONE SNAPSHOT for the applied epoch and the lag, so the lag judged is
	// the lag of the epoch read.
	progress := r.snapshot()
	self := progress.selfView(target.Epoch, r.now())
	if self.AppliedEpoch < target.Epoch {
		return nil, &errBehindFleet{applied: self.AppliedEpoch, target: target.Epoch,
			reconciler: true, read: progress.target != 0, confirmed: self.LagConfirmed()}
	}
	company := e.Company()
	if company == nil {
		return nil, fmt.Errorf("engine: this node has applied activation epoch %d and holds no "+
			"company for it", target.Epoch)
	}
	return company, nil
}
