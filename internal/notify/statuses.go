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
