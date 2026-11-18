package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/org"
)

// The PRE-FLIGHT half of auxiliary spend: whether a reflection pass may start.
// The record half, charging each auxiliary completion, is in auxiliary.go.

// learningBudget is the reflection pass's pre-flight gate. Reflection is best
// effort, so an exhausted budget makes it decline to start rather than fail: a
// pass that runs and fails has already made its auxiliary calls.
//
// It READS the windows, spending nothing. Asking with a zero-token charge is
// wrong: the counter answers every such charge OK without looking, so the gate
// would never decline. A decline writes only the refusal's record
// ([Engine.reflectionRoom]), which counts nothing.
func (e *Engine) learningBudget(c *Company) func(context.Context, *org.Role) (bool, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return func(ctx context.Context, seat *org.Role) (bool, error) {
		return e.reflectionRoom(ctx, c, seatHandle(seat))
	}
}

// reflectionRoom is the REFLECTION STAGE's gate for one seat: whether it may
// start auxiliary work filed under that stage ([Engine.learningBudget]).
//
// Three-valued: (true, nil) where nothing caps the seat or every capped window
// has room, (false, nil) where one has none, and an error where the counter
// could not be read. The caller treats an error as room, so a coordination blip
// does not stop a company learning; the charge on the way out keeps an
// unreachable counter from being a free one. The rule is the park's
// ([meter.refusing]). A no is recorded as the gate's refusal ([meter.turnAway]): the pass asks just
// before the work and makes no call on false, so the window's `refused_at`
// shows a seat whose passes are being declined.
func (e *Engine) reflectionRoom(ctx context.Context, c *Company, handle string) (bool, error) {
	m := e.meterFor(c, handle)
	if m == nil || !m.basis.capped() {
		return true, nil
	}
	r, refusing, err := m.refusing(ctx)
	if err != nil {
		return true, err
	}
	if refusing {
		m.turnAway(ctx, r)
		return false, nil
	}
	return true, nil
}

// seatHandle is a seat's handle, tolerating the nil the gate may be handed.
func seatHandle(seat *org.Role) string {
	if seat == nil {
		return ""
	}
	return seat.Handle()
}
