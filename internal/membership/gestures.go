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

	// ErrNowhereToRebuild is taking out a member whose copies no other
	// member could take: the map places fewer copies without it than with
	// it — as on a fleet with exactly as many members as copies — so the
	// out would DROP a copy of everything it holds rather than move one,
	// and an out is the gesture that says it moves them.
	//
	// ITS REMEDY IS THE MAP'S, and the one refusal here a map extends: add
	// a data node, or lower the copies the company asks for — a field each
	// map names differently (`objects.replicas`, `estate.replicas`) and
	// every surface reads the same, so it belongs beside the map's name
	// rather than in each surface's hint alone.
	ErrNowhereToRebuild = errors.New("taking it out would leave no member to rebuild " +
		"its copies on")

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
// ([absentNow]), so a member back from a missed tick counts. It refuses a node
// the map removed and has not seen back ([ErrRemovedMember]): that one places
// nothing already.
//
// AND IT NEVER DROPS A COPY ([ErrNowhereToRebuild]): an out whose member the
// map could not do without — fewer copies placed without it than with it, as
// on a fleet with exactly as many members as copies — is refused, because the
// out would not move its copies but lose one of each, and the gesture's whole
// meaning is the move. A fleet shrinks by lowering the company's copies first,
// which is a decision about durability the company makes in its
// configuration, never a side effect of a gesture about one node. The
// question is the DRAW's, the one [placement.Draw.Size] answers, never who is
// present: a member absent now is still one the map places on, which rebuilds
// onto it when it returns, and whether it is gone for good is the tick's
// judgement to make ([OutTicks]), never a gesture's to anticipate. A member on
// probation or already out places nothing, so taking it out drops nothing.
//
// THE REFUSAL NAMES THE ACTIVATION THE MAP'S COPY COUNT CAME FROM
// ([ConfigSource.Copies]), because the count it judges is the one the map holds, and a
// company's lowered count reaches the map only when the map's duty stamps it
// on its next tick after the activation ([Tick]) — not when the configuration
// is applied. An operator who has just lowered the count and is refused again
// reads there that the map is still on the old one, rather than being told to
// lower what they already have.
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
	taken := setMember(d, node, func(m *placement.Member) { m.Out = true })
	if member.Placeable() {
		if lastPlaceable(s, d, node) {
			return s, d, fmt.Errorf("%w: %s", ErrNothingPlaceable, node)
		}
		// BEFORE THE STATE IS TOUCHED, and after the refusal above: a
		// fleet with no other present member is refused for the harder
		// reason, since there every write has nowhere to land at all.
		if without, with := taken.Size(), d.Size(); without < with {
			return s, d, fmt.Errorf("%w: without it only %d of the %d copies the map "+
				"places would have a member to hold them, at %s: %s", ErrNowhereToRebuild,
				without, with, s.Config.Copies(), node)
		}
	}
	next := s.Clone()
	if next.TakenOut == nil {
		next.TakenOut = map[string]Gesture{}
	}
	next.TakenOut[node] = Gesture{By: by, Reason: reason, At: now.UTC()}
	return next, taken, nil
}

// lastPlaceable reports whether node is the last member present to place
// copies on — which [Out] and [Bar] refuse to take away. Present is what the
// latest tick saw ([absentNow]), so a member back from a missed tick counts.
func lastPlaceable(s State, d placement.Draw, node string) bool {
	for _, m := range d.Placeable() {
		if m.Node != node && !absentNow(s, m.Node) {
			return false
		}
	}
	return true
}

// Bar bars a node from the map: it is placed on nothing — a member now is taken
// out, its share moving to the others — and stays so WHATEVER BECOMES OF ITS
// MEMBERSHIP, removed for absence, forgotten, seen back, until [In] lifts the
// bar ([State.Barred]). It is how a map records an EVICTION: the operator's
// judgement that the machine is gone, whose copies are fenced off until it is
// readmitted.
//
// A NODE THE MAP DOES NOT HOLD IS BARRED ALL THE SAME — one it removed for
// absence, or one it has never seen: a bar is about a machine that may come
// back, and a node an operator evicts is usually one the map has already let
// go. Where [Out] answers such a node that it places nothing to take out, the
// bar is exactly what must still be written.
//
// BARRING A NODE ALREADY BARRED CHANGES NOTHING, the first gesture's who, why
// and when included, so a re-sent gesture writes nothing — [Out]'s rule. And
// like [Out] it refuses to take out the last member present to place copies
// on ([ErrNothingPlaceable]).
//
// UNLIKE [Out], IT IS NOT REFUSED FOR THE COPIES IT LEAVES NOWHERE TO GO
// ([ErrNowhereToRebuild]). An out moves the copies of a node that still
// serves them, so refusing one that has nowhere to rebuild them keeps those
// copies; a bar records a machine the operator has judged gone, whose copies
// are no longer there to keep — refused, the map would go on placing copies on
// it, and membership's own removal for absence drops the same placements
// anyway.
func Bar(s State, d placement.Draw, node, by, reason string, now time.Time) (State, placement.Draw, error) {
	if _, barred := s.Barred[node]; barred {
		return s, d, nil
	}
	member, isMember := d.Member(node)
	if isMember && member.Placeable() && lastPlaceable(s, d, node) {
		return s, d, fmt.Errorf("%w: %s", ErrNothingPlaceable, node)
	}
	next := s.Clone()
	if next.Barred == nil {
		next.Barred = map[string]Gesture{}
	}
	next.Barred[node] = Gesture{By: by, Reason: reason, At: now.UTC()}
	if !isMember || member.Out {
		return next, d, nil
	}
	return next, setMember(d, node, func(m *placement.Member) { m.Out = true }), nil
}

// In puts a member back: the map places on it again, and its share moves back.
//
// IT VOUCHES FOR THE NODE, whatever is keeping it off the map: an operator's
// out, a bar ([Bar]), a probation the maintainer is counting, or a removal the
// map remembers. A member out, barred or on probation is placed on at once; a
// node removed and not seen since is FORGOTTEN, so it joins — placeable — the
// next time it is seen present and healthy, rather than after it has proven
// itself stable: the operator vouching for it in place of the ticks; and a bar
// on a node the map does not hold is lifted, so it joins as any node would.
// Forgetting it and lifting a bar change no member, only the state.
//
// PUTTING BACK A MEMBER ALREADY PLACED ON CHANGES NOTHING: the answer is the
// state and draw it was given, so a re-sent `in` writes nothing.
func In(s State, d placement.Draw, node string) (State, placement.Draw, error) {
	member, isMember := d.Member(node)
	_, removed := s.Removed[node]
	_, barred := s.Barred[node]
	switch {
	case !isMember && !removed && !barred:
		return s, d, fmt.Errorf("%w: %q", ErrUnknownMember, node)
	case isMember && member.Placeable() && !barred:
		return s, d, nil
	}
	next := s.Clone()
	delete(next.Removed, node)
	delete(next.TakenOut, node)
	delete(next.Barred, node)
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
