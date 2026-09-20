package notify

import (
	"context"
	"slices"
)

// Statuses is every chat backend's indicator driver, addressed as one.
//
// # Why a set rather than "the" driver
//
// A company can run more than one chat surface, and a turn is triggered by
// exactly one of them. A caller holding a single driver would either raise
// the indicator on the wrong backend or, more likely, raise it on none: the
// driver refuses a trigger whose `transport` key is not its own, so an
// engine that handed out the FIRST chat backend it wired would leave every
// turn on the second one silently unindicated.
//
// The dispatch needs no rules of its own because [StatusDriver.Begin]
// already answers nil for a conversation it does not own. So this walks the
// drivers and keeps whichever one claims the trigger — at most one can.
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
// It is what the engine calls when a turn that was kept alive has STOPPED
// without ending: a detached coding run that parked on a question is waiting
// on a person, not working, and an indicator still saying "is thinking…" over
// that wait is worse than no indicator at all.
//
// [Statuses.Rejoin] then [StatusSession.End] rather than a clear of its own,
// which is the whole of it: the hold is reference-counted by turn id, so a
// second turn holding the same thread — a queued follow-up, a colleague's ask
// — keeps ITS indicator and only the last hold takes the indicator down. A
// driver-level clear would have taken that second turn's indicator with it.
//
// A hold this node does not have answers a nil session whose End is a no-op,
// which is the honest outcome where the park landed on a node that is not the
// one that raised the indicator: that node cleared it when it released the
// seat ([Statuses.ClearFor]).
func (s *Statuses) Release(ctx context.Context, handle, turnID string) {
	s.Rejoin(handle, turnID).End(ctx, false)
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
