package partmap

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/membership"
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
// partition every member has been moved or taken off keeps every copy it has
// rather than retiring them in favour of nobody.
func TestTheLastServerIsNeverLetGo(t *testing.T) {
	t.Parallel()
	s := settled(t, smallLayout, 1, "data-a", "data-b")
	g, p := s.targetedAt("data-a")
	id := s.state.Map.Partitions[g].ID

	// Both serve it, then it is moved off B and A is taken out: its target
	// is empty.
	s.state.Map.Partitions[g].Holders = []Holder{
		{Node: "data-a", State: Serving, Since: 1}, {Node: "data-b", State: Serving, Since: 1}}
	s.nodes["data-b"].meta.Partitions[id] = PartServing
	next, err := Move(s.state, p, "data-b", "op", "test", s.now)
	if err != nil {
		t.Fatal(err)
	}
	if next, err = Out(next, "data-a", "op", "test", s.now); err != nil {
		t.Fatal(err)
	}
	s.state = next
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
// target does not want is let go like any server — only once the target
// vouches for the partition, never in the tick that adopted it.
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
