package partmap

import (
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// MaxJoinsPerNode is how many partitions a node may be joining at once where
// the join is a TRANSFER — a partition somebody already serves, whose copy
// the joiner fetches from a donor.
//
// ONE, because a join is one snapshot transfer and the transfer's credit
// window (statelog.SnapshotTransferWindow, 32 MiB in flight) was sized to keep
// ONE pipe full: a second transfer into the same node shares that pipe, so
// both finish no sooner than one after the other would, and the first copy is
// restored twice as late — while each holds its partition's trim hold and its
// donor's artefact open the whole time. A join into a partition nobody serves
// is not a transfer — there is no donor to fetch from, which is every
// partition of a new deployment, created empty and replayed from its log's
// first record — and is not counted, or a new fleet would take one join at a
// time per node to serve its estate at all: 963 copies over three nodes, 321
// in a row each.
const MaxJoinsPerNode = 1

// Input is what one maintainer tick reads.
type Input struct {
	// Layout is the layout a NEW map is created at. A map that exists keeps
	// the layout it was created at: which layout the fleet runs is
	// recorded, never assumed from one node's build.
	Layout statelog.Layout

	// Live is every live estate lease ([PresenceOf]).
	Live []Presence

	// Company is what the company's estate block says, stamped with the
	// activation it came from ([membership.Company], Block "estate"). The
	// zero Company is none: a tick then changes neither the copies nor the
	// failure domain, and creates no map.
	Company membership.Company

	// Now stamps what the record shows an operator and is what a hold
	// expires against. Nothing else compares it: absence is counted in
	// ticks.
	Now time.Time
}

// Next is the map that should follow state, given what the tick read. It is
// one TICK: every open absence is counted once, so the caller runs it once per
// [membership.TickInterval] while it holds the map's duty and writes the
// answer by compare-and-set.
//
// PURE — but for the generation it mints for a first map, which is random by
// design — so every rule is tested without a store. Each tick runs, in order:
//
//  1. Membership ([membership.Tick]): the company's copies and label, nodes
//     returning, joining, absent and removed, out and on probation, counted
//     from the live estate leases — a lease that does not say its store is
//     healthy, or runs another layout, counting as an unhealthy one.
//  2. Balance, when what the map places changed: the members' shares, at the
//     finest tolerance the layout's partition count promises for this fleet.
//  3. Holders, converged toward every partition's target, make before
//     break: see the package doc.
//
// A first map is written only from a company, at a partitioned layout, for a
// fleet with a member. changed reports whether there is anything to write at
// all — absences, holds and balances included.
func Next(state MapState, in Input) (next MapState, changed bool) {
	first := state.Map.Generation == uuid.Nil
	layout := state.Map.Layout
	if first {
		if in.Company.Validate() != nil || in.Layout.Validate() != nil || in.Layout.Number == 0 {
			// NO FIRST MAP WITHOUT A COMPANY, which would have to invent
			// the copies every partition keeps, and none for layout 0,
			// which every data node holds whole.
			return state, false
		}
		layout = in.Layout
	}
	live := membership.ByNode(in.Live, func(p Presence) membership.Presence {
		return membership.Presence{Node: p.Node, Weight: p.Meta.Weight}
	})
	presences := make([]membership.Presence, 0, len(live))
	able := map[string]bool{}
	for _, node := range slices.Sorted(maps.Keys(live)) {
		mp := live[node].membership(layout.Number)
		presences = append(presences, mp)
		if !mp.Unhealthy {
			able[node] = true
		}
	}

	before := state.Map
	ms, drawn := membership.Tick(state.State, before.Draw(), presences, in.Company, in.Now)
	if first && len(drawn.Members) == 0 {
		// NO MAP FOR A FLEET WITH NO MEMBER: an empty one says what none
		// says, at a write per tick.
		return state, false
	}

	next = state.Clone()
	next.State = ms
	next.Map.Replicas = drawn.Replicas
	next.Map.FailureDomain = drawn.FailureDomain
	next.Map.Members = drawn.Members
	if first {
		next.Map.Generation = uuid.New()
		next.Map.Layout = layout
		next.Map.Partitions = emptyTables(layout)
	}
	forgetMoves(&next.Map)
	if first || !samePlacement(before, next.Map) {
		next.Balance = rebalance(&next.Map)
	}
	at := before.Epoch + 1
	if converge(&next.Map, live, able, at) || first {
		next.Map.Epoch = at
	}
	return next, !sameState(state, next)
}

// samePlacement reports whether two maps draw every target from the same
// members: the copies, the label and every member, share and flag included.
// The holder table and the moves are not placement — see the package doc.
func samePlacement(a, b Map) bool {
	return a.Replicas == b.Replicas && a.FailureDomain == b.FailureDomain &&
		slices.Equal(a.Members, b.Members)
}

// rebalance sets the members' shares at the finest tolerance the layout
// promises for this fleet ([placement.Draw.Reachable]) and answers the
// balance.
func rebalance(m *Map) Balance {
	d := m.Draw()
	tolerance := d.Reachable(placement.DefaultTolerance)
	balanced, report := placement.Balance(d, placement.BalanceOptions{Tolerance: tolerance})
	m.Members = balanced.Members
	return Balance{Tolerance: tolerance, BalanceReport: report}
}

// forgetMoves drops every move naming a node that is no longer a member, the
// way membership forgets an operator's out when the member leaves: a node
// removed and seen again years later is a node like any other.
func forgetMoves(m *Map) {
	d := m.Draw()
	for id, nodes := range m.Moves {
		for node := range nodes {
			if !d.Holds(node) {
				delete(nodes, node)
			}
		}
		if len(nodes) == 0 {
			delete(m.Moves, id)
		}
	}
	if len(m.Moves) == 0 {
		m.Moves = nil
	}
}

// report is what one live node's lease says, as the convergence reads it.
type report struct {
	// parts is what it says of each partition, nil when it runs another
	// layout: a partition's name means another partition there.
	parts map[string]PartitionState

	// epoch is the map epoch it acted on, 0 when that was another
	// generation's — no proof of having read this one.
	epoch uint64
}

// converging is one tick's holder convergence in progress.
type converging struct {
	m       *Map
	draw    placement.Draw
	at      uint64
	reports map[string]report
	targets [][]string
	changed bool

	// reporting is every node with a live lease, in node order, so an
	// adoption walks them the same way on every holder of the duty.
	reporting []string

	// able is every node whose lease the tick counts present and healthy
	// ([Presence.membership]): the nodes a join may be named on, whose word
	// may vouch for a partition, and whose word may make them a holder or
	// a server — a copy adopted, a join promoted, a leave taken back.
	//
	// A NODE OUTSIDE IT IS AN ABSENT ONE, whatever its lease says: its store
	// has said it failed, or will not say, or it runs another layout, and
	// membership counts it exactly as a node that has gone. Its word that
	// it is GIVING A COPY UP is still taken — a drain, a join given up, a
	// release — because that is the one direction in which the word of a
	// store nobody may trust costs nothing: it moves a copy out of what
	// routers route to, never into it.
	able map[string]bool
}

// converge brings every partition's holders one make-before-break step toward
// its target, stamping every holder it changes with the epoch at, and reports
// whether it changed any. able is the live nodes the tick counts present and
// healthy.
func converge(m *Map, live map[string]Presence, able map[string]bool, at uint64) bool {
	c := &converging{m: m, draw: m.Draw(), at: at, reports: map[string]report{},
		targets: m.targets(), able: able}
	for node, p := range live {
		var r report
		if p.Meta.Layout != nil && *p.Meta.Layout == m.Layout.Number {
			r.parts = p.Meta.Partitions
		}
		if p.Meta.MapGeneration == m.Generation {
			r.epoch = p.Meta.MapEpoch
		}
		c.reports[node] = r
	}
	c.reporting = slices.Sorted(maps.Keys(c.reports))
	for g := range m.Partitions {
		c.settle(g)
	}
	c.join()
	for g := range m.Partitions {
		sortHolders(m.Partitions[g].Holders)
		if len(m.Partitions[g].Holders) == 0 {
			m.Partitions[g].Holders = nil
		}
	}
	return c.changed
}

// says is what node's lease says of partition g, and false when it has no
// live lease, runs another layout, or does not say what it holds.
func (c *converging) says(node string, g int) (PartitionState, bool) {
	r, ok := c.reports[node]
	if !ok || r.parts == nil {
		return "", false
	}
	s, ok := r.parts[c.m.Partitions[g].ID]
	return s, ok
}

// holdsNothing reports whether node's live lease, at the map's layout, lists
// what it holds and partition g is not among it.
func (c *converging) holdsNothing(node string, g int) bool {
	r, ok := c.reports[node]
	if !ok || r.parts == nil {
		return false
	}
	_, held := r.parts[c.m.Partitions[g].ID]
	return !held
}

// acted reports whether node has acted on the map that put a holder of it in
// its state at since: its lease names this map's generation at an epoch at
// least since.
func (c *converging) acted(node string, since uint64) bool {
	return c.reports[node].epoch >= since
}

// set moves a holder to a state, stamped with this tick's epoch.
func (c *converging) set(h *Holder, s HolderState) {
	h.State, h.Since = s, c.at
	c.changed = true
}

// settle is every step of one partition's convergence but the joins, which
// are rationed across the whole map ([converging.join]).
func (c *converging) settle(g int) {
	p := &c.m.Partitions[g]
	target := c.targets[g]
	inTarget := func(node string) bool { return slices.Contains(target, node) }

	// 1. A holder whose node the map no longer has is dropped: membership
	// removed it for absence, and its register row still pins the
	// partition's logs until an operator evicts it.
	kept := p.Holders[:0]
	for _, h := range p.Holders {
		if c.draw.Holds(h.Node) {
			kept = append(kept, h)
			continue
		}
		c.changed = true
	}
	p.Holders = kept

	// 2-4. What each holder says of itself.
	kept = p.Holders[:0]
	for _, h := range p.Holders {
		said, _ := c.says(h.Node, g)
		switch {
		case h.State == Serving && (said == PartDraining || said == PartReleased):
			// Leaving on its own — it saw a leave the map has since
			// taken back, or was fenced — and a leave is never
			// reversed once it drains, so the map says so too and
			// nobody routes to it. Its word is enough: the map made
			// it a server only on a lease that said it served, so a
			// drain the lease reports now came after.
			c.set(&h, Leaving)
		case h.State == Joining && (said == PartDraining || said == PartReleased) &&
			c.acted(h.Node, h.Since):
			// A joiner that, having read the map that named it, says it
			// is giving the partition up has given the join up, and
			// the map lets it go rather than hold its node's one join
			// for ever. Before it has read that map, a released is
			// what an earlier tenure left, and a rejoin read as a
			// leave would never finish.
			c.set(&h, Leaving)
		case h.State == Leaving && (said == PartReleased || c.holdsNothing(h.Node, g)) &&
			c.acted(h.Node, h.Since):
			// Gone, at its own word, having acted on the map that
			// told it to go: the release is this leave's, not an
			// earlier tenure's. A node that lists what it holds and
			// not this partition has nothing of it to release — a
			// joiner withdrawn before it adopted anything, or a node
			// that already dropped its file — and is gone as surely.
			c.changed = true
			continue
		case h.State == Joining && said == PartServing && c.acted(h.Node, h.Since) &&
			c.able[h.Node]:
			// Serving, at its own word, having acted on the map that
			// named it — the word of a node the tick counts able
			// ([converging.able]).
			c.set(&h, Serving)
		}
		kept = append(kept, h)
	}
	p.Holders = kept

	// 5. Adopt what exists: a node that says it holds the partition and is
	// not listed — after the cutover, a restore, or a node the map removed
	// for absence that came back with its files.
	//
	// AN ESTABLISHED COPY IS ADOPTED SERVING, wanted or not: it serves,
	// and it restores a copy with no transfer. One the target does not
	// want is then let go like any other server — under both conditions
	// below — rather than adopted leaving and let go at once, which would
	// leave the whole make-before-break to the node's own check before it
	// drains. A copy still being built is adopted joining where the target
	// wants it, and leaving where it does not: it serves nothing, and its
	// release fences anything it might still publish.
	//
	// ONLY ON THE WORD OF A NODE THE TICK COUNTS ABLE. A store that has
	// said it failed is no account of what it holds, and a copy adopted on
	// its word would be routed to, or be a joiner the trim counts from
	// nothing; it is adopted the first tick the node is able again.
	for _, node := range c.reporting {
		if !c.draw.Holds(node) || !c.able[node] || holderOf(p, node) != nil {
			continue
		}
		said, ok := c.says(node, g)
		if !ok {
			continue
		}
		var state HolderState
		switch {
		case said == PartServing:
			state = Serving
		case (said == PartAdopting || said == PartCatchingUp) && inTarget(node):
			state = Joining
		case said == PartAdopting || said == PartCatchingUp:
			state = Leaving
		default:
			continue
		}
		p.Holders = append(p.Holders, Holder{Node: node, State: state, Since: c.at})
		c.changed = true
	}

	for i := range p.Holders {
		h := &p.Holders[i]
		said, _ := c.says(h.Node, g)
		switch {
		case h.State == Leaving && inTarget(h.Node) && said == PartServing && c.able[h.Node]:
			// 6. Wanted again, and it has not started to leave: the
			// cheapest copy there is, the one already serving — on
			// the word of a node the tick counts able.
			c.set(h, Serving)
		case h.State == Joining && !inTarget(h.Node) && said != PartServing:
			// 7. A joiner the target moved away from before it served
			// is withdrawn at once. It served nothing, so no copy
			// anybody reads is lost — and left to finish, one stuck
			// without a donor would hold its node's only join for
			// ever.
			c.set(h, Leaving)
		}
	}

	// 8. Retire what is no longer wanted, under ADR-0019's two
	// conditions. THAT IS ALSO WHY THE LAST SERVER IS NEVER RETIRED, with
	// no count of its own: a copy goes only while a non-empty target
	// serves the partition, and a target node is never the one retired,
	// so at least the whole target is left serving.
	if !c.targetServes(p, g, target) {
		return
	}
	for i := range p.Holders {
		if h := &p.Holders[i]; h.State == Serving && !inTarget(h.Node) {
			c.set(h, Leaving)
		}
	}
}

// targetServes reports whether every node of a partition's target serves it,
// by BOTH accounts and at the same epoch — ADR-0019's two conditions for
// letting any other copy go, adapted to a map with one writer:
//
//   - (a) the map lists it serving, and its own lease — one the tick counts
//     present and healthy — says it serves the partition;
//   - (b) that lease names a map epoch at least the one at which the map made
//     it a serving holder: it has itself ACTED on the map that made it one.
//
// (b) is what (a) cannot say. Without it a node the map flipped back from
// leaving to serving — on a map it has not read yet — is still, on the older
// map it holds, about to leave; it vouches that it serves, and the copy
// retired against its word and the copy it drops are the same two copies
// gone. An empty target vouches for nothing.
//
// AND A NODE COUNTED UNHEALTHY VOUCHES FOR NOTHING, whatever its lease says of
// the partition. Membership counts it exactly as an absent one — a store that
// says it has failed, or will not say, answers for none of what it holds — and
// an absent node has no lease to vouch with. Taking its word would let the
// last good copy go on the say-so of a store that has said it cannot be
// trusted with one.
func (c *converging) targetServes(p *Partition, g int, target []string) bool {
	if len(target) == 0 {
		return false
	}
	for _, node := range target {
		h := holderOf(p, node)
		if h == nil || h.State != Serving || !c.able[node] {
			return false
		}
		if said, _ := c.says(node, g); said != PartServing {
			return false
		}
		if !c.acted(node, h.Since) {
			return false
		}
	}
	return true
}

// join adds a JOINING holder for every target node not yet holding its
// partition, walking the partitions in the layout's order so the ration below
// is spent the same way by every holder of the duty.
//
// A join into a partition somebody serves is a transfer, and a node takes at
// most [MaxJoinsPerNode] of those at once: the ones it has in flight, counted
// across the whole map, include those this tick adds. A join into a partition
// nobody serves has no donor to transfer from and is not rationed.
//
// NO JOIN IS NAMED ON A NODE THE TICK COUNTS ABSENT OR UNHEALTHY. It stays in
// the targets for membership's grace, so no copy it should hold is let go in
// the meantime, but a join named on it is one it cannot start — and a joining
// holder is counted by the trim from nothing (a joiner's tail is exactly what
// must not be trimmed), so every log of the partition would stop trimming for
// as long as the node stayed away: the grace, or under an operator's hold a
// whole day. The join is named on the first tick it is back. A join already
// named when it went away stays, for the trim's reason.
func (c *converging) join() {
	transfers := map[string]int{}
	for g := range c.m.Partitions {
		p := &c.m.Partitions[g]
		if servers(p) == 0 {
			continue
		}
		for _, h := range p.Holders {
			if h.State == Joining {
				transfers[h.Node]++
			}
		}
	}
	for g := range c.m.Partitions {
		p := &c.m.Partitions[g]
		transfer := servers(p) > 0
		for _, node := range c.targets[g] {
			if !c.able[node] {
				continue
			}
			if holderOf(p, node) != nil {
				// Held already, in some state: a joiner in flight, a
				// server, or a leaver that has started to drain and
				// joins afresh once it has gone.
				continue
			}
			if transfer && transfers[node] >= MaxJoinsPerNode {
				continue
			}
			p.Holders = append(p.Holders, Holder{Node: node, State: Joining, Since: c.at})
			c.changed = true
			if transfer {
				transfers[node]++
			}
		}
	}
}

// servers is how many of a partition's holders the map lists serving.
func servers(p *Partition) int {
	n := 0
	for _, h := range p.Holders {
		if h.State == Serving {
			n++
		}
	}
	return n
}

// holderOf is node's holder entry in a partition, and nil when it has none.
func holderOf(p *Partition, node string) *Holder {
	for i := range p.Holders {
		if p.Holders[i].Node == node {
			return &p.Holders[i]
		}
	}
	return nil
}
