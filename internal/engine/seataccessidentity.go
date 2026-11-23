package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// Identity has one seat's sync keep or give up its own identity on lane
// (ADR-0179), under the kind's lease as every ledger write is, and answers the
// verdict the sync recorded: the kept identity's check, or the attempt it did
// not keep and why.
func (a *SeatAccessAPI) Identity(ctx context.Context, q setupapi.AccessQuery, handle string,
	lane seataccess.Lane, ask seataccess.IdentityAsk, by iam.Actor) (seataccess.IdentityCheck, error) {

	vendor, ok := seataccess.Lookup(q.Kind)
	if !ok {
		return seataccess.IdentityCheck{}, setupapi.ErrNoIdentity
	}
	if spec, ok := vendor.Catalog().Lane(lane); !ok || !spec.Identity.Offered() {
		return seataccess.IdentityCheck{}, setupapi.ErrNoIdentity
	}
	var seat *seataccess.Seat
	for _, s := range seataccess.SeatsOf(q.Company) {
		if s.Handle == handle {
			seat = &s
			break
		}
	}
	if seat == nil {
		return seataccess.IdentityCheck{}, fmt.Errorf("%w: %s", setupapi.ErrNoSuchAccessSeat, handle)
	}
	if want, _ := seataccess.DesiredOf(seat.Role, lane); ask.Keep && !want.Enabled {
		return seataccess.IdentityCheck{}, fmt.Errorf("%w: %s", setupapi.ErrNotAwaitingHandIn, handle)
	}
	// THE SYNC READS THE COMPANY THIS NODE RUNS, so a revision the route just
	// wrote (the seat's `identity`) is applied first.
	if err := a.waitApplied(ctx, q.Kind); err != nil {
		return seataccess.IdentityCheck{}, err
	}
	ledger, err := a.engine.SeatAccessLedger()
	if err != nil {
		return seataccess.IdentityCheck{}, err
	}
	// THIS PASS'S VERDICT ALONE: the sync stamps what it records at its own
	// start, never earlier than this.
	started := time.Now().UTC()
	asks := map[string]seataccess.Ask{handle: {Lane: lane, Identity: &ask}}
	if syncErr := a.syncIdentity(ctx, q.Kind, by, asks, handle); syncErr != nil {
		return seataccess.IdentityCheck{}, syncErr
	}
	rec, _, err := ledger.SeatRecord(ctx, q.Kind, handle)
	if errors.Is(err, coord.ErrUnavailable) {
		return seataccess.IdentityCheck{}, fmt.Errorf("%w: %w", setupapi.ErrAccessUnavailable, err)
	}
	if err != nil {
		return seataccess.IdentityCheck{}, err
	}
	ir := rec.Lanes[lane].Identity
	switch {
	case ask.Drop && !ir.Kept():
		return seataccess.IdentityCheck{Status: seataccess.IdentityCompanys,
			Said: "Calls go as the company's identity again."}, nil
	case ask.Drop:
		return seataccess.IdentityCheck{}, errors.New("engine: the sync did not give the identity up")
	case ir != nil && ir.Attempt != nil && !ir.Attempt.At.Before(started):
		return *ir.Attempt, nil
	case ir.Kept() && ir.Check != nil && !ir.Check.At.Before(started):
		return *ir.Check, nil
	}
	return seataccess.IdentityCheck{}, errors.New("engine: the sync kept no identity and recorded no reason")
}

// syncIdentity runs the seat's sync carrying an identity ask and answers its
// fault, which a save's sync leaves to the record: here the person waits on
// this pass's verdict, and an older record's reason would answer the wrong one.
func (a *SeatAccessAPI) syncIdentity(ctx context.Context, kind integration.Kind, by iam.Actor,
	asks map[string]seataccess.Ask, handle string) error {

	ctx, cancel := context.WithTimeout(ctx, setup.PassDeadline)
	defer cancel()
	a.mark(kind, true)
	defer a.mark(kind, false)
	_, err := a.engine.SyncSeatAccess(ctx, kind, by, asks, []string{handle})
	switch {
	case errors.Is(err, ErrSeatAccessBusy):
		return fmt.Errorf("%w: %w", setupapi.ErrAccessSyncInFlight, err)
	case errors.Is(err, coord.ErrUnavailable):
		return fmt.Errorf("%w: %w", setupapi.ErrAccessUnavailable, err)
	case errors.Is(err, integration.ErrNotConfigured):
		return fmt.Errorf("%w: %w", setupapi.ErrNotAwaitingHandIn, err)
	}
	return err
}

// seatIdentity is who signs handle's calls on lane, for a growth product.
func (e *Engine) seatIdentity(lane seataccess.Lane) func(ctx context.Context, handle string) (seataccess.Signer,
	error) {
	return func(ctx context.Context, handle string) (seataccess.Signer, error) {
		ledger, err := e.SeatAccessLedger()
		if err != nil {
			return seataccess.Signer{}, err
		}
		return seataccess.ResolveSeat(ctx, ledger, e.runningConfig(), lane, handle)
	}
}

var _ setupapi.Identities = (*SeatAccessAPI)(nil)
