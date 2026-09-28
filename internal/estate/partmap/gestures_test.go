package partmap

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY GESTURE ON A FLEET WITH NO MAP IS REFUSED BY NAME — which is every
// fleet whose estate is still whole on every data node — rather than answered
// with a map nobody wrote.
func TestAGestureWithNoMapIsRefused(t *testing.T) {
	t.Parallel()
	p := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}
	for name, gesture := range map[string]func() error{
		"out":    func() error { _, err := Out(MapState{}, "data-00", "op", "", base); return err },
		"in":     func() error { _, err := In(MapState{}, "data-00"); return err },
		"hold":   func() error { _, err := HoldFor(MapState{}, time.Hour, "op", "", base); return err },
		"move":   func() error { _, err := Move(MapState{}, p, "data-00", "op", "", base); return err },
		"cancel": func() error { _, err := CancelMove(MapState{}, p, "data-00"); return err },
	} {
		if err := gesture(); !errors.Is(err, ErrNoMap) {
			t.Errorf("%s with no map: %v, want ErrNoMap", name, err)
		}
	}
}

// TAKING A MEMBER OUT MOVES WHAT IT HOLDS WHILE IT SERVES: every target stops
// naming it, the shares are balanced again, and the epoch stays where it is —
// the holders follow on the maintainer's ticks, each partition released only
// once its new target serves it. Sent again, it writes nothing; and putting
// the member back undoes it. A membership refusal carries the map's name.
func TestTakingAMemberOutMovesItsPartitionsWhileItServes(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 2, nodeIDs(4)...)
	epoch := s.state.Map.Epoch
	next, err := Out(s.state, "data-01", "op", "disk swap", base)
	if err != nil {
		t.Fatal(err)
	}
	if next.Map.Epoch != epoch {
		t.Fatalf("taking a member out moved the epoch to %d", next.Map.Epoch)
	}
	for _, target := range next.Map.targets() {
		if slices.Contains(target, "data-01") {
			t.Fatalf("a target %v still names the member taken out", target)
		}
	}
	again, err := Out(next, "data-01", "someone else", "a retry", base.Add(time.Minute))
	if err != nil || !sameState(again, next) {
		t.Fatalf("taking out a member already out changed the record (%v)", err)
	}
	s.state = next
	s.settle(200)
	s.converged()
	if held := s.holding("data-01"); len(held) != 0 {
		t.Fatalf("the member taken out still holds %v", held)
	}

	back, err := In(s.state, "data-01")
	if err != nil {
		t.Fatal(err)
	}
	s.state = back
	s.settle(200)
	s.converged()
	if s.holding("data-01")[Serving] == 0 {
		t.Fatal("the member put back holds nothing")
	}

	if _, err := Out(s.state, "data-99", "op", "", base); !errors.Is(err, membership.ErrUnknownMember) {
		t.Fatalf("taking out a stranger: %v, want membership's refusal", err)
	}
}

// A HOLD KEEPS AN ABSENT MEMBER'S PARTITIONS WHERE THEY ARE for as long as it
// lasts — the gesture for planned maintenance — and a release ends it.
func TestAHoldKeepsAnAbsentMembersPartitions(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 2, nodeIDs(3)...)
	held, err := HoldFor(s.state, time.Hour, "op", "kernel upgrade", s.now)
	if err != nil {
		t.Fatal(err)
	}
	s.state = held
	s.nodes["data-02"].down = true
	for range membership.OutTicks + 5 {
		s.tick()
	}
	if !s.state.Map.Draw().Holds("data-02") {
		t.Fatal("a held map removed a member")
	}
	s.state = Release(s.state)
	if s.state.Hold != nil {
		t.Fatal("a release left the hold")
	}
	s.tick()
	if s.state.Map.Draw().Holds("data-02") {
		t.Fatal("a member gone past the grace was kept once the hold was released")
	}
	if _, err := HoldFor(s.state, 2*membership.MaxHold, "op", "", s.now); !errors.Is(err, membership.ErrHoldRange) {
		t.Fatalf("a hold past the most there is: %v", err)
	}
}

// A MOVE REBUILDS ONE PARTITION'S COPY ELSEWHERE AND THEN LETS IT GO: the
// partition's target skips the node, the maintainer moves its holders there
// make-before-break, and the node keeps every other partition it held. Sent
// again, it writes nothing; cancelled, the partition may return.
func TestAMoveRebuildsOnePartitionElsewhere(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 2, nodeIDs(4)...)
	_, p := s.targetedAt(s.state.Map.targets()[3]...)
	node := s.state.Map.Target(p)[0]
	others := s.holding(node)[Serving]

	next, err := Move(s.state, p, node, "op", "hot disk", base)
	if err != nil {
		t.Fatal(err)
	}
	if next.Map.Epoch != s.state.Map.Epoch {
		t.Fatalf("a move moved the epoch to %d", next.Map.Epoch)
	}
	again, err := Move(next, p, node, "someone else", "a retry", base.Add(time.Hour))
	if err != nil || !sameState(again, next) {
		t.Fatalf("a move sent again changed the record (%v)", err)
	}
	s.state = next
	s.settle(200)
	s.converged()
	g, _ := groupOf(s.state.Map.Layout, p)
	if holderOf(&s.state.Map.Partitions[g], node) != nil {
		t.Fatalf("%s still holds %s after the move settled", node, p)
	}
	if got := s.holding(node)[Serving]; got != others-1 {
		t.Fatalf("%s serves %d partitions after moving one of its %d", node, got, others)
	}

	cancelled, err := CancelMove(s.state, p, node)
	if err != nil {
		t.Fatal(err)
	}
	if len(cancelled.Map.Moves) != 0 {
		t.Fatalf("a cancelled move is still recorded: %v", cancelled.Map.Moves)
	}
	if again, err := CancelMove(cancelled, p, node); err != nil || !sameState(again, cancelled) {
		t.Fatalf("cancelling a move that does not exist changed the record (%v)", err)
	}
}

// A MOVE IS REFUSED BY NAME where it cannot be done: a partition the layout
// does not have, a node that holds nothing of it, and a move that would leave
// the partition no member to be rebuilt on.
func TestAMoveThatCannotBeDoneIsRefused(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 1, "data-00", "data-01")
	g, p := s.targetedAt("data-00")
	if _, err := Move(s.state, statelog.PartitionID{Space: statelog.SpaceTracker, Index: 99},
		"data-00", "op", "", base); !errors.Is(err, ErrUnknownPartition) {
		t.Errorf("moving a partition the layout does not have: %v", err)
	}
	if _, err := CancelMove(s.state, statelog.PartitionID{Space: statelog.SpaceTracker, Index: 99},
		"data-00"); !errors.Is(err, ErrUnknownPartition) {
		t.Errorf("cancelling a move of a partition the layout does not have: %v", err)
	}
	if _, err := Move(s.state, p, "data-01", "op", "", base); !errors.Is(err, ErrNotAHolder) {
		t.Errorf("moving %s off a node that does not hold it: %v", p, err)
	}

	// Both hold it, and it is moved off one: moving it off the other too
	// would leave it nowhere.
	s.state.Map.Partitions[g].Holders = []Holder{
		{Node: "data-00", State: Serving, Since: 1}, {Node: "data-01", State: Serving, Since: 1}}
	next, err := Move(s.state, p, "data-00", "op", "", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Move(next, p, "data-01", "op", "", base); !errors.Is(err, ErrNowhereToMove) {
		t.Fatalf("moving %s off its last member: %v", p, err)
	}
}

// A GESTURE THAT CHANGES WHAT THE MAP PLACES IS BALANCED IN THE SAME WRITE:
// the members taking a departing member's share hold it in proportion to their
// weights, to the tolerance the layout promises, from the moment the gesture
// lands — rather than as straw2 alone would deal it until some later change.
func TestAGestureThatChangesPlacementIsBalanced(t *testing.T) {
	t.Parallel()
	first, _ := Next(MapState{}, Input{Layout: ownerLayout,
		Live: newSim(t, ownerLayout, 3, nodeIDs(8)...).live(), Company: company(3, "", 1), Now: base})
	next, err := Out(first, "data-03", "op", "", base)
	if err != nil {
		t.Fatal(err)
	}
	dev := next.Map.Draw().Layout().Deviation()
	if !next.Balance.Converged || dev > next.Balance.Tolerance || next.Balance.Deviation != dev {
		t.Fatalf("after taking a member out the layout is %.4f off against a tolerance of "+
			"%.4f, and the record says %+v", dev, next.Balance.Tolerance, next.Balance)
	}
}
