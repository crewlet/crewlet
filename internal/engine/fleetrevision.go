package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// AN IRREVERSIBLE JUDGEMENT AGAINST THE ACTIVE REVISION RUNS ONLY ON A NODE
// SERVING THE ACTIVATION THE FLEET'S POINTER NAMES ([fleetCompanyAt], the one
// rule): the mailbox retirement ([Engine.activeSeats]), the clearing of a
// removed seat's pause ([Engine.clearRemovedSeatPauses]) and the code-retention
// purge ([codeRetentionDuty]). Each acts for the whole fleet and has no undo: a
// lagging node would delete the mail or pause of a seat just added back, or
// purge a repository a lengthened code.deleted_retention_days saves. Stricter than
// [Reconciler.Admits] on purpose: the posture admits a converging node and
// fails open, right for work a later apply corrects (lag alone never sheds,
// configplane) and wrong here, where declining only delays the judgement by a
// sweep, an apply or a duty tick, against a grace measured in hours and days.

// errBehindFleet is a node that does not serve the activation the fleet's
// pointer names, and how far it has got. Every node is behind for a moment at
// every activation and every boot, so it says whether the lag is CONFIRMED
// ([configplane.FleetView.LagConfirmed] over [applyProgress.selfView]: an
// attempt at the epoch failed, or it is behind longer than propagation
// explains) or propagation, which every reader treats as a quiet "not yet": a
// warning on every deploy teaches readers to ignore the one a failed apply
// raises.
type errBehindFleet struct {
	applied, target int64
	// reconciler is whether this node has a reconciler yet, and read whether
	// its tick has read the activation pointer. Both are false only while
	// booting: `crewlet run` builds the reconciler after the engine, ticks it
	// once before serving, and exits if it cannot build one.
	reconciler, read bool
	// confirmed is [configplane.FleetView.LagConfirmed]; never true of a node
	// that has not read the pointer, which the posture reads as waiting.
	confirmed bool
	// served is what this node's company was applied from against the
	// pointer, by instant ([servedAgainst]): a node with no reconciler, or one
	// that has not read the pointer, has no epoch to give. A decline a person
	// reads ([chatPrune.tick] logs it every tick) must name both activations,
	// or waiting cannot be told from failing.
	served string
}

func (err *errBehindFleet) Error() string {
	switch {
	case !err.reconciler:
		return fmt.Sprintf("engine: this node runs no reconciler, so it cannot tell whether it "+
			"serves activation epoch %d: %s", err.target, err.served)
	case !err.read:
		return fmt.Sprintf("engine: this node's reconciler has not read the activation pointer yet, "+
			"so it cannot tell whether it serves activation epoch %d: %s", err.target, err.served)
	}
	return fmt.Sprintf("engine: this node serves activation epoch %d and the fleet is on %d: %s",
		err.applied, err.target, err.served)
}

// reconciled is whether this node's reconciler has read the activation
// pointer, so its progress can say how far behind it is.
func (err *errBehindFleet) reconciled() bool { return err.reconciler && err.read }

// Is makes a lag that is not confirmed [maintenance.ErrRevisionPropagating],
// the roster's quiet answer.
func (err *errBehindFleet) Is(target error) bool {
	return target == maintenance.ErrRevisionPropagating && !err.confirmed
}

// servedFleetRevision is the company this node serves where it serves the
// revision the fleet's pointer names — THREE ANSWERS, never two: the company
// and nil ([fleetCompanyAt]); [maintenance.ErrNoActiveRevision] when nothing
// is activated, NOT CURRENT here (unlike the traffic posture), since a node
// boots on a company nobody activated; or any other error, UNKNOWN or BEHIND
// ([errBehindFleet], confirmed or [maintenance.ErrRevisionPropagating], which
// every reader must tell apart).
//
// The pointer is read before the company, so the company can only be newer
// (the pointer only moves forward); where it is not the pointer's, it is read
// again AFTER the reconciler's progress, since an apply installs its company
// before the reconciler records its epoch.
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
	company := e.Company()
	if _, err = fleetCompanyAt(target, company); err == nil {
		return company, nil
	}
	served := servedAgainst(target, company)
	r := e.reconciler.Load()
	if r == nil {
		return nil, &errBehindFleet{target: target.Epoch, served: served}
	}
	// ONE SNAPSHOT for the applied epoch and the lag, so the lag judged is
	// the lag of the epoch read.
	progress := r.snapshot()
	self := progress.selfView(target.Epoch, r.now())
	if self.AppliedEpoch < target.Epoch {
		return nil, &errBehindFleet{applied: self.AppliedEpoch, target: target.Epoch,
			reconciler: true, read: progress.target != 0, confirmed: self.LagConfirmed(),
			served: served}
	}
	return fleetCompanyAt(target, e.Company())
}

// fleetCompany is nil when c is the company of the activation the fleet's
// pointer names or a later one, else an error saying why — for a caller
// judging a company it holds rather than the one this node publishes (the
// pause clearing in [Engine.clearRemovedSeatPauses], on the company an apply
// is about to publish; the model catalogue's retry, on the one it is about to
// install). Every other reader asks [Engine.servedFleetRevision].
func (e *Engine) fleetCompany(ctx context.Context, c *Company) error {
	if e.backends == nil || e.backends.Fleet == nil {
		return fmt.Errorf("engine: this node has no fleet store to read the activation pointer from")
	}
	target, found, err := e.backends.Fleet.Target(ctx)
	if err != nil {
		return fmt.Errorf("engine: read the activation pointer: %w", err)
	}
	if !found {
		return maintenance.ErrNoActiveRevision
	}
	_, err = fleetCompanyAt(target, c)
	return err
}

// fleetCompanyAt is c when it was applied from target's activation or a later
// one, else an error saying it is not. It compares INSTANTS ([Company.ActivatedAt]
// against [coord.ActivationAt], strictly later on every activation) in the
// precision the store keeps, not epochs: a re-activation of an unchanged
// revision (rotation) is a new activation, and a company applied from the
// pointer's own activation is accepted while the reconciler is still inside
// the apply publishing it, where the pause clearing runs
// ([Engine.followCompany]). A company no activation named (a booted Tier B
// file) is not the pointer's until the reconciler's first apply.
func fleetCompanyAt(target coord.Activation, c *Company) (*Company, error) {
	if c != nil && !c.ActivatedAt.IsZero() &&
		store.EncodeTime(c.ActivatedAt) >= store.EncodeTime(target.At) {
		return c, nil
	}
	return nil, errors.New("engine: " + servedAgainst(target, c))
}

// servedAgainst names, by instant, the activation c was applied from and the
// one the fleet's pointer names: the words [fleetCompanyAt] and
// [errBehindFleet] both carry, so a reader is told the same whichever noticed.
func servedAgainst(target coord.Activation, c *Company) string {
	fleet := fmt.Sprintf("the fleet is on activation epoch %d of %s",
		target.Epoch, target.At.Format(time.RFC3339Nano))
	switch {
	case c == nil:
		return "this node publishes no company yet and " + fleet
	case c.ActivatedAt.IsZero():
		// AN OFFER, most likely: what this node runs no activation has
		// named, so there is no instant of its own to report.
		return "this node serves a company no activation has named (an offline " +
			"command's choice the fleet has not taken, or one handed to the engine " +
			"in code) and " + fleet
	}
	return fmt.Sprintf("this node serves the activation of %s and %s",
		c.ActivatedAt.Format(time.RFC3339Nano), fleet)
}

// fleetSeatWait bounds how long [Engine.FleetSeat] waits for this node to run
// the activation the fleet's pointer names: a write just saved is applied here
// within moments.
const fleetSeatWait = 10 * time.Second

// FleetSeat is handle's agent seat as the revision the fleet's activation
// pointer names holds it, once this node runs that revision, waiting for it
// up to [fleetSeatWait]: what an answer is checked against right after a save.
func (e *Engine) FleetSeat(ctx context.Context, handle string) (*org.Role, error) {
	deadline := time.Now().Add(fleetSeatWait)
	for {
		company, err := e.servedFleetRevision(ctx)
		var behind *errBehindFleet
		switch {
		case err == nil && (company == nil || company.Org == nil):
			return nil, errors.New("engine: no company is active")
		case err == nil:
			if seat := company.Org.AgentSeatByHandle(handle); seat != nil {
				return seat, nil
			}
			return nil, fmt.Errorf("engine: %q is no agent seat of the active revision", handle)
		case !errors.As(err, &behind) || time.Now().After(deadline):
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
