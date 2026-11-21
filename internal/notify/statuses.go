package notify

import (
	"context"
	"slices"
)

// Statuses is every chat backend's indicator driver, addressed as one.
//
// # Why a set rather than "the" driver
//
// A company can run several chat surfaces, and a turn is triggered by exactly
// one. A driver refuses a trigger whose `transport` key is not its own, so
// handing out the FIRST backend wired would leave every turn on the second
// silently unindicated. [StatusDriver.Begin] already answers nil for a
// conversation it does not own, so this walks the drivers and keeps the one
// that claims the trigger; at most one can.
type Statuses struct{ drivers []*StatusDriver }

// NewStatuses collects the drivers a node is running.
//
// A nil or off driver is kept rather than filtered: it answers nil to
// everything, which is exactly what it should, and dropping it would make
// the set's length mean something it does not.
func NewStatuses(drivers ...*StatusDriver) *Statuses {
	kept := make([]*StatusDriver, 0, len(drivers))
	for _, d := range drivers {
		if d != nil {
			kept = append(kept, d)
		}
	}
	return &Statuses{drivers: kept}
}

// Begin opens or joins the indicator for a turn's trigger.
//
// NEVER NIL-CHECKED BY THE CALLER: a nil set, an empty one, and a trigger
// from a source with no indicator all answer a nil session, whose methods
// are no-ops. The turn engine should not have to ask whether indicators
// exist before saying what phase it is in.
func (s *Statuses) Begin(ctx context.Context, handle, turnID, phase string, metadata map[string]string) *StatusSession {
	if s == nil {
		return nil
	}
	for _, d := range s.drivers {
		if session := d.Begin(ctx, handle, turnID, phase, metadata); session != nil {
			return session
		}
	}
	return nil
}

// Rejoin takes back the hold a resumed turn already has, on whichever backend
// holds it.
//
// NEVER NIL-CHECKED BY THE CALLER, on the same terms as Begin: a resume whose
// session is not on this node — the seat moved, or the process restarted —
// answers a nil session whose methods are no-ops. At most one driver can hold
// a given turn's hold, because a turn is woken by exactly one surface.
func (s *Statuses) Rejoin(handle, turnID string) *StatusSession {
	if s == nil {
		return nil
	}
	for _, d := range s.drivers {
		if session := d.Rejoin(handle, turnID); session != nil {
			return session
		}
	}
	return nil
}

// Release drops ONE turn's hold on its indicator, wherever that hold is.
//
// The engine calls it when a kept-alive turn has STOPPED without ending: an
// executor parked on a question waits on a person, and "is thinking…" over
// that wait is worse than no indicator.
//
// [Statuses.Rejoin] then [StatusSession.End], not a clear of its own: holds
// are reference-counted by turn id, so a second turn in the thread keeps ITS
// indicator and only the last hold takes it down. A hold this node lacks
// answers a nil session whose End is a no-op: the node that raised it cleared
// it on releasing the seat ([Statuses.ClearFor]).
func (s *Statuses) Release(ctx context.Context, handle, turnID string) {
	s.Rejoin(handle, turnID).End(ctx, false)
}

// Queue raises a queued indicator for a person's message waiting behind the
// seat's current work, on whichever backend the message came from
// ([StatusDriver.Queue]); it reports whether any did.
func (s *Statuses) Queue(ctx context.Context, handle, wake string, metadata map[string]string) bool {
	if s == nil {
		return false
	}
	for _, d := range s.drivers {
		if d.Queue(ctx, handle, wake, metadata) {
			return true
		}
	}
	return false
}

// Dequeue takes the waits of these wakes away on every backend
// ([StatusDriver.Dequeue]): a driver that queued none of them does nothing.
func (s *Statuses) Dequeue(ctx context.Context, handle string, wakes []string) {
	if s == nil {
		return
	}
	for _, d := range s.drivers {
		d.Dequeue(ctx, handle, wakes)
	}
}

// ClearFor takes down every indicator either backend holds for one seat.
//
// Both, unconditionally: a seat can be configured on two chat surfaces at
// once, and a driver holding nothing for it does nothing. See
// [StatusDriver.ClearFor] for why a node that stops running a seat has to make
// this call.
func (s *Statuses) ClearFor(ctx context.Context, handle string) {
	if s == nil {
		return
	}
	for _, d := range s.drivers {
		d.ClearFor(ctx, handle)
	}
}

// Backends names the chat surfaces with a live driver, sorted.
func (s *Statuses) Backends() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.drivers))
	for _, d := range s.drivers {
		if name := d.Backend(); name != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Stop takes every live indicator down.
func (s *Statuses) Stop(ctx context.Context) {
	if s == nil {
		return
	}
	for _, d := range s.drivers {
		d.Stop(ctx)
	}
}
