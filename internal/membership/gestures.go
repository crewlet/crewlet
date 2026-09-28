package membership

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/placement"
)

// Gesture refusals, each naming what the operator asked for that cannot be
// done. A map adds its own for a map that does not exist yet.
//
// THEY CARRY NO PACKAGE PREFIX, unlike every other error in this tree, and
// that is the rule rather than an exception to it: the text is the detail of
// every surface's refusal — the API's, the CLI's, the dashboard's — and the
// name a reader needs in front of it is the MAP's, since two maps answer these
// gestures. So each map's gestures wrap them in its own name, and an operator
// reads "objstore/upkeep: no such member of the placement map", never a
// package that means nothing to them.
var (
	// ErrUnknownMember names a node the map neither holds nor remembers.
	ErrUnknownMember = errors.New("no such member of the placement map")

	// ErrRemovedMember is taking out a node the map removed for absence
	// and has not seen back: it is not a member and places nothing, so
	// there is nothing to take out. It IS an unknown member to the
	// gesture, and wraps [ErrUnknownMember] so a caller that renders only
	// that still answers truthfully.
	//
	// A STATEMENT OF WHAT IS, with the node named last as every refusal
	// here names it, and no remedy in it: each surface words the remedy
	// for its own reader in its hint (`crewlet objects in`, a "Put back"
	// button), so a command name here would be the wrong one on two of the
	// three.
	ErrRemovedMember = fmt.Errorf("%w: the map removed it for absence, so it places "+
		"nothing to take out", ErrUnknownMember)

	// ErrNothingPlaceable is taking out the last member copies could be
	// placed on.
	ErrNothingPlaceable = errors.New("taking it out would leave no present member to " +
		"place copies on")

	// ErrHoldRange is a hold of no length, or one past [MaxHold].
	ErrHoldRange = errors.New("a hold lasts more than nothing and at most a day")
)

// Out takes a member out: the map places nothing on it, so its share moves to
// the others while it keeps serving what it holds — the gesture that makes a
// planned removal a copy rather than a recovery, since everything it holds
// has a live source until the others have it. The member stays until [In]
// puts it back or it is gone for the grace. A member on probation may be
// taken out too, and then stays out once its probation ends.
//
// TAKING OUT A MEMBER ALREADY OUT CHANGES NOTHING, the record of who took it
// out, why and when included: the answer is the state and draw it was given,
// so a caller that writes only what changed writes nothing, and an operator
// re-sending a gesture whose answer was lost — the thing a lost answer tells
// them to do — neither moves the record's version under a maintainer's tick
// nor overwrites the first operator's reason with a retry's.
//
// It refuses to take out the last member present to place copies on: every
// write would then have nowhere to land. Present is what the latest tick saw
// ([absentNow]), so a member back from a missed tick counts. And it refuses a
// node the map removed and has not seen back ([ErrRemovedMember]): that one
// places nothing already.
//
// Pure over the record: the caller reads it, applies this — and whatever its
// map does with a change to its members — and writes the result with a
// compare-and-set.
func Out(s State, d placement.Draw, node, by, reason string, now time.Time) (State, placement.Draw, error) {
	member, ok := d.Member(node)
	switch {
	case !ok:
		if _, removed := s.Removed[node]; removed {
			return s, d, fmt.Errorf("%w: %q", ErrRemovedMember, node)
		}
		return s, d, fmt.Errorf("%w: %q", ErrUnknownMember, node)
	case member.Out:
		return s, d, nil
	}
	if member.Placeable() {
		others := 0
		for _, m := range d.Placeable() {
			if m.Node != node && !absentNow(s, m.Node) {
				others++
			}
		}
		if others == 0 {
			return s, d, fmt.Errorf("%w: %s", ErrNothingPlaceable, node)
		}
	}
	next := s.Clone()
	if next.TakenOut == nil {
		next.TakenOut = map[string]Gesture{}
	}
	next.TakenOut[node] = Gesture{By: by, Reason: reason, At: now.UTC()}
	return next, setMember(d, node, func(m *placement.Member) { m.Out = true }), nil
}

// In puts a member back: the map places on it again, and its share moves back.
//
// IT VOUCHES FOR THE NODE, whatever is keeping it off the map: an operator's
// out, a probation the maintainer is counting, or a removal the map
// remembers. A member out or on probation is placed on at once; a node
// removed and not seen since is FORGOTTEN, so it joins — placeable — the next
// time it is seen present and healthy, rather than after it has proven itself
// stable: the operator vouching for it in place of the ticks. Forgetting it
// changes no member, only the state.
//
// PUTTING BACK A MEMBER ALREADY PLACED ON CHANGES NOTHING: the answer is the
// state and draw it was given, so a re-sent `in` writes nothing.
func In(s State, d placement.Draw, node string) (State, placement.Draw, error) {
	member, isMember := d.Member(node)
	_, removed := s.Removed[node]
	switch {
	case !isMember && !removed:
		return s, d, fmt.Errorf("%w: %q", ErrUnknownMember, node)
	case isMember && member.Placeable():
		return s, d, nil
	}
	next := s.Clone()
	delete(next.Removed, node)
	delete(next.TakenOut, node)
	next.tidy()
	if !isMember {
		return next, d, nil
	}
	return next, setMember(d, node, func(m *placement.Member) { m.Out, m.Probation = false, false }), nil
}

// absentNow reports whether the latest tick counted node absent: its run is
// open AND no tick has seen it present since the one that last counted it.
//
// AN OPEN RUN IS NOT AN ABSENCE. A member back from one missed tick keeps its
// run open for a whole [StableTicks] while it proves itself stable — placed
// on, holding its lease, serving everything — so reading the run alone as
// "gone" called a member absent for up to ten minutes after it had returned,
// and refused taking out a second member beside it for all of that time. The
// tick that counts a member absent zeroes [Absence.Present], and the first
// that sees it back raises it, so Present is exactly the latest tick's
// verdict.
func absentNow(s State, node string) bool {
	run, open := s.Absence[node]
	return open && run.Present == 0
}

// setMember is d with one member changed, on members of its own — never
// writing into the slice a caller still holds.
func setMember(d placement.Draw, node string, change func(*placement.Member)) placement.Draw {
	d.Members = slices.Clone(d.Members)
	for i := range d.Members {
		if d.Members[i].Node == node {
			change(&d.Members[i])
		}
	}
	return d
}

// HoldFor holds the map for d FROM NOW: no member is removed however long it
// is gone, until the hold expires or is released — while its absence keeps
// being counted, so a member still gone when the hold ends is removed on the
// next tick. It changes no member. A member on probation is the exception
// ([Hold]): it has no share for the hold to keep, and leaves the map the tick
// it is not seen.
//
// IT ALWAYS WRITES A NEW HOLD, ending d after the call — replacing any hold
// already placed, and its who, why and when with it. So a hold sent again
// EXTENDS the one in force to d past the resend: an operator retrying a hold
// whose answer was lost holds the map until d after the retry, not d after
// the first send. That is deliberate rather than an accident of a retry: a
// hold is the operator stating, now, how much longer the maintenance needs,
// and the newest statement is the one that should stand — a shorter one
// included, which ends the hold sooner. Unlike [Out] and [In], a repeat is
// therefore never a no-op, and a surface telling an operator a re-send is
// harmless must say that it restarts the hold.
func HoldFor(s State, d time.Duration, by, reason string, now time.Time) (State, error) {
	if d <= 0 || d > MaxHold {
		return s, fmt.Errorf("%w: asked for %s", ErrHoldRange, d)
	}
	now = now.UTC()
	next := s.Clone()
	next.Hold = &Hold{Until: now.Add(d), By: by, Reason: reason, At: now}
	return next, nil
}

// Release ends a hold, if there is one. It changes no member, and a state
// with no hold is answered as it was given, so a re-sent release writes
// nothing.
func Release(s State) State {
	if s.Hold == nil {
		return s
	}
	next := s.Clone()
	next.Hold = nil
	return next
}
