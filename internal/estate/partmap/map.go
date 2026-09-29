package partmap

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// HolderState is what the map says a holder is doing with a partition.
type HolderState string

const (
	// Joining is a node the map has named to hold a partition and that has
	// not yet said it serves it: it is adopting a copy or catching up.
	// Nobody routes to it.
	Joining HolderState = "joining"

	// Serving is a node that holds the partition and answers for it:
	// routers send it the partition's reads and writes.
	Serving HolderState = "serving"

	// Leaving is a node the map has told to give the partition up — or
	// one that is giving it up on its own. It keeps serving until it starts
	// its leave, and is removed once it reports the partition released.
	Leaving HolderState = "leaving"
)

// HolderStates is every holder state, for the enum's own validation and for a
// surface that declares its own copy of the set.
var HolderStates = []HolderState{Joining, Serving, Leaving}

// Valid reports whether s is a holder state this build knows.
func (s HolderState) Valid() bool { return slices.Contains(HolderStates, s) }

// Holder is one node's place in a partition's holder table.
type Holder struct {
	// Node is the node's id.
	Node string `json:"node"`

	// State is what the map says it is doing with the partition.
	State HolderState `json:"state"`

	// Since is the map epoch at which it entered State. A node proves it
	// has ACTED on the map that put it there by reporting a map epoch at
	// least this one — see the package doc's two leave conditions.
	Since uint64 `json:"since"`
}

// Partition is one partition's holder table.
type Partition struct {
	// ID is the partition's name, statelog.PartitionID's String.
	ID string `json:"id"`

	// Holders are the nodes holding it in any state, sorted by node.
	Holders []Holder `json:"holders,omitempty"`
}

// Map is the fleet's estate placement: which data nodes hold each partition
// of the replicated estate, and everything the target placement is drawn
// from.
//
// A VALUE, identical on every node that read the same version of it, and
// every function on it is pure. Its draw is BUILT, not stored ([Map.Draw]):
// the salt and the groups are what make it the ESTATE map, the same for every
// estate map there will ever be, and the groups follow from the layout, so a
// stored copy of either could only ever disagree with the layout.
type Map struct {
	// Generation names this map's lineage: minted when a map is first
	// created, so a map written after the stored key was lost and
	// recreated — whose epochs start again — is told apart from the one a
	// node acted on.
	Generation uuid.UUID `json:"generation"`

	// Epoch counts changes to the HOLDER TABLE and nothing else: see the
	// package doc.
	Epoch uint64 `json:"epoch"`

	// Layout is how the estate is divided: fixed when the map is created,
	// and never layout 0, which no map places — every data node holds its
	// one partition whole.
	Layout statelog.Layout `json:"layout"`

	// Replicas is how many copies of each partition the company asks for,
	// 1..placement.MaxReplicas; each partition's target has [Map.Size] of
	// them, bounded by the members it can place on.
	Replicas int `json:"replicas"`

	// FailureDomain is the node label a partition's copies are spread
	// across, and empty for no constraint.
	FailureDomain string `json:"failure_domain,omitempty"`

	// Members are the data nodes in the map, sorted by node, unique.
	Members []placement.Member `json:"members"`

	// Partitions is one holder table per partition of the layout, in the
	// layout's order.
	Partitions []Partition `json:"partitions"`

	// Moves is an operator's per-partition moves, by partition and then by
	// node: each names a node the partition's target skips — so its copy
	// there is rebuilt elsewhere and then released — and who asked, why
	// and when. It lasts until the operator cancels it, or the node leaves
	// the map.
	Moves map[string]map[string]membership.Gesture `json:"moves,omitempty"`
}

// Draw is the map as a draw: its members, copies and failure domain, under
// the estate map's salt, over its layout's partitions.
func (m Map) Draw() placement.Draw {
	return placement.Draw{
		Salt:          placement.EstateSalt,
		Replicas:      m.Replicas,
		FailureDomain: m.FailureDomain,
		Members:       m.Members,
		Groups:        GroupsOf(m.Layout),
	}
}

// Size is how many copies each partition's target has, before any move
// ([placement.Draw.Size]).
func (m Map) Size() int { return m.Draw().Size() }

// Validate refuses a map nobody may place by, naming what is wrong with it.
func (m Map) Validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", placement.ErrInvalid, fmt.Sprintf(format, args...))
	}
	switch {
	case m.Generation == uuid.Nil:
		return fail("it names no generation")
	case m.Epoch == 0:
		return fail("its epoch is 0; an estate map's epochs count from 1")
	}
	if err := m.Layout.Validate(); err != nil {
		return fmt.Errorf("%w: %w", placement.ErrInvalid, err)
	}
	if m.Layout.Number == 0 {
		return fail("it places layout 0, which no map places: every data node " +
			"holds that layout's one partition whole")
	}
	d := m.Draw()
	if err := d.Validate(); err != nil {
		return err
	}
	parts := m.Layout.Partitions()
	if len(m.Partitions) != len(parts) {
		return fail("it has %d partition tables and layout %d has %d partitions",
			len(m.Partitions), m.Layout.Number, len(parts))
	}
	for i, p := range m.Partitions {
		if want := parts[i].String(); p.ID != want {
			return fail("partition table %d is %q, and the layout's partition %d is %q",
				i, p.ID, i, want)
		}
		for j, h := range p.Holders {
			switch {
			case j > 0 && p.Holders[j-1].Node >= h.Node:
				return fail("%s's holders are not sorted and unique at %q", p.ID, h.Node)
			case !h.State.Valid():
				return fail("%s's holder %q is in state %q, which this build does not know",
					p.ID, h.Node, h.State)
			case !d.Holds(h.Node):
				return fail("%s is held by %q, which is not a member of the map", p.ID, h.Node)
			case h.Since == 0 || h.Since > m.Epoch:
				return fail("%s's holder %q entered its state at epoch %d, outside 1..%d",
					p.ID, h.Node, h.Since, m.Epoch)
			}
		}
	}
	for id, nodes := range m.Moves {
		p, err := statelog.ParsePartitionID(id)
		if err != nil {
			return fail("a move names %q: %v", id, err)
		}
		if _, ok := groupOf(m.Layout, p); !ok {
			return fail("a move names %s, which layout %d does not have", id, m.Layout.Number)
		}
		if len(nodes) == 0 {
			return fail("the moves of %s name no node", id)
		}
		for node := range nodes {
			if !d.Holds(node) {
				return fail("a move of %s names %q, which is not a member of the map", id, node)
			}
		}
	}
	return nil
}

// table is p's holder table, and false for a partition the layout does not
// have.
func (m Map) table(p statelog.PartitionID) (int, *Partition, bool) {
	g, ok := groupOf(m.Layout, p)
	if !ok || g >= len(m.Partitions) {
		return 0, nil, false
	}
	return g, &m.Partitions[g], true
}

// drawFor is the draw p's target is taken from: the map's, with the nodes an
// operator moved p off marked as taking no copies — for p alone — AS FAR AS THE
// OTHER MEMBERS CAN HOLD p's COPIES ([Map.MoveWaiting]).
func (m Map) drawFor(p statelog.PartitionID) placement.Draw {
	d := m.Draw()
	return without(d, m.movedOff(p, d))
}

// drawWithoutEveryMove is p's draw with EVERY node an operator moved p off
// marked as taking no copies, whether or not the others can hold them — what
// [Move] measures a new move against, and nothing else.
func (m Map) drawWithoutEveryMove(p statelog.PartitionID) placement.Draw {
	return without(m.Draw(), m.Moves[p.String()])
}

// without is d with every member named in nodes marked as taking no copies.
func without[V any](d placement.Draw, nodes map[string]V) placement.Draw {
	if len(nodes) == 0 {
		return d
	}
	d.Members = slices.Clone(d.Members)
	for i := range d.Members {
		if _, ok := nodes[d.Members[i].Node]; ok {
			d.Members[i].Out = true
		}
	}
	return d
}

// movedOff is the nodes p's target is drawn without: every node an operator
// moved p off, EXCEPT where drawing without all of them would leave p fewer
// copies than the map places — d.Size() — and then the moved nodes p's
// ranking in d prefers are kept, as many as that shortfall.
//
// A MOVE MOVES A COPY; IT NEVER DROPS ONE. [Move] refuses one with no member
// to rebuild the copy on, but the members change after it: take another member
// out, or lose one to absence, and a target drawn without every moved node
// holds p a copy short — for as long as the move stood, every other partition
// at the company's copies, and a member able to hold the missing copy sitting
// idle for no reason but an earlier gesture. So the moved node holds p again,
// the move stays on the map, and it takes effect once a member returns. Kept in
// p's RANKING order rather than by which node still holds a copy, because a
// target is a function of the draw alone: one read off the holder table would
// move each time the holders moved toward it.
func (m Map) movedOff(p statelog.PartitionID, d placement.Draw) map[string]bool {
	moved := m.Moves[p.String()]
	g, _, ok := m.table(p)
	if !ok || len(moved) == 0 {
		return nil
	}
	placeable := 0
	for _, member := range d.Members {
		if member.Placeable() {
			placeable++
		}
	}
	// THE MOVED NODES THAT WOULD HOLD A COPY BUT FOR THE MOVE, in p's
	// ranking: a moved node the map places nothing on already holds none.
	var placing []string
	for _, node := range d.Ranked(g) {
		if _, isMoved := moved[node]; !isMoved {
			continue
		}
		if member, _ := d.Member(node); member.Placeable() {
			placing = append(placing, node)
		}
	}
	keep := max(0, len(placing)-(placeable-d.Size()))
	out := make(map[string]bool, len(moved))
	for node := range moved {
		out[node] = true
	}
	for _, node := range placing[:keep] {
		delete(out, node)
	}
	return out
}

// MoveWaiting reports whether an operator's move of p off node is on the map
// but NOT IN EFFECT: without node the members left could not hold p's copies,
// so p's target names node again until a member returns ([Map.movedOff]). False
// for a move in effect and for a move that does not exist.
func (m Map) MoveWaiting(p statelog.PartitionID, node string) bool {
	if _, moved := m.Moves[p.String()][node]; !moved {
		return false
	}
	return !m.movedOff(p, m.Draw())[node]
}

// Target is the nodes p SHOULD be held by, primary first: the up set of p's
// group in the map's draw, drawn without the nodes an operator moved p off
// — so a move rebuilds the copy on the member p's ranking offers next, spread
// across failure domains as far as the members allow, rather than leaving p
// one short ([Move] refuses a move with no member to rebuild on, and a move
// the members have since left no room for waits: [Map.MoveWaiting]). So every
// partition's target has [Map.Size] nodes, moved or not. Nil for a partition
// the layout does not have.
//
// It hashes every member on each call; [Map.targets] computes every
// partition's at once.
func (m Map) Target(p statelog.PartitionID) []string {
	g, _, ok := m.table(p)
	if !ok {
		return nil
	}
	d := m.drawFor(p)
	return slices.Clone(d.Ranked(g)[:d.Size()])
}

// targets is every partition's [Map.Target], in the layout's order, from one
// layout of the draw.
func (m Map) targets() [][]string {
	d := m.Draw()
	l := d.Layout()
	parts := m.Layout.Partitions()
	out := make([][]string, len(parts))
	for g, p := range parts {
		if len(m.Moves[p.String()]) == 0 {
			out[g] = l.Up(g)
			continue
		}
		moved := m.drawFor(p)
		out[g] = moved.Ranked(g)[:moved.Size()]
	}
	return out
}

// Serving is the nodes that serve p — its holders in state [Serving] — in the
// order a router tries them: the target's order first, then the rest of p's
// ranking. Nil for a partition the layout does not have, and empty for one
// nobody serves.
func (m Map) Serving(p statelog.PartitionID) []string {
	g, table, ok := m.table(p)
	if !ok {
		return nil
	}
	order := map[string]int{}
	for _, node := range append(m.Target(p), m.Draw().Ranked(g)...) {
		if _, seen := order[node]; !seen {
			order[node] = len(order)
		}
	}
	var out []string
	for _, h := range table.Holders {
		if h.State == Serving {
			out = append(out, h.Node)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return order[a] - order[b] })
	return out
}

// HoldersOf is p's holder table, in every state — which is what the trim
// counts, since a joining node's tail is exactly what must not be trimmed —
// sorted by node. Nil for a partition the layout does not have. The slice is
// the caller's own.
func (m Map) HoldersOf(p statelog.PartitionID) []Holder {
	_, table, ok := m.table(p)
	if !ok {
		return nil
	}
	return slices.Clone(table.Holders)
}

// Clone is a copy sharing nothing with m.
func (m Map) Clone() Map {
	out := m
	out.Layout.Spaces = slices.Clone(m.Layout.Spaces)
	for i, space := range out.Layout.Spaces {
		out.Layout.Spaces[i].Domains = slices.Clone(space.Domains)
	}
	out.Members = slices.Clone(m.Members)
	out.Partitions = make([]Partition, len(m.Partitions))
	for i, p := range m.Partitions {
		out.Partitions[i] = Partition{ID: p.ID, Holders: slices.Clone(p.Holders)}
	}
	if m.Moves != nil {
		out.Moves = make(map[string]map[string]membership.Gesture, len(m.Moves))
		for id, nodes := range m.Moves {
			out.Moves[id] = maps.Clone(nodes)
		}
	}
	return out
}

// emptyTables is one empty holder table per partition of a layout.
func emptyTables(l statelog.Layout) []Partition {
	parts := l.Partitions()
	out := make([]Partition, len(parts))
	for i, p := range parts {
		out[i] = Partition{ID: p.String()}
	}
	return out
}

// sortHolders sorts a holder table by node.
func sortHolders(h []Holder) {
	slices.SortFunc(h, func(a, b Holder) int { return strings.Compare(a.Node, b.Node) })
}
