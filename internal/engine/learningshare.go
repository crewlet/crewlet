package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
)

// learningShare holds one seat's learning to learning.max_share of its
// company day of spend: its learning calls are counted on a counter of their
// own beside the seat's, and one is not made once that counter reaches the
// share of the seat's day. The next company day starts it again.
type learningShare struct {
	budgets interface {
		Used(ctx context.Context, scope string, windows coord.Windows) (coord.Usage, error)
		PostChargeSeat(ctx context.Context, seat string, tokens int, windows coord.Windows) (coord.Usage, error)
	}
	seat, learning string
	zone           *time.Location
	max            float64
}

// learningShareFor is each seat's share, nil where none holds it: no
// coordination store, a cap of 1, or a role no agent seat counter names.
func (e *Engine) learningShareFor(c *Company) func(*org.Role) *learningShare {
	if e.backends == nil || e.backends.Fleet == nil || c == nil || c.Config == nil {
		return nil
	}
	share := c.Config.Learning.Share()
	if share >= 1 {
		return nil
	}
	return func(role *org.Role) *learningShare {
		if c.Org == nil {
			return nil
		}
		seat := c.Org.AgentSeatByHandle(seatHandle(role))
		if seat == nil {
			return nil
		}
		id, ok := c.Org.AgentIDFor(seat)
		if !ok {
			return nil
		}
		return &learningShare{budgets: e.backends.Fleet, seat: coord.AgentScope(id.String()),
			learning: coord.LearningScope(id.String()), zone: basisOf(c, seat).zone, max: share}
	}
}

// room reports whether the seat's learning is under its share of the day at.
// A counter that cannot be read is room: a blip must not stop a company
// learning, and the call is still counted on the way out.
func (s *learningShare) room(ctx context.Context, at time.Time) bool {
	if s == nil {
		return true
	}
	windows := coord.WindowsAt(at, s.zone)
	seat, err := s.budgets.Used(ctx, s.seat, windows)
	if err != nil {
		return true
	}
	learning, err := s.budgets.Used(ctx, s.learning, windows)
	if err != nil {
		return true
	}
	total, spent := seat.In(period.Day).Used, learning.In(period.Day).Used
	if total > 0 && float64(spent) >= s.max*float64(total) {
		log.InfoContext(ctx, "learning_share_reached", "seat", s.seat, "learning_tokens", spent,
			"day_tokens", total, "share", s.max,
			"detail", "the seat's learning waits for the next company day")
		return false
	}
	return true
}

// record counts tokens a learning call spent on the seat's learning counter.
func (s *learningShare) record(ctx context.Context, tokens int, at time.Time) {
	if s == nil || tokens <= 0 {
		return
	}
	if _, err := s.budgets.PostChargeSeat(ctx, s.learning, tokens, coord.WindowsAt(at, s.zone)); err != nil {
		log.WarnContext(ctx, "learning_share_not_recorded", "seat", s.seat, "error", err,
			"detail", "this call is missing from the seat's learning share")
	}
}
