package partmap

import (
	"errors"
	"slices"
	"strings"
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
		"out":     func() error { _, err := Out(MapState{}, "data-00", "op", "", base); return err },
		"bar":     func() error { _, err := Bar(MapState{}, "data-00", "op", "", base); return err },
		"in":      func() error { _, err := In(MapState{}, "data-00"); return err },
		"hold":    func() error { _, err := HoldFor(MapState{}, time.Hour, "op", "", base); return err },
		"release": func() error { _, err := Release(MapState{}); return err },
		"move":    func() error { _, err := Move(MapState{}, p, "data-00", "op", "", base); return err },
		"cancel":  func() error { _, err := CancelMove(MapState{}, p, "data-00"); return err },
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

// AN EVICTED NODE THAT COMES BACK IS PLACED ON NOTHING UNTIL IT IS PUT BACK.
//
// The eviction bars it: its partitions are rebuilt on the others, it is
// removed for its absence, and when the machine returns under its old id — and
// stays for far longer than any probation — no partition is placed on it,
// because every log it would serve still gates it as evicted. Putting it back
// is the one thing that places on it again.
func TestAnEvictedNodeThatComesBackIsPlacedOnNothing(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 2, nodeIDs(4)...)
	next, err := Bar(s.state, "data-01", "op", "evicted", base)
	if err != nil {
		t.Fatal(err)
	}
	s.state = next
	s.nodes["data-01"].down = true
	for range 2*membership.OutTicks + 5 {
		s.tick()
	}
	if s.state.Map.Draw().Holds("data-01") {
		t.Fatal("the premise: a node gone for two graces is still a member")
	}
	s.nodes["data-01"].down = false
	s.settle(4 * membership.StableTicks)
	s.converged()
	if held := s.holding("data-01"); len(held) != 0 {
		t.Fatalf("an evicted node back for %d ticks holds %v", 4*membership.StableTicks, held)
	}

	back, err := In(s.state, "data-01")
	if err != nil {
		t.Fatal(err)
	}
	s.state = back
	s.settle(200)
	s.converged()
	if s.holding("data-01")[Serving] == 0 {
		t.Fatal("the node put back holds nothing")
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
	if s.state, err = Release(s.state); err != nil {
		t.Fatal(err)
	}
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

// A MOVE THAT WOULD DROP A COPY RATHER THAN MOVE IT IS REFUSED. On a fleet with
// no member to spare — three data nodes at three copies, the default shape —
// every member already holds every partition, so moving one off a node has
// nowhere to rebuild the copy: taken, the partition would keep one copy fewer
// for as long as the move stood, which is not what a move is. The refusal
// says what to do instead, and the record is left as it was.
func TestAMoveThatWouldDropACopyIsRefused(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 3, nodeIDs(3)...)
	_, p := s.targetedAt(s.state.Map.targets()[0]...)
	_, err := Move(s.state, p, "data-00", "op", "hot disk", base)
	if !errors.Is(err, ErrNowhereToMove) {
		t.Fatalf("moving %s off one of the three members holding its three copies: %v, "+
			"want ErrNowhereToMove (target %v)", p, err, s.state.Map.Target(p))
	}
	// And that a lowered count counts once the MAP shows it, since the move
	// is judged by the map's count and the duty stamps a new one on a tick.
	for _, want := range []string{p.String(), "data-00", "estate.replicas",
		s.state.Config.Copies(), "once the estate map shows the lower count"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}

	// A fourth member is somewhere to rebuild it.
	s.add("data-03", 1, nil)
	s.settle(200)
	if _, err := Move(s.state, p, s.state.Map.Target(p)[0], "op", "hot disk", base); err != nil {
		t.Fatalf("moving %s with a member to spare: %v", p, err)
	}
}

// AN OUT THAT WOULD DROP A COPY RATHER THAN MOVE IT IS REFUSED, for a move's
// reason and by membership's rule for both maps: on a fleet with no member to
// spare every member holds every partition, so taking one out has nowhere to
// rebuild what it holds, and every partition would keep one copy fewer once
// it was let go. The refusal names the field the company lowers to mean
// fewer, the record is left as it was, and a fourth member makes room.
func TestAnOutThatWouldDropACopyIsRefused(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 3, nodeIDs(3)...)
	given, _ := s.state.Encode()
	answered, err := Out(s.state, "data-00", "op", "retiring", base)
	if !errors.Is(err, membership.ErrNowhereToRebuild) {
		t.Fatalf("taking out one of three members holding three copies: %v, "+
			"want membership.ErrNowhereToRebuild", err)
	}
	for _, want := range []string{"estate/partmap: ", "data-00", "estate.replicas",
		s.state.Config.Copies(), "once the estate map shows the lower count"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if got, _ := answered.Encode(); string(got) != string(given) {
		t.Errorf("a refused out answered\n%s\nfor\n%s", got, given)
	}

	s.add("data-03", 1, nil)
	s.settle(200)
	if _, err := Out(s.state, "data-00", "op", "retiring", base); err != nil {
		t.Fatalf("taking a member out with one to spare: %v", err)
	}
}

// A MOVE MOVES A COPY AND NEVER DROPS ONE, WHATEVER HAPPENS AFTER IT. Taken
// while a member was spare, and then left with none to spare — the member it
// rebuilt the copy on taken out, or removed for being gone — the node it moved
// off holds the partition again, so the partition keeps the map's copies like
// every other one rather than one fewer while a member able to hold it sits
// idle. The move stays on the map, WAITING, and takes effect again once a
// member returns.
func TestAMoveWaitsWhileNoOtherMemberCanHoldTheCopy(t *testing.T) {
	t.Parallel()
	for name, leave := range map[string]func(s *sim, node string){
		"taken out": func(s *sim, node string) {
			next, err := Out(s.state, node, "op", "retiring", base)
			if err != nil {
				s.t.Fatal(err)
			}
			s.state = next
		},
		"removed for absence": func(s *sim, node string) {
			s.nodes[node].down = true
			for range membership.OutTicks + 1 {
				s.tick()
			}
			if s.state.Map.Draw().Holds(node) {
				s.t.Fatalf("%s is still a member after %d ticks gone", node, membership.OutTicks+1)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := settled(t, smallLayout, 3, nodeIDs(4)...)
			_, p := s.targetedAt(s.state.Map.targets()[0]...)
			moved := s.state.Map.Target(p)[0]
			next, err := Move(s.state, p, moved, "op", "hot disk", base)
			if err != nil {
				t.Fatal(err)
			}
			s.state = next
			s.settle(200)
			s.converged()
			if s.state.Map.MoveWaiting(p, moved) || slices.Contains(s.state.Map.Target(p), moved) {
				t.Fatalf("a move with a member to spare is not in effect: target %v",
					s.state.Map.Target(p))
			}

			leave(s, s.state.Map.Target(p)[0])
			everyTargetIsWhole(t, s.state.Map)
			if !slices.Contains(s.state.Map.Target(p), moved) || !s.state.Map.MoveWaiting(p, moved) {
				t.Fatalf("with no member to spare %s's target is %v, want %s back in it and the "+
					"move waiting", p, s.state.Map.Target(p), moved)
			}
			if _, recorded := s.state.Map.Moves[p.String()][moved]; !recorded {
				t.Fatal("the waiting move is no longer on the map")
			}
			s.settle(400)
			s.converged()
			for _, c := range s.state.Map.Coverage(s.live()) {
				if c.Short() {
					t.Errorf("%s settled short: %d of %d copies", c.Partition, len(c.Serving), c.Wanted)
				}
			}

			// A MEMBER RETURNS, and the move is in effect again.
			s.add("data-09", 1, nil)
			s.settle(400)
			s.converged()
			everyTargetIsWhole(t, s.state.Map)
			if s.state.Map.MoveWaiting(p, moved) || slices.Contains(s.state.Map.Target(p), moved) {
				t.Errorf("with a member back the move still waits: %s's target is %v", p,
					s.state.Map.Target(p))
			}
		})
	}
}

// everyTargetIsWhole fails unless every partition's target — moved or not —
// has as many nodes as the map places copies: what Coverage counts a partition
// short against.
func everyTargetIsWhole(t *testing.T, m Map) {
	t.Helper()
	for g, target := range m.targets() {
		id := m.Partitions[g].ID
		p, err := statelog.ParsePartitionID(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(target) != m.Size() || len(m.Target(p)) != m.Size() {
			t.Errorf("%s's target is %v, and the map places %d copies", id, target, m.Size())
		}
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
