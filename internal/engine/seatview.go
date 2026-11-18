package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/org"
)

// SeatView answers [session.Chart] (which seat a signed-in person acts as)
// against the org chart of the epoch this node applied, read per call: the
// seat a request resolves to is the one every surface on this node routes and
// authorizes by, never a second copy of the org that could disagree.
type SeatView struct{ engine *Engine }

var _ session.Chart = SeatView{}

// SeatViewOf is the seam over one engine, or the zero value over none. Not a
// nil [session.Chart], which would make every bound person seatless (the arm
// [session.Binding.Handle] must never reach by fall-through): a view with no
// engine answers unknown, a 503.
func SeatViewOf(e *Engine) SeatView { return SeatView{engine: e} }

// errNoOrg is a view's answer on a node with no company yet: wrapping
// [session.ErrNoCompany], so the identity writer can say "create a company
// first" while the request path reads it as the unknown arm.
var errNoOrg = fmt.Errorf("engine: %w, so it cannot say which seat anybody "+
	"holds", session.ErrNoCompany)

// Seat finds a seat in the running organisation by its handle. One it does
// not hold is conclusively not found, with no error: the organisation is the
// company this node serves, not a log it may be behind on.
func (v SeatView) Seat(_ context.Context, handle string) (session.Seat, bool, error) {
	company := v.company()
	if company == nil {
		return session.Seat{}, false, errNoOrg
	}
	role := company.Org.Role(handle)
	if role == nil {
		return session.Seat{}, false, nil
	}
	seat := session.Seat{Handle: role.Handle(), Name: role.Name,
		Kind: string(role.EffectiveKind())}
	if unit := company.Org.UnitFor(role); unit != nil {
		seat.Unit = unit.Key()
	}
	return seat, true, nil
}

// HumanSeats is every human seat of the running organisation, in the order the
// company declares them, and false on a node that runs no company yet.
func (v SeatView) HumanSeats() ([]session.Seat, bool) {
	company := v.company()
	if company == nil {
		return nil, false
	}
	var seats []session.Seat
	for role := range company.Org.AllRoles() {
		if role.EffectiveKind() != org.KindHuman {
			continue
		}
		seat := session.Seat{Handle: role.Handle(), Name: role.Name,
			Kind: string(role.EffectiveKind())}
		if unit := company.Org.UnitFor(role); unit != nil {
			seat.Unit = unit.Key()
		}
		seats = append(seats, seat)
	}
	return seats, true
}

// company is the epoch this view answers from, or nil with no engine or no
// company.
func (v SeatView) company() *Company {
	if v.engine == nil {
		return nil
	}
	c := v.engine.Company()
	if c == nil || c.Org == nil {
		return nil
	}
	return c
}
