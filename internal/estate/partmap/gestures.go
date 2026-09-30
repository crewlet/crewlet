package partmap

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The gestures' own refusals. Every other refusal is membership's —
// [membership.ErrUnknownMember], [membership.ErrRemovedMember],
// [membership.ErrNothingPlaceable], [membership.ErrNowhereToRebuild] and
// [membership.ErrHoldRange] — wrapped in this map's name ([refused]), so the
// detail every surface shows says which map refused.
var (
	// ErrNoMap is a gesture on a fleet that has no estate map.
	//
	// IT SAYS THAT THERE IS NONE AND NOTHING ABOUT WHETHER ONE IS COMING,
	// because that depends on the layout, and the two answers send an
	// operator in opposite directions: at layout 0 there never is one —
	// every data node holds the whole estate ([WholeEstate]) — and at a
	// partitioned layout none has been written yet ([Unplaced]). Whoever
	// refuses a gesture with it says which, after it; a "yet" here put a
	// wait in front of layout 0's fact.
	ErrNoMap = errors.New("estate/partmap: there is no estate map")

	// ErrUnknownPartition is a move naming a partition the map's layout
	// does not have.
	ErrUnknownPartition = errors.New("estate/partmap: the estate map's layout has no such partition")

	// ErrNotAHolder is moving a partition off a node that does not hold it
	// in any state: there is no copy there to move.
	ErrNotAHolder = errors.New("estate/partmap: the node does not hold the partition")

	// ErrNowhereToMove is a move with no member to rebuild the copy on:
	// every member the partition could be placed on without the node
	// already holds a copy of it, so taken, the move would DROP the copy —
	// the partition one copy short for as long as the move stood — rather
	// than move it.
	ErrNowhereToMove = errors.New("estate/partmap: there is no member to rebuild " +
		"the partition's copy on")
)

// refused is a membership refusal as this map answers it: named for the map,
// and — for the one refusal whose remedy is the map's own
// ([membership.ErrNowhereToRebuild]) — saying what to change, in the words
// [Move]'s refusal uses for the same want of a member.
func refused(err error) error {
	if errors.Is(err, membership.ErrNowhereToRebuild) {
		return fmt.Errorf("estate/partmap: %w — %s", err, fewerCopies("take it out"))
	}
	return fmt.Errorf("estate/partmap: %w", err)
}

// fewerCopies is the remedy for a gesture with no member to rebuild a copy
// on, which then makes the gesture.
//
// THE MAP'S COUNT, NOT THE CONFIGURATION'S, is what the gesture is judged
// by, and the map takes a lowered count only when its duty stamps it, on the
// duty's next tick after the configuration is activated ([membership.Tick]).
// So the remedy says to wait for the map to show it: an operator who lowered
// estate.replicas and repeated the gesture at once was refused again and told
// to lower what they already had.
func fewerCopies(then string) string {
	return "add a data node first, or lower estate.replicas if the company means " +
		"to keep fewer, and " + then + " once the estate map shows the lower count " +
		"— the map takes it on its duty's next tick after the activation"
}

// Out takes a member out of the estate map ([membership.Out]): every
// partition's target stops naming it, so what it holds is rebuilt on the
// others while it keeps serving, and then released — each partition under the
// same two conditions as any leave. The shares are balanced again. Taking out a
// member already out answers the record it was given.
//
// On a fleet with no member to spare — as many members as copies — there is
// nowhere to rebuild, and the out is REFUSED
// ([membership.ErrNowhereToRebuild]) rather than taken as every partition
// keeping one copy fewer: an out, like a [Move], moves copies and never drops
// one, and that is membership's rule for both maps (ADR-0008). A fleet
// shrinks by the company lowering estate.replicas first. A move standing on
// the map cannot make an out the membership rule admitted drop a copy either:
// once the members could not do without the node it was moved off, the move
// waits and that node holds the partition again ([Map.MoveWaiting]).
//
// NO EPOCH MOVES: the epoch counts the holder table, and a gesture changes only
// the targets — the maintainer's next tick moves the holders toward them.
//
// Pure over the record: the caller reads it, applies this and writes the
// result with a compare-and-set.
func Out(state MapState, node, by, reason string, now time.Time) (MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	return gesture(state, func(s membership.State, d placement.Draw) (membership.State, placement.Draw, error) {
		return membership.Out(s, d, node, by, reason, now)
	})
}

// Bar bars a node from the estate map ([membership.Bar]) — an eviction's part in
// the map: a member is taken out as [Out] takes one, and the bar is recorded
// whether or not the map holds the node, and lasts through its removal and
// its return until [In] lifts it — so the maintainer never places a partition
// on an evicted node that comes back while its logs still gate it. Barring a
// node already barred answers the record it was given.
//
// NO EPOCH MOVES, as with every membership gesture.
func Bar(state MapState, node, by, reason string, now time.Time) (MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	return gesture(state, func(s membership.State, d placement.Draw) (membership.State, placement.Draw, error) {
		return membership.Bar(s, d, node, by, reason, now)
	})
}

// In puts a member back ([membership.In]): the targets name it again, and the
// shares are balanced again. A node removed and not seen since is forgotten,
// and a bar on a node the map does not hold is lifted, neither of which
// changes a target. Putting back a member already placed on answers the record
// it was given.
func In(state MapState, node string) (MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	return gesture(state, func(s membership.State, d placement.Draw) (membership.State, placement.Draw, error) {
		return membership.In(s, d, node)
	})
}

// gesture applies a membership gesture to the record, and balances the shares
// if it changed what the map places. One that changed nothing at all answers
// the record as it was given.
func gesture(state MapState,
	change func(membership.State, placement.Draw) (membership.State, placement.Draw, error)) (MapState, error) {

	s, d, err := change(state.State, state.Map.Draw())
	if err != nil {
		return state, refused(err)
	}
	next := state.Clone()
	next.State = s
	next.Map.Members = d.Members
	if !samePlacement(state.Map, next.Map) {
		next.Balance = rebalance(&next.Map)
	}
	return next, nil
}

// HoldFor holds the map for d FROM NOW ([membership.HoldFor]): no member is
// removed however long it is gone, until the hold expires or is released, so
// a node restarted for maintenance keeps its partitions. It changes no target.
// A hold sent again EXTENDS the one in force to d past the resend.
func HoldFor(state MapState, d time.Duration, by, reason string, now time.Time) (MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	s, err := membership.HoldFor(state.State, d, by, reason, now)
	if err != nil {
		return state, refused(err)
	}
	next := state.Clone()
	next.State = s
	return next, nil
}

// Release ends a hold, if there is one ([membership.Release]). A map with no
// hold is answered as it was given; a fleet with no map at all is refused like
// every other gesture, rather than answered with a record nobody wrote.
func Release(state MapState) (MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	next := state.Clone()
	next.State = membership.Release(state.State)
	return next, nil
}

// Move moves one partition off one node: the partition's target skips the
// node, so its copy there is rebuilt on another member — the one the
// partition's ranking offers next, within its failure domains — and then
// released under the two conditions every leave waits for. The move lasts
// until [CancelMove] or the node leaves the map — and it is in effect only
// while the other members can hold the partition's copies without the node:
// should members leave after it, the node holds the partition again and the
// move waits for one to return ([Map.MoveWaiting]), since a move moves a copy
// and never drops one. Moving a partition off a node it has already been moved
// off answers the record it was given, the first gesture's who and why kept.
//
// It refuses a partition the layout does not have, a node that does not hold
// the partition in any state, and a move with NO MEMBER TO REBUILD THE COPY ON
// — where the partition's target without the node would be smaller than with
// it, as on a fleet with exactly as many members as copies. That move would
// drop a copy rather than move it, which only lowering the company's copies
// does — taking a member out moves every copy it holds, or is refused
// ([membership.ErrNowhereToRebuild]) exactly as this is — and a gesture that
// says it moves a copy must not be the way to drop one. Like the membership
// gestures it changes the targets alone, and moves no epoch.
func Move(state MapState, p statelog.PartitionID, node, by, reason string, now time.Time) (MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	_, table, ok := state.Map.table(p)
	if !ok {
		return state, fmt.Errorf("%w: %q", ErrUnknownPartition, p.String())
	}
	if _, moved := state.Map.Moves[p.String()][node]; moved {
		return state, nil
	}
	if holderOf(table, node) == nil {
		return state, fmt.Errorf("%w: %s on %q", ErrNotAHolder, p, node)
	}
	next := state.Clone()
	if next.Map.Moves == nil {
		next.Map.Moves = map[string]map[string]membership.Gesture{}
	}
	if next.Map.Moves[p.String()] == nil {
		next.Map.Moves[p.String()] = map[string]membership.Gesture{}
	}
	next.Map.Moves[p.String()][node] = membership.Gesture{By: by, Reason: reason, At: now.UTC()}
	// AGAINST EVERY MOVE, never the target's own draw: that one keeps a moved
	// node where the members could not do without it, so it always has the
	// copies and would accept every move — including the one that has
	// nowhere to go.
	if moved, copies := next.Map.drawWithoutEveryMove(p).Size(), state.Map.Size(); moved < copies {
		return state, fmt.Errorf("%w: %s off %q: without %q only %d of its %d copies would "+
			"have a member to hold them, at %s — %s", ErrNowhereToMove, p, node, node, moved,
			copies, state.Config.Copies(), fewerCopies("move it"))
	}
	return next, nil
}

// CancelMove lifts a move ([Move]): the partition's target may name the node
// again. Cancelling a move that does not exist answers the record it was
// given, so a re-sent cancel writes nothing; a partition the layout does not
// have is refused, since that is a gesture naming nothing.
func CancelMove(state MapState, p statelog.PartitionID, node string) (MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	if _, _, ok := state.Map.table(p); !ok {
		return state, fmt.Errorf("%w: %q", ErrUnknownPartition, p.String())
	}
	if _, moved := state.Map.Moves[p.String()][node]; !moved {
		return state, nil
	}
	next := state.Clone()
	delete(next.Map.Moves[p.String()], node)
	if len(next.Map.Moves[p.String()]) == 0 {
		delete(next.Map.Moves, p.String())
	}
	if len(next.Map.Moves) == 0 {
		next.Map.Moves = nil
	}
	return next, nil
}
