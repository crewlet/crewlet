package partmap

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/statelog"
)

// testLayout is a partitioned layout of the default shape at these counts:
// the tracker space with the tracker's log and its vectors, the pages space
// with the pages' log and theirs, and one company partition. It restates
// engine.DefaultLayoutOne's shape because the engine imports what this
// package's consumers import, and a test here cannot import it.
func testLayout(tracker, pages int) statelog.Layout {
	return statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: tracker, Domains: []string{"tracker", "vectors"}},
		{Space: statelog.SpacePages, Partitions: pages, Domains: []string{"pages", "vectors"}},
		{Space: statelog.SpaceCompany, Partitions: 1, Domains: []string{"tracker"}},
	}}
}

// ownerLayout is the owner's default: 256 tracker partitions, 64 pages
// partitions and the company's one — 321 in all.
var ownerLayout = testLayout(256, 64)

// smallLayout is a layout small enough to reason about partition by
// partition: eight tracker partitions, two pages partitions and the company's.
var smallLayout = testLayout(8, 2)

// company is the company's estate block at this activation.
func company(replicas int, domain string, epoch uint64) membership.Company {
	return membership.Company{Epoch: epoch, Replicas: replicas, FailureDomain: domain, Block: "estate"}
}

// base is the suite's base instant: fixed, and in the past.
var base = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// yes is a pointer to true, for a lease that says its store is healthy.
func yes() *bool { b := true; return &b }

// no is a pointer to false.
func no() *bool { b := false; return &b }

// layoutNo is a pointer to a layout number.
func layoutNo(n int) *int { return &n }

// simNode is one data node as the simulation runs it: its lease, and whether
// it is up.
type simNode struct {
	meta Meta
	down bool

	// stale, when set, is the map the node acts on instead of the current
	// one: a node that has not read the latest version yet.
	stale *Map
}

// sim is a fleet of data nodes and the map's maintainer, ticked by hand: the
// maintainer runs [Next], and every node that is up acts on the map the way
// the join and leave protocols do — one step per act.
type sim struct {
	t       *testing.T
	state   MapState
	nodes   map[string]*simNode
	layout  statelog.Layout
	company membership.Company
	now     time.Time
	ticks   int

	// served is every partition some node has served, which from then on
	// some node must go on serving.
	served map[string]bool

	// check is the invariant every round is held to: checkServed unless a
	// case says otherwise.
	check func()
}

// newSim is a fleet of equal, healthy nodes running layout, with no map yet.
func newSim(t *testing.T, layout statelog.Layout, replicas int, nodes ...string) *sim {
	t.Helper()
	s := &sim{t: t, nodes: map[string]*simNode{}, layout: layout,
		company: company(replicas, "", 1), now: base, served: map[string]bool{}}
	s.check = s.checkServed
	for _, n := range nodes {
		s.add(n, 1, nil)
	}
	return s
}

// add brings a healthy node up with this weight and these labels, holding
// nothing.
func (s *sim) add(node string, weight int, labels map[string]string) *simNode {
	n := &simNode{meta: Meta{Weight: weight, Labels: labels, Layout: layoutNo(s.layout.Number),
		Healthy: yes(), Partitions: map[string]PartitionState{}}}
	s.nodes[node] = n
	return n
}

// live is every up node's lease, as the maintainer lists them.
func (s *sim) live() []Presence {
	var out []Presence
	for _, node := range slices.Sorted(maps.Keys(s.nodes)) {
		if n := s.nodes[node]; !n.down {
			meta := n.meta
			meta.Partitions = maps.Clone(n.meta.Partitions)
			out = append(out, Presence{Node: node, Meta: meta})
		}
	}
	return out
}

// tick runs the maintainer once, keeps what it wrote, and reports whether it
// wrote anything. Every map it writes must be one a store would accept.
func (s *sim) tick() bool {
	s.t.Helper()
	next, changed := Next(s.state, Input{Layout: s.layout, Live: s.live(), Company: s.company, Now: s.now})
	s.ticks++
	if !changed {
		return false
	}
	if _, err := next.Encode(); err != nil {
		s.t.Fatalf("tick %d wrote a map no store would accept: %v", s.ticks, err)
	}
	if next.Map.Epoch < s.state.Map.Epoch {
		s.t.Fatalf("tick %d moved the epoch back from %d to %d", s.ticks, s.state.Map.Epoch, next.Map.Epoch)
	}
	s.state = next
	return true
}

// act has every up node take one step toward what its map says: a joiner
// adopts, then catches up, then serves; a leaver drains, then releases; a
// released partition the map no longer lists is dropped. Each node then
// names the map it acted on. It reports whether any lease changed.
func (s *sim) act() bool {
	changed := false
	for _, node := range slices.Sorted(maps.Keys(s.nodes)) {
		n := s.nodes[node]
		if n.down {
			continue
		}
		m := s.state.Map
		if n.stale != nil {
			m = *n.stale
		}
		before := maps.Clone(n.meta.Partitions)
		beforeEpoch := n.meta.MapEpoch
		for _, table := range m.Partitions {
			h := holderOf(&table, node)
			cur, held := n.meta.Partitions[table.ID]
			switch {
			case h == nil:
				if held && cur == PartReleased {
					delete(n.meta.Partitions, table.ID)
				}
			case h.State == Joining || h.State == Serving:
				switch cur {
				case "", PartReleased:
					n.meta.Partitions[table.ID] = PartAdopting
				case PartAdopting:
					n.meta.Partitions[table.ID] = PartCatchingUp
				case PartCatchingUp:
					n.meta.Partitions[table.ID] = PartServing
				}
			case h.State == Leaving:
				switch cur {
				case PartServing, PartAdopting, PartCatchingUp:
					n.meta.Partitions[table.ID] = PartDraining
				case PartDraining:
					n.meta.Partitions[table.ID] = PartReleased
				}
			}
		}
		n.meta.MapGeneration, n.meta.MapEpoch = m.Generation, m.Epoch
		changed = changed || !maps.Equal(before, n.meta.Partitions) || beforeEpoch != m.Epoch
	}
	return changed
}

// settle ticks and acts until neither the map nor any lease changes, failing
// past limit rounds, and answers how many rounds it took. Every round is
// checked.
func (s *sim) settle(limit int) int {
	s.t.Helper()
	for round := 1; round <= limit; round++ {
		changed := s.tick()
		s.check()
		if acted := s.act(); !changed && !acted {
			return round
		}
	}
	s.t.Fatalf("the map had not settled after %d rounds", limit)
	return limit
}

// checkServed holds the invariant every round keeps while every node is up:
// a partition some node has served is served by at least one node whose lease
// says so — the make-before-break the convergence exists for.
func (s *sim) checkServed() {
	s.t.Helper()
	s.checkWith(func(n *simNode) bool { return !n.down })
}

// checkCopies is checkServed counting the copies on nodes that are down too:
// a node that is down serves nothing, but its copy is intact on its disk, and
// what the maintainer must never do is have the LAST copy anywhere let go.
func (s *sim) checkCopies() {
	s.t.Helper()
	s.checkWith(func(*simNode) bool { return true })
}

func (s *sim) checkWith(counts func(*simNode) bool) {
	s.t.Helper()
	for _, table := range s.state.Map.Partitions {
		copies := 0
		for _, n := range s.nodes {
			if counts(n) && n.meta.Partitions[table.ID] == PartServing {
				copies++
			}
		}
		if s.served[table.ID] && copies == 0 {
			s.t.Fatalf("after tick %d no node holds a serving copy of %s, which was served",
				s.ticks, table.ID)
		}
		if copies > 0 {
			s.served[table.ID] = true
		}
	}
}

// converged fails the test unless every partition is held by exactly its
// target, every holder serving by the map's account and its own.
func (s *sim) converged() {
	s.t.Helper()
	m := s.state.Map
	targets := m.targets()
	for g, table := range m.Partitions {
		var holders []string
		for _, h := range table.Holders {
			if h.State != Serving {
				n := s.nodes[h.Node]
				s.t.Fatalf("%s's holder %s is %s (since %d) after settling at epoch %d; its lease "+
					"says %q at map epoch %d, down %v; the target is %v; members %+v", table.ID, h.Node,
					h.State, h.Since, m.Epoch, n.meta.Partitions[table.ID], n.meta.MapEpoch, n.down,
					targets[g], m.Members)
			}
			if said := s.nodes[h.Node].meta.Partitions[table.ID]; said != PartServing {
				s.t.Fatalf("%s's holder %s says %q after settling", table.ID, h.Node, said)
			}
			holders = append(holders, h.Node)
		}
		want := slices.Sorted(slices.Values(targets[g]))
		if !slices.Equal(holders, want) {
			s.t.Fatalf("%s is held by %v and its target is %v", table.ID, holders, want)
		}
	}
}

// holding is the partitions node holds in the map, by state.
func (s *sim) holding(node string) map[HolderState]int {
	out := map[HolderState]int{}
	for _, table := range s.state.Map.Partitions {
		if h := holderOf(&table, node); h != nil {
			out[h.State]++
		}
	}
	return out
}

// nodes is n node ids, data-00 onwards.
func nodeIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("data-%02d", i)
	}
	return out
}
