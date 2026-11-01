package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/store"
)

// A JUDGEMENT THAT CANNOT BE UNDONE, MADE AGAINST THE ACTIVE REVISION, IS MADE
// ONLY ON A NODE THAT SERVES THE REVISION THE FLEET'S ACTIVATION POINTER NAMES.
//
// Three such judgements exist, and each acts for the whole fleet:
//
//   - the MAILBOX RETIREMENT judges a seat absent from the revision and deletes
//     its mail ([Engine.activeSeats]);
//   - the CLEARING OF A REMOVED SEAT'S PAUSE judges a seat absent and deletes a
//     pause a person placed ([Engine.clearRemovedSeatPauses]);
//   - the CODE-RETENTION PURGE judges a repository deleted longer than the
//     revision's code.deleted_retention_days and removes it ([codeRetentionDuty]).
//
// A node that has not applied the pointer's activation — propagation, a
// refused apply, a node stuck on an older revision — judges against a
// revision the fleet has replaced. For traffic that is the posture's business,
// and lag alone never sheds (configplane). These cannot wait for a posture: a
// seat the fleet has just added back would lose its mail or its pause, and a
// revision that lengthens the retention to save a deleted repository would be
// defeated by a duty holder that failed to apply it. None has an undo.
//
// STRONGER THAN [Reconciler.Admits], on purpose: the posture admits a node
// still converging and fails open on what it cannot read, which is right for
// work that a later apply corrects and wrong for work nothing corrects. What
// declining costs here is a judgement made later — a retirement a sweep later;
// a pause cleared by the apply that catches this node up; a purge a reconcile
// interval later on a node still applying the revision, and one tick of its
// duty later on a node that failed it — by this node once it catches up or by
// whichever node holds the duty and serves the fleet's revision, against a
// grace and a retention measured in hours and days.
//
// ONE RULE ([fleetCompanyAt]), because it is written once: two copies of "does
// this node serve the fleet's revision" are two places for the reads below to
// be reordered or for one of the three answers to be collapsed.
//
// # Compared on the activation's instant
//
// WHETHER A COMPANY IS THE POINTER'S is read off the company itself
// ([fleetCompanyAt]): the instant of the activation it was applied from
// ([Company.ActivatedAt]) against the one the pointer publishes, strictly later
// on every activation ([coord.ActivationAt]). So a re-activation of an
// unchanged revision — the rotation gesture — is a different activation here
// too, and a company applied from the pointer's own activation is accepted
// even while the reconciler is still inside the apply that publishes it, which
// is where the pause clearing runs ([Engine.followCompany]). A company no
// activation has named yet — a Tier B file a node booted with — is not the
// pointer's until the reconciler's first apply. The reconciler is asked only
// to say what a company that is NOT the pointer's means: still propagating, or
// a lag it confirms.
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

// errBehindFleet is a node that does not serve the activation the fleet's
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
//   - the company and a nil error: the company this node publishes was applied
//     from the pointer's activation or a later one ([fleetCompanyAt]);
//   - [maintenance.ErrNoActiveRevision]: nothing has been activated. NOT
//     CURRENT for this rule, unlike the reconciler's posture, which judges
//     traffic and reads an unconfigured deployment as converged: a node boots
//     on a company file, or its store's company, before its first reconcile
//     tick activates anything, and an irreversible judgement against a
//     revision nobody activated is exactly what this exists to refuse. A
//     fresh deployment meets it for the seconds before its first activation;
//   - any other error: UNKNOWN or BEHIND — the pointer could not be read, or
//     this node does not serve the pointer's activation ([errBehindFleet]: it
//     runs no reconciler yet, its reconciler has not read the pointer yet, or
//     it serves an older epoch, naming both) — and that last says whether the
//     lag is CONFIRMED or still propagation ([errBehindFleet.confirmed],
//     [maintenance.ErrRevisionPropagating]), which every reader must tell
//     apart: a node is behind for a moment at every activation and every
//     boot.
//
// The reads are ordered so the company can only be NEWER than the pointer it
// is checked against, never older: the pointer first, the company after it.
// The pointer only ever moves forward, so a company applied from a LATER
// activation than the one read is only ever a truer one — the pointer moved
// after it was read and this node had already applied what it moved to, the
// posture's own reading of that race. Where the company is not the pointer's,
// the reconciler's progress says why, and the company is read once more AFTER
// that progress: an apply installs its company before the reconciler records
// its epoch, so a progress that has reached the pointer's epoch is one whose
// company the second read sees.
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
	if company, err := fleetCompanyAt(target, e.Company()); err == nil {
		return company, nil
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
	return fleetCompanyAt(target, e.Company())
}

// fleetCompany is c when it is the company of the activation the fleet is
// pointed at, or a later one, or an error saying why that is unknown — for a
// caller judging a company it holds rather than the one this node publishes:
// the clearing of a removed seat's pause, which runs on the company an apply
// is about to publish ([Engine.clearRemovedSeatPauses]), and the model
// catalogue's retry, on the one it is about to install. Every other reader
// asks [Engine.servedFleetRevision].
func (e *Engine) fleetCompany(ctx context.Context, c *Company) (*Company, error) {
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
	return fleetCompanyAt(target, c)
}

// fleetCompanyAt is c when it was applied from target's activation or a later
// one — COMPARED ON THE ACTIVATION'S INSTANT, in the precision the store keeps
// it in, for the reason this file gives — or an error saying it is not.
func fleetCompanyAt(target coord.Activation, c *Company) (*Company, error) {
	switch {
	case c == nil:
		return nil, errors.New("engine: this node publishes no company yet")
	case c.ActivatedAt.IsZero():
		// AN OFFER, most likely: what this node runs no activation has
		// named, so there is no instant of its own to report.
		return nil, fmt.Errorf("engine: this node serves a company no activation has "+
			"named (an offline command's choice the fleet has not taken, or one "+
			"handed to the engine in code) and the fleet is on activation epoch %d of %s",
			target.Epoch, target.At.Format(time.RFC3339Nano))
	case store.EncodeTime(c.ActivatedAt) < store.EncodeTime(target.At):
		return nil, fmt.Errorf("engine: this node serves the activation of %s and the "+
			"fleet is on activation epoch %d of %s", c.ActivatedAt.Format(time.RFC3339Nano),
			target.Epoch, target.At.Format(time.RFC3339Nano))
	}
	return c, nil
}
