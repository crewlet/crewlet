package queries

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	seatplacement "github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The estate question: which data nodes hold each partition of the replicated
// estate, and what each of them says about its part in holding it.
//
// # One typed value, rendered in one place
//
// Three readers — `GET /estate` (and the socket's query of the same name), the
// dashboard's Estate screen and `crewlet estate map` — and the gesture routes'
// answers, which render a member and a hold the same way. So the shape is
// [FleetEstate] and the rendering [RenderEstate]: the command line's tests
// serve what it returns, and the dashboard's suite reads the golden
// internal/api writes from it (`internal/api/testdata/estate_answer.json`).
//
// # Two sources, joined by node
//
// The STORED MAP — never this node's watched copy, for the object map's reason:
// every node gives the same answer — says who is a member, who holds each
// partition in which state and since which epoch, and what an operator has
// asked. Each data node's ESTATE LEASE (coord.ClassEstate) says what the map
// cannot: whether its store is healthy, which layout it runs, the map it last
// acted on, and what it holds of each partition in its own words. The lease is
// what the maintainer reads membership from, so a member with no live lease is
// exactly one it is counting absent.
//
// # Five states, and layout 0 is one of them
//
// A fleet with no map is two different fleets, and the answer names which: at
// layout 0 the estate is not divided and every data node holds the whole of it
// — the state this build runs, answered with the live data nodes that hold it —
// and at a partitioned layout no map has been written yet. Each state that is
// not a placed map carries the sentence every surface gives it, so an operator
// reads the same words on the screen, on the command line and in a gesture's
// refusal.

// EstateMapState is which of the five things the estate was when the question
// was asked.
type EstateMapState string

const (
	// EstateMapUnavailable is a coordination store that would not give the
	// map up, or a lease listing it could not read.
	EstateMapUnavailable EstateMapState = "unavailable"

	// EstateMapWhole is layout 0: the estate is not divided, every data
	// node holds the whole of it, and there is no map.
	EstateMapWhole EstateMapState = "whole"

	// EstateMapNone is a partitioned layout whose first map has not been
	// written: nothing is placed yet.
	EstateMapNone EstateMapState = "no_map"

	// EstateMapUnreadable is a map this build cannot place by — one a newer
	// build wrote during an upgrade.
	EstateMapUnreadable EstateMapState = "unreadable"

	// EstateMapPlaced is a map read and rendered: every [PlacedEstate]
	// field is present.
	EstateMapPlaced EstateMapState = "placed"
)

// EstateMapStates is every state this build renders.
func EstateMapStates() []EstateMapState {
	return []EstateMapState{EstateMapUnavailable, EstateMapWhole, EstateMapNone,
		EstateMapUnreadable, EstateMapPlaced}
}

// Valid reports whether s is a state this build renders.
func (s EstateMapState) Valid() bool { return slices.Contains(EstateMapStates(), s) }

// The sentences the two states with nothing to show carry. Layout 0's is
// [partmap.WholeEstate] and a map not yet written is [partmap.Unplaced].
const (
	estateUnavailableDetail = "the estate map or the estate leases could not be read from " +
		"the coordination store, so where each partition is held is unknown from here — " +
		"ask again, or ask another node"
	estateUnreadableDetail = "the estate map was written by a newer build than this " +
		"node's, which does not read it — ask a node running that build, or once the " +
		"upgrade has finished"
)

// FleetEstate is the estate question's answer.
//
// FLAT ON THE WIRE, with a state's own fields present exactly when it is that
// state: the embedded pointers are inlined when set and absent when not, so a
// count whose zero is a reading — no partition unserved — is always written for
// a placed map and never for any other.
type FleetEstate struct {
	State EstateMapState `json:"state"`

	// Layout is the layout number the answer describes: the map's where one
	// is placed, and the one the answering node runs otherwise.
	Layout int `json:"layout"`

	// Detail is what the state means, in the words every surface gives it —
	// for every state but a placed map, which says it itself.
	Detail string `json:"detail,omitempty"`

	*WholeEstate
	*PlacedEstate
}

// WholeEstate is layout 0: the one partition, and the data nodes that each hold
// it whole.
type WholeEstate struct {
	// Partition is the one partition layout 0 has.
	Partition string `json:"partition"`

	// Holders is every live data node, by node — each holds the whole
	// estate — as the fleet's presence names them: the answer layout 0's
	// routing gives, never the estate leases, which a build from before
	// them does not claim while it serves the whole estate.
	Holders []WholeHolder `json:"holders"`
}

// WholeHolder is one data node holding the whole estate.
type WholeHolder struct {
	Node string `json:"node"`

	// Lease is what its estate lease says, absent for a node that claims
	// none — a build from before the lease, which holds and serves the
	// whole estate all the same.
	Lease *EstateLease `json:"lease,omitempty"`

	// Reports is what that lease says of the one partition, absent where
	// it says nothing of it.
	Reports partmap.PartitionState `json:"reports,omitempty"`
}

// EstateLease is what one data node's estate lease says of itself.
type EstateLease struct {
	// Weight is the share of the estate it offers.
	Weight int `json:"weight"`

	// Layout is the layout it says it runs, absent when its lease does not
	// say.
	Layout *int `json:"layout,omitempty"`

	// Healthy is whether its store says it can hold partitions, absent when
	// the lease does not say — which the map counts exactly as a failed
	// store — and Detail what it said.
	Healthy *bool  `json:"healthy,omitempty"`
	Detail  string `json:"detail,omitempty"`

	// Able is whether the map counts it present and healthy
	// ([partmap.Able]): a node outside it is, to the map, one that is not
	// there.
	Able bool `json:"able"`

	// MapEpoch is the map epoch it last acted on, absent when that was not
	// the placed map's generation — no proof of having read this one — and
	// always at layout 0, which has no map to act on.
	MapEpoch uint64 `json:"map_epoch,omitempty"`

	// FreeBytes is what is free on the volume its partition files are on.
	FreeBytes int64 `json:"free_bytes"`

	// Building is the held partitions whose lexical index is still being
	// built there, so a search prefers another holder.
	Building []string `json:"building,omitempty"`
}

// PlacedEstate is a placed map as the estate question renders it.
type PlacedEstate struct {
	// Generation is the map's lineage: a map written again after its key
	// was lost is a new one, whose epochs start again. It is also what a
	// hold and a release are confirmed by.
	Generation uuid.UUID `json:"generation"`

	// Epoch counts changes to the holder table and nothing else.
	Epoch uint64 `json:"epoch"`

	// Spaces is how the layout divides the estate: each space, its
	// partition count and the domains with a log in each partition.
	Spaces []statelog.SpaceLayout `json:"spaces"`

	// Replicas is how many copies of each partition the company asks for,
	// and Copies how many the map places — fewer while it has fewer
	// placeable members than that.
	Replicas int `json:"replicas"`
	Copies   int `json:"copies"`

	// FailureDomain is the node label a partition's copies are spread
	// across, absent for none; DistinctDomains how many of its values the
	// placeable members span; DomainLimited whether that is fewer than a
	// partition has copies.
	FailureDomain   string `json:"failure_domain,omitempty"`
	DistinctDomains int    `json:"distinct_domains"`
	DomainLimited   bool   `json:"domain_limited"`

	// Hold is an operator's hold in force now, absent when there is none.
	Hold *MapHold `json:"hold,omitempty"`

	// Balance is how evenly the map spreads the partitions over its
	// members' weights, as last measured, absent for a map nothing has
	// measured.
	Balance *EstateBalance `json:"balance,omitempty"`

	// Unserved and Short count the partitions no copy can answer for and
	// the partitions fewer copies can answer for than their target has
	// ([partmap.Coverage]); Joining and Leaving the holders in those
	// states; Moves the operator's moves in force.
	Unserved int `json:"unserved"`
	Short    int `json:"short"`
	Joining  int `json:"joining"`
	Leaving  int `json:"leaving"`
	Moves    int `json:"moves"`

	// Members are the map's members in node order.
	Members []EstateMember `json:"members"`

	// Removed are the nodes the map removed for absence, still remembers,
	// and has not seen back.
	Removed []MapRemoval `json:"removed"`

	// Partitions are every partition of the layout, in its order.
	Partitions []EstatePartition `json:"partitions"`
}

// EstateBalance is the map's last measurement of its members' partitions
// against what their weights entitle them to, and the tolerance it was asked
// for — the finest the partition count promises for the fleet it balanced.
// CONVERGED FALSE IS A MEASUREMENT, NEVER A FAULT: nothing alarms on it.
type EstateBalance struct {
	DeviationPercent float64 `json:"deviation_percent"`
	TolerancePercent float64 `json:"tolerance_percent"`
	Converged        bool    `json:"converged"`
	Rounds           int     `json:"rounds"`
}

// EstateMember is one member as the estate question renders it: the map's view
// of it, what it holds, and what its own estate lease reports.
type EstateMember struct {
	MapMember

	// SharePercent is its measured share of every partition copy the map's
	// draw places, 0..100 — what its weight bought once the balancer ran,
	// and 0 for a member out or on probation.
	SharePercent float64 `json:"share_percent"`

	// Serving, Joining and Leaving count the partitions the map lists it
	// holding in each state.
	Serving int `json:"serving"`
	Joining int `json:"joining"`
	Leaving int `json:"leaving"`

	// MovedOff is the partitions an operator moved off it, in the layout's
	// order.
	MovedOff []string `json:"moved_off,omitempty"`

	// Live is whether it holds a live estate lease offering a share — the
	// reading the maintainer counts presence by — and Lease what that lease
	// says.
	Live  bool         `json:"live"`
	Lease *EstateLease `json:"lease,omitempty"`
}

// EstatePartition is one partition as the estate question renders it.
type EstatePartition struct {
	// ID is its name, and Space the space it is in.
	ID    string `json:"id"`
	Space string `json:"space"`

	// Target is the nodes it should be held by, primary first.
	Target []string `json:"target"`

	// Serving is how many copies can answer for it now — holders the map
	// lists serving whose node it counts present and healthy — and Wanted
	// how many its target has ([partmap.Coverage]).
	Serving int `json:"serving"`
	Wanted  int `json:"wanted"`

	// Holders are the nodes holding it in any state, by node.
	Holders []EstateHolder `json:"holders"`

	// Moves are the operator's moves of it off a node, by node.
	Moves []EstateMove `json:"moves,omitempty"`
}

// EstateHolder is one node's place in a partition's holder table, beside what
// its own lease says of the partition.
type EstateHolder struct {
	Node string `json:"node"`

	// State is what the map says it is doing with the partition, and Since
	// the map epoch at which it entered that state.
	State partmap.HolderState `json:"state"`
	Since uint64              `json:"since"`

	// Reports is what its own estate lease says of the partition, absent
	// when it says nothing of it: no live lease, another layout, or a lease
	// that does not list it.
	Reports partmap.PartitionState `json:"reports,omitempty"`

	// Able is whether the map counts its node present and healthy.
	Able bool `json:"able"`
}

// EstateMove is an operator's move of a partition off a node: who asked, why
// and when — for display, never compared.
type EstateMove struct {
	Node   string    `json:"node"`
	By     string    `json:"by"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// RenderEstateMove is the move of p off node in force in m, or nil.
func RenderEstateMove(m partmap.Map, p statelog.PartitionID, node string) *EstateMove {
	g, ok := m.Moves[p.String()][node]
	if !ok {
		return nil
	}
	return &EstateMove{Node: node, By: g.By, Reason: g.Reason, At: g.At}
}

// RenderEstateWhole is layout 0 as the estate question renders it: every live
// data node, from the presence leases, each with what its estate lease says.
func RenderEstateWhole(running statelog.Layout, presence, estate []coord.Lease) FleetEstate {
	parts := running.Partitions()
	partition := ""
	if len(parts) > 0 {
		partition = parts[0].String()
	}
	reports := estateReports(estate)
	able := partmap.Able(presencesOf(estate), running.Number)
	whole := &WholeEstate{Partition: partition, Holders: []WholeHolder{}}
	for _, lease := range presence {
		profile, ok := seatplacement.FromLease(lease)
		if !ok || !profile.HoldsData() {
			continue
		}
		row := WholeHolder{Node: profile.ID}
		if p, ok := reports[profile.ID]; ok {
			row.Lease = renderEstateLease(p.Meta, able[profile.ID], uuid.Nil)
			row.Reports = p.Meta.Partitions[partition]
		}
		whole.Holders = append(whole.Holders, row)
	}
	slices.SortFunc(whole.Holders, func(a, b WholeHolder) int { return strings.Compare(a.Node, b.Node) })
	return FleetEstate{State: EstateMapWhole, Layout: running.Number,
		Detail: partmap.WholeEstate, WholeEstate: whole}
}

// RenderEstate is a placed map as the estate question renders it, joined with
// the live estate leases. One for a node the map does not hold is ignored, as
// the maintainer adds it at its next tick.
func RenderEstate(state partmap.MapState, estate []coord.Lease, now time.Time) FleetEstate {
	m := state.Map
	d := m.Draw()
	live := presencesOf(estate)
	reports := estateReports(estate)
	able := partmap.Able(live, m.Layout.Number)
	coverage := m.Coverage(live)
	targets := m.Targets()
	parts := m.Layout.Partitions()

	placed := &PlacedEstate{
		Generation: m.Generation, Epoch: m.Epoch,
		Spaces:   m.Layout.Spaces,
		Replicas: m.Replicas, Copies: m.Size(),
		FailureDomain:   m.FailureDomain,
		DistinctDomains: d.DistinctDomains(), DomainLimited: d.DomainLimited(),
		Hold:       RenderHold(state.State, now),
		Balance:    renderEstateBalance(state.Balance),
		Members:    make([]EstateMember, 0, len(m.Members)),
		Removed:    RenderRemoved(state.State, d),
		Partitions: make([]EstatePartition, 0, len(m.Partitions)),
	}
	held := map[string]map[partmap.HolderState]int{}
	movedOff := map[string][]string{}
	for g, table := range m.Partitions {
		row := EstatePartition{
			ID: table.ID, Space: string(parts[g].Space), Target: targets[g],
			Serving: len(coverage[g].Serving), Wanted: coverage[g].Wanted,
			Holders: make([]EstateHolder, 0, len(table.Holders)),
		}
		if row.Target == nil {
			row.Target = []string{}
		}
		for _, h := range table.Holders {
			holder := EstateHolder{Node: h.Node, State: h.State, Since: h.Since, Able: able[h.Node]}
			if p, ok := reports[h.Node]; ok && p.Meta.Layout != nil && *p.Meta.Layout == m.Layout.Number {
				holder.Reports = p.Meta.Partitions[table.ID]
			}
			row.Holders = append(row.Holders, holder)
			if held[h.Node] == nil {
				held[h.Node] = map[partmap.HolderState]int{}
			}
			held[h.Node][h.State]++
			switch h.State {
			case partmap.Joining:
				placed.Joining++
			case partmap.Leaving:
				placed.Leaving++
			}
		}
		for _, node := range sortedKeys(m.Moves[table.ID]) {
			row.Moves = append(row.Moves, *RenderEstateMove(m, parts[g], node))
			movedOff[node] = append(movedOff[node], table.ID)
			placed.Moves++
		}
		if coverage[g].Unserved() {
			placed.Unserved++
		}
		if coverage[g].Short() {
			placed.Short++
		}
		placed.Partitions = append(placed.Partitions, row)
	}

	copies := d.Layout().Copies()
	total := 0
	for _, n := range copies {
		total += n
	}
	for _, member := range m.Members {
		view, _ := RenderMember(state.State, d, member.Node)
		row := EstateMember{MapMember: view, MovedOff: movedOff[member.Node],
			Serving: held[member.Node][partmap.Serving],
			Joining: held[member.Node][partmap.Joining],
			Leaving: held[member.Node][partmap.Leaving]}
		if total > 0 {
			row.SharePercent = 100 * float64(copies[member.Node]) / float64(total)
		}
		if p, ok := reports[member.Node]; ok {
			row.Live = true
			row.Lease = renderEstateLease(p.Meta, able[member.Node], m.Generation)
		}
		placed.Members = append(placed.Members, row)
	}
	return FleetEstate{State: EstateMapPlaced, Layout: m.Layout.Number, PlacedEstate: placed}
}

// renderEstateBalance is b as the estate question shows it, or nil for a map
// nothing has measured — every map is balanced when it is first written, so
// this is a record written before balances were.
func renderEstateBalance(b partmap.Balance) *EstateBalance {
	if b.Tolerance == 0 {
		return nil
	}
	return &EstateBalance{
		DeviationPercent: 100 * b.Deviation,
		TolerancePercent: 100 * b.Tolerance,
		Converged:        b.Converged,
		Rounds:           b.Rounds,
	}
}

// renderEstateLease is what a lease says, as the estate question shows it. Its
// map epoch is an epoch of the map named by generation only when the lease
// names that generation — and for layout 0, which has no map, none at all.
func renderEstateLease(m partmap.Meta, able bool, generation uuid.UUID) *EstateLease {
	out := &EstateLease{Weight: m.Weight, Layout: m.Layout, Healthy: m.Healthy,
		Detail: m.Detail, Able: able, FreeBytes: m.FreeBytes, Building: m.Building}
	if generation != uuid.Nil && m.MapGeneration == generation {
		out.MapEpoch = m.MapEpoch
	}
	return out
}

// estateReports is every live estate lease that offers a share, by node — the
// maintainer's own reading ([partmap.PresenceOf]), so a member whose lease
// offers none reads as not live here exactly as it counts absent there.
func estateReports(leases []coord.Lease) map[string]partmap.Presence {
	out := make(map[string]partmap.Presence, len(leases))
	for _, lease := range leases {
		if p, ok := partmap.PresenceOf(lease); ok {
			if _, twice := out[p.Node]; !twice {
				out[p.Node] = p
			}
		}
	}
	return out
}

// presencesOf is every live estate lease that offers a share, in the listing's
// order.
func presencesOf(leases []coord.Lease) []partmap.Presence {
	out := make([]partmap.Presence, 0, len(leases))
	for _, lease := range leases {
		if p, ok := partmap.PresenceOf(lease); ok {
			out = append(out, p)
		}
	}
	return out
}

// sortedKeys is a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// EstateReader is the estate map as the estate question reads it: the stored
// record, read as every node reads it, and the layout the answering node runs —
// what a fleet with no map is. [engine.EstateControl] is one.
type EstateReader interface {
	EstateMap(ctx context.Context) (coord.EstateMapRecord, bool, error)
	Running() statelog.Layout
}

// estate answers the estate question.
//
// FIVE STATES NAMED APART, never folded into an empty map — see the file's doc.
// A lease listing that could not be read is UNAVAILABLE rather than a map with
// every member gone: the leases are what says who is live, and a map drawn
// without them would show a fleet that has stopped.
func (s Sources) estate(ctx context.Context, _ Params) (any, error) {
	running := s.Estate.Running()
	unavailable := FleetEstate{State: EstateMapUnavailable, Layout: running.Number,
		Detail: estateUnavailableDetail}
	leases, err := s.Coord.ListLive(ctx, coord.ClassEstate)
	if err != nil {
		log.WarnContext(ctx, "estate_leases_unavailable", "error", err)
		return unavailable, nil
	}
	rec, found, err := s.Estate.EstateMap(ctx)
	if err != nil {
		log.WarnContext(ctx, "estate_map_unavailable", "error", err)
		return unavailable, nil
	}
	if !found {
		if running.Number != 0 {
			return FleetEstate{State: EstateMapNone, Layout: running.Number,
				Detail: partmap.Unplaced(running.Number)}, nil
		}
		presence, listErr := s.Coord.ListLive(ctx, coord.ClassNode)
		if listErr != nil {
			log.WarnContext(ctx, "estate_presence_unavailable", "error", listErr)
			return unavailable, nil
		}
		return RenderEstateWhole(running, presence, leases), nil
	}
	state, err := partmap.DecodeMapState(rec.Value)
	if err != nil {
		log.WarnContext(ctx, "estate_map_unreadable", "error", err)
		return FleetEstate{State: EstateMapUnreadable, Layout: running.Number,
			Detail: estateUnreadableDetail}, nil
	}
	return RenderEstate(state, leases, s.clock()), nil
}
