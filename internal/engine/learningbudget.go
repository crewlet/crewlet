package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/org"
)

// The PRE-FLIGHT half of auxiliary spend: whether a reflection pass may start
// at all. The record half — every auxiliary completion charged to the counters
// and written to the spend history after it returns — is the seam's, in
// auxiliary.go.

// learningBudget is the reflection pass's pre-flight gate.
//
// Reflection is best effort, so it does not FAIL on an exhausted budget — it
// declines to start. That distinction is the whole point: a pass that runs
// and fails has already made its auxiliary calls.
//
// IT ASKS FOR THE HEADROOM, which is a read and moves nothing. It used to ask
// with a charge of zero tokens, and the counter answers every such charge OK
// without looking — a phase whose provider reported no usage still ran, and
// refusing it would stop a company over a backend that omits the field — so
// the gate had never declined a pass: a company at its ceiling went on
// starting reflection passes, and paying for their auxiliary calls, until
// each one's first charge was refused mid-pass.
func (e *Engine) learningBudget(c *Company) func(context.Context, *org.Role) (bool, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return func(ctx context.Context, seat *org.Role) (bool, error) {
		headroom := e.remainingFor(c, seatHandle(seat))
		if headroom == nil {
			// Nothing in the epoch caps this seat's spend.
			return true, nil
		}
		left, err := headroom.Remaining(ctx)
		if err != nil {
			// UNKNOWN is not "no". A coordination blip must not silently
			// stop a company learning; the charge on the way out is what
			// keeps an unreachable counter from also being a free one.
			return true, err
		}
		// A capped window with nothing left refuses the next token, so
		// no pass that needs one may start.
		return left > 0, nil
	}
}

// seatHandle is a seat's handle, tolerating the nil the gate may be handed.
func seatHandle(seat *org.Role) string {
	if seat == nil {
		return ""
	}
	return seat.Handle()
}
