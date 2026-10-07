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
		return e.reflectionRoom(ctx, c, seatHandle(seat))
	}
}

// reflectionRoom is the REFLECTION STAGE's gate for one seat: whether it may
// start auxiliary work filed under that stage, which is everything it spends
// remembering a turn once the turn is over — the reflection pass
// ([Engine.learningBudget]) and the rewrites of a conversation entry
// ([Dispatcher.ReflectionRoom]).
//
// Three-valued: (true, nil) where nothing in the epoch caps the seat or a
// capped window has room, (false, nil) where one has none, and an error where
// the counter could not be read — which the caller treats as room, since a
// coordination blip must not silently stop a company learning, and the
// charge on the way out is what keeps an unreachable counter from also being
// a free one.
func (e *Engine) reflectionRoom(ctx context.Context, c *Company, handle string) (bool, error) {
	headroom := e.remainingFor(c, handle)
	if headroom == nil {
		// Nothing in the epoch caps this seat's spend.
		return true, nil
	}
	left, err := headroom.Remaining(ctx)
	if err != nil {
		// UNKNOWN is not "no".
		return true, err
	}
	// A capped window with nothing left refuses the next token, so no work
	// that needs one may start.
	return left > 0, nil
}

// seatHandle is a seat's handle, tolerating the nil the gate may be handed.
func seatHandle(seat *org.Role) string {
	if seat == nil {
		return ""
	}
	return seat.Handle()
}
