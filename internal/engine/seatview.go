package engine

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/iam/session"
)

// SeatView answers [session.Chart] — which seat a signed-in person acts as —
// against the organisation this node runs: the org chart of the configuration
// epoch it applied.
//
// THE RUNNING COMPANY, read per call, and nothing beside it. The org changes
// only when a revision is activated, and every node applies the activation
// pointer within seconds of it moving, so the seat a request is resolved
// against is the seat every surface on this node routes, attributes and
// authorizes by — never a second copy of the org that could disagree with it.
type SeatView struct{ engine *Engine }

var _ session.Chart = SeatView{}

// SeatViewOf is the seam over one engine, or the zero value over none.
//
// THE ZERO VALUE IS NOT NIL, and the difference is what such a view answers. A
// nil [session.Chart] would make every bound person seatless — the one arm
// [session.Binding.Handle] is documented never to be reached by a
// fall-through — so a view with no engine answers UNKNOWN to every seat
// question instead, which is 503 and says come back to a node that can tell.
func SeatViewOf(e *Engine) SeatView { return SeatView{engine: e} }

// errNoOrg is what a view answers on a node that runs no company yet.
var errNoOrg = errors.New("engine: this node runs no company yet, so it " +
	"cannot say which seat anybody holds")

// Seat finds a seat in the running organisation by its handle.
//
// A SEAT THE ORGANISATION DOES NOT HOLD is not found, with no error: a removed
// seat is simply absent, and that is conclusive, because the organisation is
// the company this node serves rather than a log it may be behind on.
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

// Version names the organisation [SeatView.Seat] answers from: it moves on
// every epoch this node publishes, so two equal readings answer every seat
// alike — which is what lets the dangling-binding watch skip a beat on which
// nothing moved.
func (v SeatView) Version(context.Context) (uint64, error) {
	if v.company() == nil {
		return 0, errNoOrg
	}
	return v.engine.epoch.installed.Load(), nil
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
