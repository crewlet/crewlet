package partmap

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// settled is a fleet of these nodes over layout at replicas copies, run until
// its map has settled.
func settled(t *testing.T, layout statelog.Layout, replicas int, nodes ...string) *sim {
	t.Helper()
	s := newSim(t, layout, replicas, nodes...)
	s.settle(200)
	s.converged()
	return s
}

// targetedAt is the group and id of the first partition whose target is
// exactly want.
func (s *sim) targetedAt(want ...string) (int, statelog.PartitionID) {
	s.t.Helper()
	for g, target := range s.state.Map.targets() {
		if slices.Equal(target, want) {
			p, err := statelog.ParsePartitionID(s.state.Map.Partitions[g].ID)
			if err != nil {
				s.t.Fatal(err)
			}
			return g, p
		}
	}
	s.t.Fatalf("no partition's target is %v", want)
	return 0, statelog.PartitionID{}
}

// holder is node's holder entry in partition g, failing when it has none.
func (s *sim) holder(g int, node string) Holder {
	s.t.Helper()
	h := holderOf(&s.state.Map.Partitions[g], node)
	if h == nil {
		s.t.Fatalf("%s holds nothing of %s", node, s.state.Map.Partitions[g].ID)
	}
	return *h
}

// A FIRST MAP IS WRITTEN ONLY FROM A COMPANY, AT A PARTITIONED LAYOUT, FOR A
// FLEET WITH A MEMBER: without the company it would invent the copies every
// partition keeps, layout 0 is placed by no map, and a map with no member says
// what none says at a write per tick. With all three it is epoch 1 of a new
// generation, every partition's table present and its target joining.
func TestAFirstMapNeedsACompanyAPartitionedLayoutAndAMember(t *testing.T) {
	t.Parallel()
	live := []Presence{{Node: "data-00", Meta: Meta{Weight: 1, Layout: layoutNo(1), Healthy: yes()}}}
	zero := statelog.Layout{Number: 0, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceEstate, Partitions: 1, Domains: []string{"tracker"}}}}
	for name, in := range map[string]Input{
		"no company": {Layout: smallLayout, Live: live, Now: base},
		"layout 0":   {Layout: zero, Live: live, Company: company(3, "", 1), Now: base},
		"no member":  {Layout: smallLayout, Company: company(3, "", 1), Now: base},
		"no layout":  {Live: live, Company: company(3, "", 1), Now: base},
	} {
		if next, changed := Next(MapState{}, in); changed || next.Map.Generation != uuid.Nil {
			t.Errorf("%s: a first map was written: %+v", name, next.Map)
		}
	}

	next, changed := Next(MapState{}, Input{Layout: smallLayout, Live: live,
		Company: company(3, "", 7), Now: base})
	if !changed {
		t.Fatal("a company, a layout and a member wrote no first map")
	}
	m := next.Map
	if m.Generation == uuid.Nil || m.Epoch != 1 || m.Replicas != 3 || next.Config.Epoch != 7 {
		t.Fatalf("the first map is generation %s epoch %d replicas %d from activation %d",
			m.Generation, m.Epoch, m.Replicas, next.Config.Epoch)
	}
	if err := next.Validate(); err != nil {
		t.Fatalf("the first map does not validate: %v", err)
	}
	for _, table := range m.Partitions {
		if !slices.Equal(table.Holders, []Holder{{Node: "data-00", State: Joining, Since: 1}}) {
			t.Errorf("%s's first holders are %+v, want its one member joining at 1",
				table.ID, table.Holders)
		}
	}
}

// A NEW DEPLOYMENT JOINS EVERY PARTITION AT ONCE. Nobody serves any partition
// of a new map, so no join is a transfer and none is rationed: the first tick
// names every copy of every partition, 963 at the owner's layout over three
// nodes, where a ration of one would have taken 321 joins in a row per node.
// And then it converges on the target, and stays there.
func TestANewDeploymentJoinsEveryPartitionAtOnce(t *testing.T) {
	t.Parallel()
	s := newSim(t, ownerLayout, 3, nodeIDs(3)...)
	s.tick()
	joining := 0
	for _, table := range s.state.Map.Partitions {
		for _, h := range table.Holders {
			if h.State == Joining {
				joining++
			}
		}
	}
	if joining != 321*3 {
		t.Fatalf("the first tick named %d joins, want every copy of every partition, %d",
			joining, 321*3)
	}
	s.act()
	rounds := s.settle(50)
	s.converged()
	epoch := s.state.Map.Epoch
	if s.tick() {
		t.Fatal("a settled map changed with nothing happening")
	}
	t.Logf("settled in %d more rounds at epoch %d", rounds, epoch)
}

// THE MAP CONVERGES ON ITS TARGET as the fleet grows, one node at a time, and
// every partition stays served the whole way: a copy is let go only once its
// target serves. A join into a served partition is a transfer, and no node is
// ever in more than MaxJoinsPerNode of them at once.
func TestTheMapConvergesAsTheFleetGrows(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 3, nodeIDs(3)...)
	for _, node := range []string{"data-03", "data-04", "data-05"} {
		s.add(node, 1, nil)
		for round := 0; ; round++ {
			if round > 200 {
				t.Fatalf("%s's arrival had not settled after 200 rounds", node)
			}
			changed := s.tick()
			s.check()
			transfers := map[string]int{}
			for _, table := range s.state.Map.Partitions {
				if servers(&table) == 0 {
					continue
				}
				for _, h := range table.Holders {
					if h.State == Joining {
						transfers[h.Node]++
					}
				}
			}
			for n, count := range transfers {
				if count > MaxJoinsPerNode {
					t.Fatalf("after tick %d %s is in %d transfers at once, want at most %d",
						s.ticks, n, count, MaxJoinsPerNode)
				}
			}
			if acted := s.act(); !changed && !acted {
				break
			}
		}
		s.converged()
		if held := s.holding(node)[Serving]; held == 0 {
			t.Fatalf("%s joined a six-member fleet and holds nothing", node)
		}
	}
}

// THE JOIN RATION COSTS A TICK PER TRANSFER, AND NO MORE — the figures
// MaxJoinsPerNode's doc states. A join into a served partition is named on
// one tick and its node's next one on the tick that promotes it, so with an
// executor that finishes every join within the tick a node's transfers take
// one tick each: a fourth data node joining three at the owner's layout, and
// the survivors of four re-replicating what the lost one held once
// membership removes it. Measured here rather than asserted from the doc, and
// bounded so a change that made a transfer cost two ticks fails rather than
// doubling a figure nobody re-reads.
func TestTheJoinRationCostsATickPerTransfer(t *testing.T) {
	t.Parallel()
	grow := newSim(t, ownerLayout, 3, nodeIDs(3)...)
	grow.settleAtOnce(100)
	grow.add("data-03", 1, nil)
	ticks := grow.settleAtOnce(1000)
	held := grow.holding("data-03")[Serving]
	t.Logf("a fourth data node takes %d copies in %d ticks (%v at the %v tick)", held, ticks,
		time.Duration(ticks)*membership.TickInterval, membership.TickInterval)
	if held == 0 || ticks > held+4 {
		t.Fatalf("a fourth data node took %d copies in %d ticks, want at most one tick per "+
			"transfer and four to start and finish", held, ticks)
	}

	lose := newSim(t, ownerLayout, 3, nodeIDs(4)...)
	lose.settleAtOnce(100)
	lose.check = lose.checkCopies
	survivors := nodeIDs(4)[1:]
	before := map[string]int{}
	for _, node := range survivors {
		before[node] = lose.holding(node)[Serving]
	}
	lose.nodes["data-00"].down = true
	ticks = lose.settleAtOnce(1000) - membership.OutTicks
	most := 0
	for _, node := range survivors {
		most = max(most, lose.holding(node)[Serving]-before[node])
	}
	t.Logf("the survivors of four take up to %d copies each in %d ticks past the removal (%v)",
		most, ticks, time.Duration(ticks)*membership.TickInterval)
	if most == 0 || ticks > most+4 {
		t.Fatalf("restoring the lost node's copies took %d ticks, and no survivor took more "+
			"than %d transfers", ticks, most)
	}
}

// THE TWO LEAVE CONDITIONS. A copy the target no longer names is let go only
// when every node of the target serves the partition by the map's account AND
// by its own — (a) — and has itself acted on the map that made it a server:
// its lease names a map epoch at least its Since, in this map's generation —
// (b). Each case breaks one clause and the copy stays. A lease the tick counts
// unhealthy is no account at all: membership counts its node absent, and an
// absent node has no lease to vouch with, whatever that lease says it serves.
func TestAHolderLeavesOnlyUnderBothConditions(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		state      HolderState // the target's holder state in the map
		said       PartitionState
		epoch      uint64      // the map epoch the target's lease names
		otherGen   bool        // the lease names another generation
		down       bool        // the target has no live lease
		unhealthy  func(*Meta) // the target's lease, made one the tick counts unhealthy
		wantLeaves bool
	}{
		"both hold":                            {state: Serving, said: PartServing, epoch: 20, wantLeaves: true},
		"(a) the lease says catching up":       {state: Serving, said: PartCatchingUp, epoch: 20},
		"(a) the lease says faulted":           {state: Serving, said: PartFaulted, epoch: 20},
		"(a) the target has no live lease":     {state: Serving, said: PartServing, epoch: 20, down: true},
		"(a) the map lists the target joining": {state: Joining, said: PartServing, epoch: 20},
		"(a) the target's store has failed": {state: Serving, said: PartServing, epoch: 20,
			unhealthy: func(m *Meta) { m.Healthy, m.Detail = no(), "disk gone" }},
		"(a) the target does not say it is healthy": {state: Serving, said: PartServing, epoch: 20,
			unhealthy: func(m *Meta) { m.Healthy = nil }},
		"(b) the lease names an older epoch": {state: Serving, said: PartServing, epoch: 19},
		"(b) another generation's epoch":     {state: Serving, said: PartServing, epoch: 99, otherGen: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := settled(t, smallLayout, 1, "data-00", "data-01")
			g, _ := s.targetedAt("data-00")
			m := &s.state.Map
			m.Epoch = 20
			m.Partitions[g].Holders = []Holder{
				{Node: "data-00", State: c.state, Since: 20},
				{Node: "data-01", State: Serving, Since: 3},
			}
			id := m.Partitions[g].ID
			target, other := s.nodes["data-00"], s.nodes["data-01"]
			target.meta.Partitions[id] = c.said
			target.meta.MapGeneration, target.meta.MapEpoch = m.Generation, c.epoch
			if c.otherGen {
				target.meta.MapGeneration = uuid.New()
			}
			target.down = c.down
			if c.unhealthy != nil {
				c.unhealthy(&target.meta)
			}
			other.meta.Partitions[id] = PartServing
			other.meta.MapGeneration, other.meta.MapEpoch = m.Generation, 20

			s.tick()
			left := s.holder(g, "data-01").State == Leaving
			if left != c.wantLeaves {
				t.Fatalf("the copy the target does not name left = %v, want %v (map %+v)",
					left, c.wantLeaves, s.state.Map.Partitions[g].Holders)
			}
		})
	}
}

// TWO COPIES CANNOT VOUCH FOR EACH OTHER ON DIFFERENT MAPS — the hazard the
// second condition exists for. A partition held by A is moved to B; B joins
// and serves, and A is told to leave. Before A reads that, the operator moves
// the partition back: A, still serving, is wanted again and serves again,
// and B is not wanted. On the map A HOLDS, A is leaving and B serves — so A
// drains, as its own check against that map allows. Had the maintainer let B
// go on A's word that it serves, both would drain and nobody would hold the
// partition. It waits instead for A to have read the map that made it a server
// again — which A, draining, never does — so B keeps serving, A's own drain
// is recorded, and A joins again from B.
func TestTwoCopiesCannotVouchForEachOtherOnDifferentMaps(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 1, "data-a", "data-b")
	g, p := s.targetedAt("data-a")

	move := func(off, by string) {
		t.Helper()
		next, err := Move(s.state, p, off, by, "test", s.now)
		if err != nil {
			t.Fatalf("move %s off %s: %v", p, off, err)
		}
		s.state = next
	}
	cancel := func(off string) {
		t.Helper()
		next, err := CancelMove(s.state, p, off)
		if err != nil {
			t.Fatalf("cancel the move of %s off %s: %v", p, off, err)
		}
		s.state = next
	}

	move("data-a", "op")
	for round := 0; ; round++ {
		if round > 50 {
			t.Fatal("A was never told to leave")
		}
		s.tick()
		s.check()
		if s.holder(g, "data-a").State == Leaving {
			break
		}
		s.act()
	}
	// A has not read the map that told it to leave: it goes on acting on
	// the one before.
	behind := s.state.Map.Clone()
	behind.Epoch--
	for i, table := range behind.Partitions {
		for j, h := range table.Holders {
			if h.Node == "data-a" && i == g {
				behind.Partitions[i].Holders[j].State = Serving
			}
		}
	}
	s.nodes["data-a"].stale = &behind

	cancel("data-a")
	move("data-b", "op")
	s.tick()
	s.check()
	if got := s.holder(g, "data-a").State; got != Serving {
		t.Fatalf("A, still serving and wanted again, is %s", got)
	}
	if got := s.holder(g, "data-b").State; got != Serving {
		t.Fatalf("B was let go on the word of A, which has not read the map "+
			"that made it a server again: B is %s", got)
	}

	// Now A reads the map it missed: on it, A is leaving and B serves, so
	// A drains.
	missed := s.state.Map.Clone()
	for j, h := range missed.Partitions[g].Holders {
		switch h.Node {
		case "data-a":
			missed.Partitions[g].Holders[j].State = Leaving
		case "data-b":
			missed.Partitions[g].Holders[j].State = Serving
		}
	}
	missed.Epoch = s.state.Map.Epoch - 1
	s.nodes["data-a"].stale = &missed
	s.act()
	s.nodes["data-a"].stale = nil
	s.settle(100)
	s.converged()
	if got := s.state.Map.Target(p); !slices.Equal(got, []string{"data-a"}) {
		t.Fatalf("%s's target is %v, want A", p, got)
	}
}

// A JOIN IS NAMED ONLY ON A NODE THAT CAN TAKE ONE. A partition moved onto a
// node that is down, or whose lease the tick counts unhealthy, names no join
// on it while it stays that way: the join is one it cannot start, and the trim
// counts a joiner's tail from nothing, so a join named on a node that stayed
// away would stop the partition's logs trimming for as long as it did. The
// node stays the partition's target, so the copy it would replace is not let
// go in the meantime; and on the first tick it is back the join is named, and
// the partition moves. A join already named when its node went away is kept,
// for the trim's own reason: that joiner's tail is what must not be trimmed.
func TestAJoinIsNamedOnlyOnANodeThatCanTakeOne(t *testing.T) {
	t.Parallel()
	for name, away := range map[string]func(*simNode){
		"down":           func(n *simNode) { n.down = true },
		"failed":         func(n *simNode) { n.meta.Healthy, n.meta.Detail = no(), "disk gone" },
		"health unsaid":  func(n *simNode) { n.meta.Healthy = nil },
		"another layout": func(n *simNode) { n.meta.Layout = layoutNo(2) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := settled(t, smallLayout, 1, "data-a", "data-b")
			// B down serves nothing, and its copies are intact on its disk.
			s.check = s.checkCopies
			g, p := s.targetedAt("data-a")
			restore := s.nodes["data-b"].meta
			restore.Partitions = maps.Clone(restore.Partitions)
			away(s.nodes["data-b"])
			next, err := Move(s.state, p, "data-a", "op", "test", s.now)
			if err != nil {
				t.Fatal(err)
			}
			s.state = next
			if got := s.state.Map.Target(p); !slices.Equal(got, []string{"data-b"}) {
				t.Fatalf("%s's target is %v, want B", p, got)
			}
			for range 5 {
				s.tick()
				s.check()
				s.act()
				if h := holderOf(&s.state.Map.Partitions[g], "data-b"); h != nil {
					t.Fatalf("a join was named on B while it could not take one: %+v", *h)
				}
				if got := s.holder(g, "data-a").State; got != Serving {
					t.Fatalf("A's copy is %s while the target it would go to cannot join", got)
				}
			}

			s.nodes["data-b"].down, s.nodes["data-b"].meta = false, restore
			s.tick()
			if got := s.holder(g, "data-b"); got.State != Joining {
				t.Fatalf("B, back, is %s rather than joining", got.State)
			}
			// Named, and then away again: the join stays.
			away(s.nodes["data-b"])
			for range 3 {
				s.tick()
				if got := s.holder(g, "data-b").State; got != Joining {
					t.Fatalf("a join named before B went away is %s", got)
				}
			}
			s.nodes["data-b"].down, s.nodes["data-b"].meta = false, restore
			s.settle(50)
			s.converged()
		})
	}
}

// THE LAST SERVER IS NEVER LET GO, and an empty target vouches for nothing: a
// partition whose target names nobody keeps every copy it has rather than
// retiring them in favour of nobody.
//
// No gesture empties a target — taking out the last member that takes copies
// is refused, and a move whose node the others could not do without waits
// (TestAMoveWaitsWhileNoOtherMemberCanHoldTheCopy) — so the record is written
// here as a store might still hold it: every member out.
func TestTheLastServerIsNeverLetGo(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 1, "data-a", "data-b")
	g, p := s.targetedAt("data-a")
	id := s.state.Map.Partitions[g].ID

	// Both serve it, and then both members are out: its target is empty.
	s.state.Map.Partitions[g].Holders = []Holder{
		{Node: "data-a", State: Serving, Since: 1}, {Node: "data-b", State: Serving, Since: 1}}
	s.nodes["data-b"].meta.Partitions[id] = PartServing
	s.state.TakenOut = map[string]membership.Gesture{}
	for i := range s.state.Map.Members {
		s.state.Map.Members[i].Out = true
		s.state.TakenOut[s.state.Map.Members[i].Node] = membership.Gesture{By: "op", At: s.now}
	}
	if target := s.state.Map.Target(p); len(target) != 0 {
		t.Fatalf("%s's target is %v, want none", p, target)
	}
	for range 5 {
		s.tick()
		s.check()
		s.act()
	}
	for _, node := range []string{"data-a", "data-b"} {
		if got := s.holder(g, node).State; got != Serving {
			t.Fatalf("%s's copy on %s is %s with an empty target", p, node, got)
		}
	}
}

// THE LAST SERVER RESTARTING MID-MOVE SERVES AGAIN. A partition is moved off
// its one server, A, onto B, whose join has not finished. A shuts down and its
// lease says it is draining the partition: the map writes that down, and
// nobody serves it. A comes back, reads the map that says it is leaving, and
// its own check of the leave refuses it — B does not serve — so its lease says
// it serves, at that map's epoch. A copy that serves and that nobody else
// serves is the partition's only one, so the map lists it serving again,
// whether the target names it or not: left leaving, routers would have
// nowhere to send the partition while A answers for it, and B no donor to
// join from, for as long as A stayed up. Once B serves, A goes under the two
// conditions like any server the target does not name.
//
// Only at A's word having read the map that made it a leaver: a serving from
// before that is a node that has not seen its leave yet, and whose next word
// is the one to go by.
func TestTheLastServerRestartingMidMoveServesAgain(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		behind bool // A's serving names an epoch before its leave
		want   HolderState
	}{
		"having read its leave":    {want: Serving},
		"before reading its leave": {behind: true, want: Leaving},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := settled(t, smallLayout, 1, "data-a", "data-b")
			g, p := s.targetedAt("data-a")
			id := s.state.Map.Partitions[g].ID
			next, err := Move(s.state, p, "data-a", "op", "hot disk", s.now)
			if err != nil {
				t.Fatal(err)
			}
			s.state = next
			s.tick()
			if got := s.holder(g, "data-b").State; got != Joining {
				t.Fatalf("B is %s after the move, want joining", got)
			}

			a := s.nodes["data-a"]
			a.meta.Partitions[id] = PartDraining
			a.meta.MapGeneration, a.meta.MapEpoch = s.state.Map.Generation, s.state.Map.Epoch
			s.tick()
			if got := s.holder(g, "data-a"); got.State != Leaving {
				t.Fatalf("A, draining on its own, is %s", got.State)
			}
			leftAt := s.holder(g, "data-a").Since

			a.meta.Partitions[id] = PartServing
			a.meta.MapEpoch = leftAt
			if c.behind {
				a.meta.MapEpoch = leftAt - 1
			}
			for range 3 {
				s.tick()
			}
			if got := s.holder(g, "data-a").State; got != c.want {
				t.Fatalf("A, back and serving the partition nobody else serves, is %s, want %s "+
					"(serving %v)", got, c.want, s.state.Map.Serving(p))
			}
			if c.behind {
				return
			}
			if got := s.state.Map.Serving(p); !slices.Equal(got, []string{"data-a"}) {
				t.Fatalf("%s is served by %v, want A", p, got)
			}
			s.settle(50)
			s.converged()
			if got := s.state.Map.Serving(p); !slices.Equal(got, []string{"data-b"}) {
				t.Fatalf("%s is served by %v after the move settled, want B", p, got)
			}
		})
	}
}

// A LEAVER THAT STILL SERVES STAYS LEAVING WHILE ANOTHER COPY SERVES. Its own
// check of the leave may refuse it on a view a beat older than the map — so
// its lease says it serves, at the epoch that made it a leaver — but with the
// target serving, the partition has a server, and the leave stands: the node
// drains on its next check. Taken back, it would be retired again in the same
// tick and restamped, and the map would move its epoch every tick for as long
// as the node's view lagged.
func TestALeaverThatStillServesStaysLeavingWhileAnotherServes(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 1, "data-a", "data-b")
	g, p := s.targetedAt("data-a")
	next, err := Move(s.state, p, "data-a", "op", "hot disk", s.now)
	if err != nil {
		t.Fatal(err)
	}
	s.state = next
	for round := 0; ; round++ {
		if round > 50 {
			t.Fatal("A was never told to leave")
		}
		s.tick()
		if s.holder(g, "data-a").State == Leaving {
			break
		}
		s.act()
	}
	a := s.nodes["data-a"]
	a.meta.MapGeneration, a.meta.MapEpoch = s.state.Map.Generation, s.state.Map.Epoch
	if said := a.meta.Partitions[s.state.Map.Partitions[g].ID]; said != PartServing {
		t.Fatalf("A says %q of %s, want serving", said, p)
	}
	epoch := s.state.Map.Epoch
	if s.tick() {
		t.Fatalf("a tick with the target serving changed the map: A is %s at epoch %d, "+
			"from epoch %d", s.holder(g, "data-a").State, s.state.Map.Epoch, epoch)
	}
}

// A JOINER IS PROMOTED AT ITS OWN WORD, AT THE EPOCH THAT NAMED IT: its lease
// says it serves the partition, and names this map's generation at an epoch
// at least the one that named it — never an older tenure's serving, and never
// a claim from before it read the map.
func TestAJoinerIsPromotedAtItsOwnWordAtTheEpochThatNamedIt(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		said     PartitionState
		epoch    uint64
		otherGen bool
		promoted bool
	}{
		"serving, having read it":         {said: PartServing, epoch: 20, promoted: true},
		"serving, from before it read it": {said: PartServing, epoch: 19},
		"serving, another generation":     {said: PartServing, epoch: 30, otherGen: true},
		"still catching up":               {said: PartCatchingUp, epoch: 20},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := settled(t, smallLayout, 2, "data-00", "data-01")
			g, _ := s.targetedAt(s.state.Map.targets()[0]...)
			m := &s.state.Map
			m.Epoch = 20
			joiner := s.state.Map.targets()[g][0]
			for i := range m.Partitions[g].Holders {
				if m.Partitions[g].Holders[i].Node == joiner {
					m.Partitions[g].Holders[i] = Holder{Node: joiner, State: Joining, Since: 20}
				}
			}
			n := s.nodes[joiner]
			n.meta.Partitions[m.Partitions[g].ID] = c.said
			n.meta.MapGeneration, n.meta.MapEpoch = m.Generation, c.epoch
			if c.otherGen {
				n.meta.MapGeneration = uuid.New()
			}
			s.tick()
			if got := s.holder(g, joiner).State == Serving; got != c.promoted {
				t.Fatalf("promoted = %v, want %v", got, c.promoted)
			}
		})
	}
}

// A RELEASE REMOVES ONLY THE LEAVE IT ANSWERS: a leaver whose lease says
// released is dropped once that lease names the map that told it to leave —
// never on a released left over from an earlier tenure. A leaver whose lease
// lists what it holds without the partition — withdrawn before it adopted
// anything — has nothing to release and is dropped the same way; one that
// does not say what it holds is not.
func TestAReleaseRemovesOnlyTheLeaveItAnswers(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		said    PartitionState // empty: the lease does not list the partition
		unsaid  bool           // the lease does not say what it holds at all
		epoch   uint64
		removed bool
	}{
		"released, having read the leave":        {said: PartReleased, epoch: 20, removed: true},
		"released, from an earlier tenure":       {said: PartReleased, epoch: 19},
		"holding nothing, having read the leave": {epoch: 20, removed: true},
		"holding nothing, before reading it":     {epoch: 19},
		"not saying what it holds":               {unsaid: true, epoch: 20},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := settled(t, smallLayout, 1, "data-00", "data-01")
			g, _ := s.targetedAt("data-00")
			m := &s.state.Map
			m.Epoch = 20
			m.Partitions[g].Holders = append(m.Partitions[g].Holders,
				Holder{Node: "data-01", State: Leaving, Since: 20})
			sortHolders(m.Partitions[g].Holders)
			n := s.nodes["data-01"]
			delete(n.meta.Partitions, m.Partitions[g].ID)
			if c.said != "" {
				n.meta.Partitions[m.Partitions[g].ID] = c.said
			}
			if c.unsaid {
				n.meta.Partitions = nil
			}
			n.meta.MapGeneration, n.meta.MapEpoch = m.Generation, c.epoch
			s.tick()
			removed := holderOf(&s.state.Map.Partitions[g], "data-01") == nil
			if removed != c.removed {
				t.Fatalf("removed = %v, want %v", removed, c.removed)
			}
		})
	}
}

// WHAT EXISTS IS ADOPTED: a node reporting a partition the map does not list
// it for is added — serving where its copy is established, wanted or not,
// since it serves and restores a copy with no transfer; joining where it is
// still being built and wanted; leaving where it is still being built and not
// wanted, so its release fences anything it might still publish. A faulted,
// draining or released copy is not one to adopt. And an established copy the
// target does not want is let go like any server, only once the target
// vouches for the partition: here the target does not vouch on the tick that
// adopts it, so the copy is seen serving first, and does on the next.
func TestWhatANodeHoldsIsAdopted(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		said     PartitionState
		inTarget bool
		want     HolderState // empty: not adopted
	}{
		"serving and wanted":               {said: PartServing, inTarget: true, want: Serving},
		"serving and not wanted":           {said: PartServing, want: Serving},
		"catching up and wanted":           {said: PartCatchingUp, inTarget: true, want: Joining},
		"catching up and not wanted":       {said: PartCatchingUp, want: Leaving},
		"adopting and not wanted":          {said: PartAdopting, want: Leaving},
		"faulted":                          {said: PartFaulted, inTarget: true},
		"draining":                         {said: PartDraining, inTarget: true},
		"released":                         {said: PartReleased, inTarget: true},
		"a state this build does not know": {said: "verifying", inTarget: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := settled(t, smallLayout, 1, "data-00", "data-01")
			// The partition data-01 reports: one data-01 is the target
			// of and nobody holds, or one data-00 is the target of and
			// serves.
			wanted := "data-00"
			if c.inTarget {
				wanted = "data-01"
			}
			g, _ := s.targetedAt(wanted)
			m := &s.state.Map
			id := m.Partitions[g].ID
			m.Partitions[g].Holders = []Holder{{Node: "data-00", State: Serving, Since: 1}}
			if c.inTarget {
				m.Partitions[g].Holders = nil
			}
			s.nodes["data-01"].meta.Partitions[id] = c.said
			// data-00 has not read the map that made it the server, so it
			// vouches for nothing yet.
			vouched := s.nodes["data-00"].meta.MapEpoch
			s.nodes["data-00"].meta.MapEpoch = 0
			s.tick()
			h := holderOf(&s.state.Map.Partitions[g], "data-01")
			switch {
			case c.want == "" && h != nil && h.State != Joining:
				// A joiner is the ordinary join of a target node that
				// holds nothing the map can use; anything else is an
				// adoption.
				t.Fatalf("a %s copy was adopted as %s", c.said, h.State)
			case c.want == "":
				return
			case h == nil:
				t.Fatalf("a %s copy was not adopted", c.said)
			case h.State != c.want:
				t.Fatalf("a %s copy was adopted as %s, want %s", c.said, h.State, c.want)
			}
			if c.said != PartServing || c.inTarget {
				return
			}
			// Once the target vouches, the copy it does not want goes.
			s.nodes["data-00"].meta.MapEpoch = vouched
			s.tick()
			if got := s.holder(g, "data-01").State; got != Leaving {
				t.Fatalf("an adopted copy the target does not want is %s once the target "+
					"vouches, want leaving", got)
			}
		})
	}
}

// A JOINER THE TARGET LEFT IS WITHDRAWN AT ONCE, having served nothing — and
// a node whose join stalled for want of a donor is not left holding its one
// join for ever. A joiner that already serves is promoted instead, and then
// leaves like any server, under both conditions.
func TestAJoinerTheTargetLeftIsWithdrawn(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 1, "data-00", "data-01")
	g, _ := s.targetedAt("data-00")
	m := &s.state.Map
	id := m.Partitions[g].ID
	m.Partitions[g].Holders = append(m.Partitions[g].Holders,
		Holder{Node: "data-01", State: Joining, Since: m.Epoch})
	sortHolders(m.Partitions[g].Holders)
	s.nodes["data-01"].meta.Partitions[id] = PartAdopting
	s.tick()
	if got := s.holder(g, "data-01").State; got != Leaving {
		t.Fatalf("a joiner the target does not name is %s, want leaving", got)
	}
	s.settle(50)
	s.converged()
}

// A LEAVER WANTED AGAIN SERVES AGAIN while its lease still says it serves — the
// cheapest copy there is — and not once it has started to drain, which is
// never reversed: it releases, and joins afresh.
func TestALeaverWantedAgainServesAgainUntilItDrains(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		said PartitionState
		want HolderState
	}{
		"still serving": {said: PartServing, want: Serving},
		"draining":      {said: PartDraining, want: Leaving},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := settled(t, smallLayout, 1, "data-00", "data-01")
			g, _ := s.targetedAt("data-00")
			m := &s.state.Map
			m.Partitions[g].Holders = []Holder{{Node: "data-00", State: Leaving, Since: m.Epoch}}
			s.nodes["data-00"].meta.Partitions[m.Partitions[g].ID] = c.said
			s.tick()
			table := s.state.Map.Partitions[g]
			if len(table.Holders) != 1 || table.Holders[0].State != c.want {
				t.Fatalf("the leaver's table is %+v, want it %s alone", table.Holders, c.want)
			}
		})
	}
}

// ABSENCE IS COUNTED IN TICKS, NEVER TIMED. A member gone is removed after
// OutTicks of the maintainer's own ticks whatever the clocks of the nodes that
// held the duty said — here each tick hours after the last, as a duty that
// moved between nodes a day apart would read — and after exactly that many
// with no clock moving at all. Its holders go with it.
func TestAbsenceIsCountedInTicksNotTime(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 2, nodeIDs(3)...)
	s.nodes["data-02"].down = true
	for tick := 1; tick < membership.OutTicks; tick++ {
		s.now = s.now.Add(6 * time.Hour)
		s.tick()
		if !s.state.Map.Draw().Holds("data-02") {
			t.Fatalf("a member gone %d ticks, %v by the clock, was removed: absence "+
				"is counted in ticks", tick, time.Duration(tick)*6*time.Hour)
		}
	}
	s.tick()
	if s.state.Map.Draw().Holds("data-02") {
		t.Fatalf("a member gone %d ticks is still a member", membership.OutTicks)
	}
	for _, table := range s.state.Map.Partitions {
		if holderOf(&table, "data-02") != nil {
			t.Fatalf("the removed member still holds %s", table.ID)
		}
	}
}

// THE EPOCH COUNTS THE HOLDER TABLE AND NOTHING ELSE. A tick that only counts
// an absence changes the record and not the epoch; a gesture that moves the
// target changes the record and not the epoch; the tick that changes a holder
// moves it by one, and stamps exactly the holders it changed with it.
func TestTheEpochCountsTheHolderTableAlone(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 2, nodeIDs(3)...)
	epoch := s.state.Map.Epoch

	s.nodes["data-02"].down = true
	if !s.tick() {
		t.Fatal("a tick that counted an absence wrote nothing")
	}
	if s.state.Map.Epoch != epoch || s.state.Absence["data-02"].Ticks != 1 {
		t.Fatalf("an absence moved the epoch to %d (absence %+v)", s.state.Map.Epoch,
			s.state.Absence["data-02"])
	}
	s.nodes["data-02"].down = false

	before := s.state.Map.Clone()
	next, err := Out(s.state, "data-00", "op", "test", s.now)
	if err != nil {
		t.Fatal(err)
	}
	if next.Map.Epoch != epoch || slices.Equal(next.Map.Members, before.Members) {
		t.Fatalf("taking a member out moved the epoch to %d, or changed no member", next.Map.Epoch)
	}
	s.state = next
	s.tick()
	if s.state.Map.Epoch != epoch+1 {
		t.Fatalf("the tick that moved holders toward the new target left the epoch at %d, "+
			"want %d", s.state.Map.Epoch, epoch+1)
	}
	for g, table := range s.state.Map.Partitions {
		for _, h := range table.Holders {
			was := holderOf(&before.Partitions[g], h.Node)
			changed := was == nil || *was != Holder{Node: h.Node, State: h.State, Since: was.Since}
			if changed && h.Since != epoch+1 {
				t.Fatalf("%s's changed holder %s is stamped %d, want %d", table.ID, h.Node,
					h.Since, epoch+1)
			}
			if !changed && h.Since != was.Since {
				t.Fatalf("%s's unchanged holder %s was restamped %d", table.ID, h.Node, h.Since)
			}
		}
	}
}

// A NODE THAT DOES NOT SAY ITS STORE IS HEALTHY IS NOT PLACED ON: a newcomer
// whose lease leaves health out, or runs another layout, is not made a
// member, and a member whose lease stops saying is counted absent — named as
// unhealthy, with what it said instead — and removed after the grace.
func TestANodeThatDoesNotSayItIsHealthyIsNotPlacedOn(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*Meta){
		"health unsaid":  func(m *Meta) { m.Healthy = nil },
		"failed":         func(m *Meta) { m.Healthy, m.Detail = no(), "disk gone" },
		"layout unsaid":  func(m *Meta) { m.Layout = nil },
		"another layout": func(m *Meta) { m.Layout = layoutNo(2) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := settled(t, smallLayout, 2, nodeIDs(3)...)
			change(&s.add("data-09", 1, nil).meta)
			change(&s.nodes["data-02"].meta)
			s.tick()
			if s.state.Map.Draw().Holds("data-09") {
				t.Fatal("a newcomer that did not say it can hold partitions was made a member")
			}
			run := s.state.Absence["data-02"]
			if run.Ticks != 1 || run.Reason != membership.ReasonUnhealthy || run.Detail == "" {
				t.Fatalf("a member that stopped saying was counted %+v", run)
			}
			for range membership.OutTicks {
				s.tick()
			}
			if s.state.Map.Draw().Holds("data-02") {
				t.Fatal("a member that did not say it can hold partitions for the grace " +
					"is still a member")
			}
		})
	}
}

// NOTHING A NODE THE TICK COUNTS UNHEALTHY SAYS MAKES IT A HOLDER, OR A
// SERVER. Membership counts such a node exactly as an absent one — a store
// that says it has failed, or will not say, or runs another layout, answers
// for none of what it holds — so its lease is no account of a partition in
// either direction: no copy it reports is adopted, no join of its is
// promoted, and no leave of its is taken back on its word that it serves,
// since each of those would have routers send a partition's reads and writes
// to a store that has said it cannot be trusted with one. What it already
// holds stays as it is until it is able again, or membership removes it.
// Each transition is shown happening on a healthy lease first, so a case
// that stays put is the health's doing.
func TestNothingAnUnhealthyNodeSaysMakesItAServer(t *testing.T) {
	t.Parallel()
	transitions := map[string]struct {
		// stage puts data-01 where the transition starts and has its
		// lease say what makes it happen, returning the partition.
		stage func(s *sim) int
		// want is data-01's holder state after the tick on a healthy
		// lease; on an unhealthy one it keeps was, and "" is no holder.
		want, was HolderState
	}{
		"adopted serving": {want: Serving, stage: func(s *sim) int {
			g, _ := s.targetedAt("data-01")
			s.state.Map.Partitions[g].Holders = nil
			s.nodes["data-01"].meta.Partitions[s.state.Map.Partitions[g].ID] = PartServing
			return g
		}},
		"adopted joining": {want: Joining, stage: func(s *sim) int {
			g, _ := s.targetedAt("data-01")
			s.state.Map.Partitions[g].Holders = nil
			s.nodes["data-01"].meta.Partitions[s.state.Map.Partitions[g].ID] = PartCatchingUp
			return g
		}},
		"adopted leaving": {want: Leaving, stage: func(s *sim) int {
			g, _ := s.targetedAt("data-00")
			s.nodes["data-01"].meta.Partitions[s.state.Map.Partitions[g].ID] = PartCatchingUp
			return g
		}},
		"promoted": {want: Serving, was: Joining, stage: func(s *sim) int {
			g, _ := s.targetedAt("data-01")
			m := &s.state.Map
			m.Partitions[g].Holders = []Holder{{Node: "data-01", State: Joining, Since: m.Epoch}}
			s.nodes["data-01"].meta.Partitions[m.Partitions[g].ID] = PartServing
			return g
		}},
		"serving again": {want: Serving, was: Leaving, stage: func(s *sim) int {
			g, _ := s.targetedAt("data-01")
			m := &s.state.Map
			m.Partitions[g].Holders = []Holder{{Node: "data-01", State: Leaving, Since: m.Epoch}}
			s.nodes["data-01"].meta.Partitions[m.Partitions[g].ID] = PartServing
			return g
		}},
	}
	unhealthy := map[string]func(*Meta){
		"healthy":        nil,
		"failed":         func(m *Meta) { m.Healthy, m.Detail = no(), "disk gone" },
		"health unsaid":  func(m *Meta) { m.Healthy = nil },
		"another layout": func(m *Meta) { m.Layout = layoutNo(2) },
	}
	for name, c := range transitions {
		for how, change := range unhealthy {
			t.Run(name+"/"+how, func(t *testing.T) {
				t.Parallel()
				s := settled(t, smallLayout, 1, "data-00", "data-01")
				g := c.stage(s)
				n := s.nodes["data-01"]
				n.meta.MapGeneration, n.meta.MapEpoch = s.state.Map.Generation, s.state.Map.Epoch
				want := c.want
				if change != nil {
					change(&n.meta)
					want = c.was
				}
				s.tick()
				var got HolderState
				if h := holderOf(&s.state.Map.Partitions[g], "data-01"); h != nil {
					got = h.State
				}
				if got != want {
					t.Fatalf("%s's holder on a %s lease is %q, want %q (table %+v)",
						s.state.Map.Partitions[g].ID, how, got, want, s.state.Map.Partitions[g].Holders)
				}
			})
		}
	}
}

// NOTHING A BARRED NODE SAYS MAKES IT A SERVER, until [In] lifts the bar.
//
// A bar is an eviction's part in the map, and every log of every partition
// still gates the node as evicted: every write it decides there is dropped,
// and the trim counts it from its tombstone rather than its row. So a copy
// it reports is never adopted serving, a join of its is never promoted, and a
// leave of its is never taken back — even where the target does not serve
// yet, which is exactly when an out's copy would serve. Each transition is
// shown happening for a node TAKEN OUT first, which the target leaves out
// exactly as it leaves out a barred one, so a case that does not happen is the
// bar's doing and not the target's. Both passes answer alike: the tick's, and
// the convergence between ticks.
func TestNothingABarredNodeSaysMakesItAServer(t *testing.T) {
	t.Parallel()
	// Each stage has the partition's target, data-00, still joining it —
	// adopting its copy, serving nothing — and data-01 saying it serves.
	transitions := map[string]func(s *sim, g int){
		// Adopted: a copy the map does not list.
		"adopted": func(s *sim, g int) {
			m := &s.state.Map
			m.Partitions[g].Holders = []Holder{{Node: "data-00", State: Joining, Since: m.Epoch}}
		},
		// Promoted: a joiner that says it serves, having read the map
		// that named it.
		"promoted": func(s *sim, g int) {
			m := &s.state.Map
			m.Partitions[g].Holders = []Holder{
				{Node: "data-00", State: Joining, Since: m.Epoch},
				{Node: "data-01", State: Joining, Since: m.Epoch},
			}
		},
		// Serving again: a leaver that still serves what nobody else
		// serves, having read the map that made it a leaver.
		"serving again": func(s *sim, g int) {
			m := &s.state.Map
			m.Partitions[g].Holders = []Holder{
				{Node: "data-00", State: Joining, Since: m.Epoch},
				{Node: "data-01", State: Leaving, Since: m.Epoch},
			}
		},
	}
	gestures := map[string]struct {
		apply func(MapState) (MapState, error)
		want  HolderState
	}{
		"taken out": {want: Serving, apply: func(st MapState) (MapState, error) {
			return Out(st, "data-01", "ops", "maintenance", base)
		}},
		"barred": {want: Leaving, apply: func(st MapState) (MapState, error) {
			return Bar(st, "data-01", "ops", "evicted", base)
		}},
	}
	passes := map[string]func(s *sim) MapState{
		"tick": func(s *sim) MapState {
			next, _ := Next(s.state, Input{Layout: s.layout, Live: s.live(), Company: s.company, Now: s.now})
			return next
		},
		"between ticks": func(s *sim) MapState {
			next, _ := Converge(s.state, s.live())
			return next
		},
	}
	for name, stage := range transitions {
		for how, gesture := range gestures {
			for pass, run := range passes {
				t.Run(name+"/"+how+"/"+pass, func(t *testing.T) {
					t.Parallel()
					s := settled(t, smallLayout, 1, "data-00", "data-01")
					next, err := gesture.apply(s.state)
					if err != nil {
						t.Fatal(err)
					}
					s.state = next
					g, _ := s.targetedAt("data-00")
					stage(s, g)
					id := s.state.Map.Partitions[g].ID
					s.nodes["data-00"].meta.Partitions[id] = PartAdopting
					s.nodes["data-01"].meta.Partitions[id] = PartServing
					n := s.nodes["data-01"]
					n.meta.MapGeneration, n.meta.MapEpoch = s.state.Map.Generation, s.state.Map.Epoch
					after := run(s)
					var got HolderState
					if h := holderOf(&after.Map.Partitions[g], "data-01"); h != nil {
						got = h.State
					}
					if got != gesture.want {
						t.Fatalf("%s's holder data-01, %s, is %q after the %s, want %q (table %+v)",
							after.Map.Partitions[g].ID, how, got, pass, gesture.want,
							after.Map.Partitions[g].Holders)
					}
				})
			}
		}
	}
}

// A BARRED NODE THAT COMES BACK WITH ITS FILES IS NEVER A SERVER, however long
// the partitions it held go unserved by their whole target.
//
// The machine is evicted, the map removes it for its absence, and a new node
// joins while it is away — one join at a time, each waiting on its transfer,
// so for hours a partition's target does not serve it all. The machine then
// returns under its old id, its lease saying it serves everything it held.
// Adopted serving, it would be routed to for as long as the new node's joins
// took, while every log of those partitions dropped each write it decided and
// the trim passed the holder applying them. It is adopted leaving instead, and
// releases; the new node's joins finish; and only [Readmit] places on it
// again.
func TestABarredNodeThatComesBackIsNeverAServer(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 2, "data-00", "data-01", "data-02")
	next, err := Bar(s.state, "data-02", "ops", "evicted", base)
	if err != nil {
		t.Fatal(err)
	}
	s.state = next
	s.nodes["data-02"].down = true
	for range 3 * membership.OutTicks {
		s.tick()
		s.check()
		s.act()
	}
	if s.state.Map.Draw().Holds("data-02") {
		t.Fatal("the premise: a node gone for three graces is still a member")
	}

	// A node joining while it is away, and not serving yet.
	s.add("data-03", 1, nil).frozen = true
	s.tick()
	if s.holding("data-03")[Joining] == 0 {
		t.Fatal("the premise: the new node was named no join")
	}
	s.nodes["data-02"].down = false
	neverServes := func(round int) {
		t.Helper()
		for _, table := range s.state.Map.Partitions {
			if h := holderOf(&table, "data-02"); h != nil && h.State == Serving {
				p, _ := statelog.ParsePartitionID(table.ID)
				t.Fatalf("round %d: the barred node is a serving holder of %s, and routers "+
					"route to %v", round, table.ID, s.state.Map.Serving(p))
			}
		}
	}
	adopted := false
	for round := range 4 * membership.StableTicks {
		s.tick()
		neverServes(round)
		adopted = adopted || s.holding("data-02")[Leaving] > 0
		s.check()
		s.act()
	}
	if !adopted {
		t.Fatal("the barred node's copies were never adopted to be let go")
	}
	if held := s.holding("data-02"); len(held) != 0 {
		t.Fatalf("the barred node still holds %v once it has released", held)
	}

	s.nodes["data-03"].frozen = false
	s.settle(400)
	s.converged()
	if held := s.holding("data-02"); len(held) != 0 {
		t.Fatalf("a barred node holds %v once the map has settled", held)
	}

	back, err := Readmit(s.state, "data-02")
	if err != nil {
		t.Fatal(err)
	}
	s.state = back
	s.settle(400)
	s.converged()
	if s.holding("data-02")[Serving] == 0 {
		t.Fatal("the node put back holds nothing")
	}
}

// ONLY THE NEWEST COMPANY SETS THE COPIES: the duty moves between nodes, and a
// node a revision behind must not set the replica count back (ADR-0020).
func TestOnlyTheNewestCompanySetsTheEstatesCopies(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 2, nodeIDs(3)...)
	s.company = company(3, "", 5)
	s.tick()
	if s.state.Map.Replicas != 3 || s.state.Config.Epoch != 5 {
		t.Fatalf("activation 5's three copies set %d from %d", s.state.Map.Replicas, s.state.Config.Epoch)
	}
	s.company = company(1, "", 4)
	s.tick()
	if s.state.Map.Replicas != 3 {
		t.Fatalf("activation 4, older than the map's 5, set the copies to %d", s.state.Map.Replicas)
	}
}

// A MOVE IS FORGOTTEN WITH ITS NODE, the way an operator's out is: a node the
// map removed and sees again much later is a node like any other.
func TestAMoveIsForgottenWithItsNode(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 2, nodeIDs(3)...)
	_, p := s.targetedAt(s.state.Map.targets()[0]...)
	node := s.state.Map.Target(p)[0]
	next, err := Move(s.state, p, node, "op", "test", s.now)
	if err != nil {
		t.Fatal(err)
	}
	s.state = next
	s.nodes[node].down = true
	for range membership.OutTicks {
		s.tick()
	}
	if s.state.Map.Draw().Holds(node) {
		t.Fatalf("%s is still a member after the grace", node)
	}
	if len(s.state.Map.Moves) != 0 {
		t.Fatalf("the moves still name a removed node: %v", s.state.Map.Moves)
	}
}

// ONE DRAW DEALS EVERY SPACE ALIKE. The map draws every partition of every
// space with one share per member, because the layout sizes every partition
// log alike: a member's share of partitions is its share of the estate. What
// that must not do is let a member carry one space at the expense of another —
// and it cannot, since a partition's ranking knows nothing of its space but
// its seed. Measured at the owner's layout over eight equal members, each
// holds within a third of its proportional share of each space, the pages
// space's 192 copies included.
func TestOneDrawDealsEverySpaceAlike(t *testing.T) {
	t.Parallel()
	next, _ := Next(MapState{}, Input{Layout: ownerLayout, Live: newSim(t, ownerLayout, 3,
		nodeIDs(8)...).live(), Company: company(3, "", 1), Now: base})
	m := next.Map
	counts := map[statelog.Space]map[string]int{}
	for g, target := range m.targets() {
		p, _ := statelog.ParsePartitionID(m.Partitions[g].ID)
		if counts[p.Space] == nil {
			counts[p.Space] = map[string]int{}
		}
		for _, node := range target {
			counts[p.Space][node]++
		}
	}
	for _, space := range []statelog.Space{statelog.SpaceTracker, statelog.SpacePages} {
		want := float64(ownerLayout.Count(space)*3) / 8
		var line []string
		for _, node := range nodeIDs(8) {
			got := float64(counts[space][node])
			line = append(line, fmt.Sprintf("%s=%d", node, counts[space][node]))
			if got < want*2/3 || got > want*4/3 {
				t.Errorf("%s holds %v of the %s space's copies, against %.1f by its share",
					node, got, space, want)
			}
		}
		t.Logf("%s (%.1f each by share): %v", space, want, line)
	}
}

// THE BALANCE CONVERGES AT WHAT THE LAYOUT PROMISES, over every fleet size the
// owner's layout serves — three data nodes to a hundred, equal weights and a
// mix of them — where at two percent a fleet past about a dozen could only run
// out of rounds. This is the measurement behind balancing the estate map at
// all: a weight means what it says, and a change of members moves shares to a
// tolerance the partition count can express rather than chasing one it
// cannot.
func TestTheEstateBalanceConvergesAtWhatTheLayoutPromises(t *testing.T) {
	t.Parallel()
	for _, size := range []int{3, 5, 8, 12, 20, 40, 64, 100} {
		for _, mixed := range []bool{false, true} {
			var live []Presence
			for i, node := range nodeIDs(size) {
				weight := 1
				if mixed {
					weight = 1 + i%4
				}
				live = append(live, Presence{Node: node, Meta: Meta{Weight: weight,
					Layout: layoutNo(1), Healthy: yes()}})
			}
			next, _ := Next(MapState{}, Input{Layout: ownerLayout, Live: live,
				Company: company(3, "", 1), Now: base})
			b := next.Balance
			t.Logf("%3d members, mixed weights %-5v: tolerance %.4f, %2d rounds, deviation %.4f, converged %v",
				size, mixed, b.Tolerance, b.Rounds, b.Deviation, b.Converged)
			if !b.Converged {
				t.Errorf("%d members (mixed %v) did not converge at the tolerance the layout "+
					"promises: %+v", size, mixed, b)
			}
		}
	}
}

// THE MAINTAINER BALANCES WHEN WHAT THE MAP PLACES CHANGES, AND ONLY THEN. A
// member arriving, a weight changing and the company's copies changing each
// move the draw, and the tick that sees it balances the shares again — to the
// tolerance the layout promises for the fleet as it now is — so the record's
// balance describes the draw it is stored with. A tick that changes nothing
// the map places balances nothing: a balance moves shares, and a share moved
// with nothing to answer is partitions moved for nothing, on every tick.
func TestTheMaintainerBalancesWhenPlacementChangesAndOnlyThen(t *testing.T) {
	t.Parallel()
	lease := func(weight int) Meta { return Meta{Weight: weight, Layout: layoutNo(1), Healthy: yes()} }
	fleet := func(weights ...int) []Presence {
		var out []Presence
		for i, w := range weights {
			out = append(out, Presence{Node: nodeIDs(len(weights))[i], Meta: lease(w)})
		}
		return out
	}
	in := Input{Layout: ownerLayout, Live: fleet(1, 1, 1, 1, 1), Company: company(3, "", 1), Now: base}
	first, _ := Next(MapState{}, in)

	// A tick that places nothing differently keeps every share and the
	// balance as they were, to the byte.
	in.Now = in.Now.Add(membership.TickInterval)
	again, _ := Next(first, in)
	if !slices.Equal(again.Map.Members, first.Map.Members) {
		t.Fatalf("a tick that changed no placement moved the shares:\n%+v\n%+v",
			first.Map.Members, again.Map.Members)
	}
	was, _ := json.Marshal(first.Balance)
	is, _ := json.Marshal(again.Balance)
	if string(was) != string(is) {
		t.Fatalf("a tick that changed no placement balanced again: %s, was %s", is, was)
	}

	for name, change := range map[string]func(Input) Input{
		"a member joins": func(in Input) Input {
			in.Live = fleet(1, 1, 1, 1, 1, 1)
			return in
		},
		"a weight changes": func(in Input) Input {
			in.Live = fleet(4, 1, 1, 1, 1)
			return in
		},
		"the copies change": func(in Input) Input {
			in.Company = company(2, "", 2)
			return in
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			next, changed := Next(first, change(in))
			if !changed {
				t.Fatal("the change wrote nothing")
			}
			d := next.Map.Draw()
			b, dev := next.Balance, d.Layout().Deviation()
			if want := d.Reachable(placement.DefaultTolerance); b.Tolerance != want {
				t.Fatalf("balanced at a tolerance of %.4f, where the layout promises %.4f for "+
					"the fleet as it now is", b.Tolerance, want)
			}
			if b.Deviation != dev || !b.Converged || dev > b.Tolerance {
				t.Fatalf("the draw stored is %.4f off against a tolerance of %.4f, and the "+
					"record says %+v", dev, b.Tolerance, b.BalanceReport)
			}
		})
	}
}

// A JOINER THAT GAVE THE JOIN UP IS LET GO, AND A REJOIN IS NOT MISTAKEN FOR
// ONE. A joiner whose lease — having read the map that named it — says it is
// draining or released has given the join up, and holds its node's one join
// for nothing until the map lets it go. The same word from before it read that
// map is what its last tenure left: a node that left the partition and was
// named again, and read as leaving it would be named and let go for ever.
func TestAJoinerThatGaveUpIsLetGoAndARejoinIsNot(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		epoch uint64
		want  HolderState
	}{
		"having read the map that named it": {epoch: 20, want: Leaving},
		"before reading it: a rejoin":       {epoch: 19, want: Joining},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := settled(t, smallLayout, 1, "data-00", "data-01")
			g, _ := s.targetedAt("data-00")
			m := &s.state.Map
			m.Epoch = 20
			m.Partitions[g].Holders = []Holder{
				{Node: "data-00", State: Joining, Since: 20}, {Node: "data-01", State: Serving, Since: 1}}
			n := s.nodes["data-00"]
			n.meta.Partitions[m.Partitions[g].ID] = PartReleased
			n.meta.MapGeneration, n.meta.MapEpoch = m.Generation, c.epoch
			s.tick()
			if got := s.holder(g, "data-00").State; got != c.want {
				t.Fatalf("the joiner is %s, want %s", got, c.want)
			}
		})
	}
}

// A FLEET IN MOTION NEVER LOSES A PARTITION'S LAST COPY. Operators take nodes
// out and put them back, move partitions off their holders and cancel the
// moves, nodes join the fleet and go down for a while, all at random, and
// every node acts on the map as the join and leave protocols do — and through
// all of it no partition that was served is left with no copy serving
// anywhere: a copy is let go only once its target serves. Then the fleet
// settles on its target.
func TestAFleetInMotionNeverLosesAPartitionsLastCopy(t *testing.T) {
	t.Parallel()
	for seed := range uint64(6) {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(seed, 0xe57a7e))
			s := settled(t, smallLayout, 2, nodeIDs(5)...)
			s.check = s.checkCopies
			downFor := map[string]int{}
			pick := func(from []string) string { return from[rng.IntN(len(from))] }
			for round := range 400 {
				nodes := slices.Sorted(maps.Keys(s.nodes))
				switch rng.IntN(10) {
				case 0:
					if next, err := Out(s.state, pick(nodes), "op", "", s.now); err == nil {
						s.state = next
					}
				case 1:
					if next, err := In(s.state, pick(nodes)); err == nil {
						s.state = next
					}
				case 2:
					g := rng.IntN(len(s.state.Map.Partitions))
					p, _ := statelog.ParsePartitionID(s.state.Map.Partitions[g].ID)
					if hs := s.state.Map.Partitions[g].Holders; len(hs) > 0 {
						if next, err := Move(s.state, p, hs[rng.IntN(len(hs))].Node, "op", "", s.now); err == nil {
							s.state = next
						}
					}
				case 3:
					for id, moved := range s.state.Map.Moves {
						p, _ := statelog.ParsePartitionID(id)
						for node := range moved {
							if next, err := CancelMove(s.state, p, node); err == nil {
								s.state = next
							}
						}
						break
					}
				case 4:
					if len(nodes) < 8 {
						s.add(fmt.Sprintf("data-%02d", 10+round), 1+rng.IntN(3), nil)
					}
				case 5:
					node := pick(nodes)
					if !s.nodes[node].down {
						s.nodes[node].down = true
						downFor[node] = 1 + rng.IntN(membership.OutTicks/2)
					}
				}
				for node, left := range downFor {
					if left--; left == 0 {
						s.nodes[node].down = false
						delete(downFor, node)
						continue
					}
					downFor[node] = left
				}
				s.tick()
				s.check()
				s.act()
			}
			for node := range s.nodes {
				s.nodes[node].down = false
				next, err := In(s.state, node)
				if err == nil {
					s.state = next
				}
			}
			for id, moved := range s.state.Map.Moves {
				p, _ := statelog.ParsePartitionID(id)
				for node := range moved {
					next, err := CancelMove(s.state, p, node)
					if err != nil {
						t.Fatal(err)
					}
					s.state = next
				}
			}
			s.settle(300)
			s.converged()
		})
	}
}

// THE CUTOVER ADOPTS EVERY COPY AND THEN LETS THE EXTRAS GO. When a fleet
// divides its estate, every data node holds every partition — each split from
// its own whole copy — and the first map finds them all serving. It adopts
// every one rather than naming a single join, since every copy a target wants
// already exists, and then lets the ones no target wants go under the same
// two conditions as any leave: no partition is ever left without a copy, and
// the fleet settles on its target with no transfer at all.
func TestTheCutoverAdoptsEveryCopyAndLetsTheExtrasGo(t *testing.T) {
	t.Parallel()
	s := newSim(t, ownerLayout, 3, nodeIDs(5)...)
	for _, n := range s.nodes {
		for _, p := range ownerLayout.Partitions() {
			n.meta.Partitions[p.String()] = PartServing
		}
	}
	s.check = s.checkCopies
	s.check()
	for round := 0; ; round++ {
		if round > 20 {
			t.Fatal("the cutover had not settled after 20 rounds")
		}
		changed := s.tick()
		s.check()
		for _, table := range s.state.Map.Partitions {
			for _, h := range table.Holders {
				if h.State == Joining {
					t.Fatalf("after tick %d %s joins %s, which it already holds", s.ticks,
						h.Node, table.ID)
				}
			}
		}
		if acted := s.act(); !changed && !acted {
			break
		}
	}
	s.converged()
	t.Logf("settled after %d ticks at epoch %d", s.ticks, s.state.Map.Epoch)
}
