package partmap

import (
	"slices"
	"testing"
)

// A SERVING HOLDER WHOSE NODE IS GONE IS NO COPY, to the surfaces and the
// alarms: the map keeps it serving for membership's grace, routers route to it
// and nothing answers, so counting it would call a partition whole for the ten
// minutes nobody can read it — and a partition whose only server went down
// served by one.
func TestACopyIsAServingHolderWhoseNodeIsAble(t *testing.T) {
	t.Parallel()
	s := newSim(t, smallLayout, 2, nodeIDs(3)...)
	s.settle(40)
	s.converged()
	for _, c := range s.state.Map.Coverage(s.live()) {
		if c.Short() || c.Unserved() || len(c.Serving) != 2 || c.Wanted != 2 {
			t.Fatalf("%s on a settled fleet: %+v, want two copies of two", c.Partition, c)
		}
	}

	// data-00 goes down; the map still lists it serving what it held.
	s.nodes["data-00"].down = true
	held := 0
	for _, c := range s.state.Map.Coverage(s.live()) {
		if slices.Contains(s.state.Map.Partitions[indexOf(t, s.state.Map, c)].nodes(), "data-00") {
			held++
			if !c.Short() || len(c.Serving) != 1 || slices.Contains(c.Serving, "data-00") {
				t.Errorf("%s held by a node that is down reads %+v, want one copy of two",
					c.Partition, c)
			}
			continue
		}
		if c.Short() {
			t.Errorf("%s, not held by the node that went down, reads short: %+v", c.Partition, c)
		}
	}
	if held == 0 {
		t.Fatal("the node that went down held nothing, so this case certifies nothing")
	}

	// And a node whose lease says its store failed counts as gone too, the
	// maintainer's own reading of it.
	s.nodes["data-00"].down = false
	s.nodes["data-00"].meta.Healthy = no()
	if able := Able(s.live(), smallLayout.Number); able["data-00"] || !able["data-01"] {
		t.Errorf("able = %v, want every healthy node and not the failed one", able)
	}
}

// NO COPY THAT CAN ANSWER IS AN UNSERVED PARTITION, which is also short.
func TestAPartitionWithNoAbleServerIsUnserved(t *testing.T) {
	t.Parallel()
	s := newSim(t, smallLayout, 1, nodeIDs(2)...)
	s.settle(40)
	s.converged()
	s.nodes["data-00"].down = true
	unserved := 0
	for _, c := range s.state.Map.Coverage(s.live()) {
		if !c.Unserved() {
			continue
		}
		unserved++
		if !c.Short() {
			t.Errorf("%s is unserved and not short: %+v", c.Partition, c)
		}
	}
	if unserved == 0 {
		t.Fatal("no partition of the node that went down reads unserved")
	}
}

// A JOINER IS NAMED ON THE PARTITION IT JOINS, whatever its node's health — the
// stall an alarm watches for is the join's, not the node's.
func TestCoverageNamesEveryJoiner(t *testing.T) {
	t.Parallel()
	s := newSim(t, smallLayout, 2, nodeIDs(2)...)
	s.settle(40)
	s.add("data-02", 1, nil)
	s.tick()
	joiners := 0
	for g, c := range s.state.Map.Coverage(s.live()) {
		for _, h := range s.state.Map.Partitions[g].Holders {
			if h.State == Joining && !slices.ContainsFunc(c.Joining, func(j Holder) bool {
				return j == h
			}) {
				t.Errorf("%s's joiner %s is not in its coverage %+v", c.Partition, h.Node, c)
			}
		}
		joiners += len(c.Joining)
	}
	if joiners == 0 {
		t.Fatal("a third node named no join at all, so this case certifies nothing")
	}
}

// TARGETS IS TARGET, FOR EVERY PARTITION AT ONCE — a moved one included, whose
// target skips the node it was moved off.
func TestTargetsIsEveryPartitionsTarget(t *testing.T) {
	t.Parallel()
	s := newSim(t, smallLayout, 2, nodeIDs(4)...)
	s.settle(80)
	p := s.state.Map.Layout.Partitions()[3]
	moved, err := Move(s.state, p, s.state.Map.Target(p)[0], "founder", "", base)
	if err != nil {
		t.Fatal(err)
	}
	m := moved.Map
	for g, target := range m.Targets() {
		if q := m.Layout.Partitions()[g]; !slices.Equal(target, m.Target(q)) {
			t.Errorf("%s: Targets says %v and Target %v", q, target, m.Target(q))
		}
	}
	if slices.Equal(m.Targets()[3], s.state.Map.Targets()[3]) {
		t.Errorf("the moved partition's target did not change: %v", m.Targets()[3])
	}
}

// indexOf is c's partition's position in m.
func indexOf(t *testing.T, m Map, c Coverage) int {
	t.Helper()
	g, ok := groupOf(m.Layout, c.Partition)
	if !ok {
		t.Fatalf("%s is not in the layout", c.Partition)
	}
	return g
}

// nodes is a partition's holders' nodes.
func (p Partition) nodes() []string {
	out := make([]string, 0, len(p.Holders))
	for _, h := range p.Holders {
		out = append(out, h.Node)
	}
	return out
}
