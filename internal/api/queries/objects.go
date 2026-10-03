package queries

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/placement"
)

// The fleet view's object-store block: where the company's files are placed,
// and what every data node says about its part in holding them.
//
// # One typed value, rendered in one place
//
// The block has three readers — the dashboard's placement card, `crewlet
// objects status`, and the gesture routes' answers, which render a member the
// same way — and a shape built out of maps in one of them and restated in the
// others is how the gate answer came to have three copies that disagreed. So
// the shape is [FleetObjects] and the rendering is [RenderObjects]: the fleet
// question answers through it, the command line's tests serve what it returns,
// and the dashboard's suite reads the golden internal/api writes from it
// (`internal/api/testdata/objects_answer.json`).
//
// # Two sources, joined by node
//
// The STORED MAP — never this node's cached copy, for the reason the whole
// view reads the lease table: every node gives the same answer — says who is a
// member, at what weight, who is out and who is gone. Each data node's OBJECTS
// LEASE (coord.ClassObjects) says what the map cannot: whether its store is
// healthy, how far its repair and scrub have got, and how many strays it still
// holds. The lease is what the map's maintainer reads membership from, so a
// member with no live lease is exactly a member the maintainer is counting
// absent.
//
// # Absent is not zero
//
// A member whose lease reported no health, repair or scrub renders without
// that sub-object rather than with a zero one: a node whose repair has not run
// has no pending count, which is a different fact from a node holding
// everything the map places on it.

// ObjectMapState is which of the four things the stored map was when the fleet
// view read it.
//
// A STATE RATHER THAN THREE BOOLEANS: `available`, `placed` and `unreadable`
// had combinations that meant nothing (a store that did not answer, whose map
// is unreadable), and every reader had to know which of them to test first.
type ObjectMapState string

const (
	// ObjectMapUnavailable is a coordination store that would not give the
	// map up. Files already stored are where they were; this answer cannot
	// say where that is.
	ObjectMapUnavailable ObjectMapState = "unavailable"

	// ObjectMapNone is a fleet with no map yet: no data node has joined, so
	// no file can be stored.
	ObjectMapNone ObjectMapState = "no_map"

	// ObjectMapUnreadable is a map this build cannot place by — one a newer
	// build wrote during an upgrade.
	ObjectMapUnreadable ObjectMapState = "unreadable"

	// ObjectMapPlaced is a map read and rendered: every [PlacedObjects]
	// field is present.
	ObjectMapPlaced ObjectMapState = "placed"
)

// Valid reports whether s is a state this build renders.
func (s ObjectMapState) Valid() bool {
	switch s {
	case ObjectMapUnavailable, ObjectMapNone, ObjectMapUnreadable, ObjectMapPlaced:
		return true
	}
	return false
}

// FleetObjects is the fleet view's object-store block.
//
// FLAT ON THE WIRE, with the placed map's fields present exactly when State is
// [ObjectMapPlaced]: the embedded pointer is inlined when set and absent when
// not, so a count whose zero is a reading — no degraded groups, a member with
// no share — is always written for a placed map and never for any other.
type FleetObjects struct {
	State ObjectMapState `json:"state"`

	*PlacedObjects
}

// PlacedObjects is a placed map as the fleet view renders it.
type PlacedObjects struct {
	// Generation is the map's lineage: a map recreated after its key was
	// lost is a new one, whose epochs start again.
	Generation uuid.UUID `json:"generation"`

	// Epoch counts placement changes.
	Epoch uint64 `json:"epoch"`

	// Replicas is how many copies of each chunk the company asks for, and
	// Copies how many the map places — fewer while it has fewer placeable
	// members than that, which is the shortfall an operator is looking
	// for.
	Replicas int `json:"replicas"`
	Copies   int `json:"copies"`

	// PGs is how many placement groups the slots are divided into.
	PGs int `json:"pgs"`

	// FailureDomain is the node label a group's copies are spread across,
	// absent for none; DistinctDomains how many of its values the
	// placeable members span; DomainLimited whether that is fewer than a
	// group has copies, so some groups hold two copies in one domain.
	FailureDomain   string `json:"failure_domain,omitempty"`
	DistinctDomains int    `json:"distinct_domains"`
	DomainLimited   bool   `json:"domain_limited"`

	// Hold is an operator's hold in force now, absent when there is none
	// or it has expired — an expired hold the maintainer has not cleared
	// yet removes members again, so it is not rendered as holding.
	Hold *MapHold `json:"hold,omitempty"`

	// DegradedGroups is how many groups place a copy on a member that is
	// absent or whose store has failed right now — each is a group a copy
	// short until that member returns or the map moves it.
	DegradedGroups int `json:"degraded_groups"`

	// Balance is how evenly the map spreads the copies over its members'
	// weights, as last measured: absent for a map nothing has measured,
	// never a zero standing in for one.
	Balance *ObjectBalance `json:"balance,omitempty"`

	// Members are the map's members in node order.
	Members []ObjectMember `json:"members"`

	// Removed are the nodes the map removed for absence, still remembers,
	// and has not seen back — the list `crewlet objects in` takes a name
	// from to vouch for one. A removed node that IS seen back is a member
	// again at once, on probation ([MapMember.Probation]), and is
	// listed there rather than here: it is one node, and two lists naming
	// it would each say half of what it is.
	Removed []MapRemoval `json:"removed"`
}

// ObjectBalance is the map's last measurement of its members' copies against
// what their weights entitle them to — the one fact that says whether a weight
// is being kept as a promise or only as an intent.
//
// ON THE SURFACES because it was in the logs alone. A balance that does not
// converge still writes the best map it measured and moves on, so the only
// symptom of a fleet whose shares cannot reach their weights — a failure
// domain too crowded to spread, members too light for their targets to be
// counted in whole copies — was an `object_map_changed` line with
// `balance_converged=false` on whichever node ran it — the map keeper, or
// the node that served an operator's out or in — which nobody reading a
// fleet looks at.
type ObjectBalance struct {
	// Epoch is the map epoch whose placement was measured. Behind the
	// map's own epoch for the one tick after a split — an epoch that
	// deliberately does not balance, measured at the maintainer's next
	// tick — so a reader compares the two before it trusts the rest.
	Epoch uint64 `json:"epoch"`

	// DeviationPercent is the largest difference between any placeable
	// member's copies and its target, as a percentage of that target, and
	// TolerancePercent what a balance aims within
	// ([placement.DefaultTolerance]). Converged is whether the one is
	// within the other.
	DeviationPercent float64 `json:"deviation_percent"`
	TolerancePercent float64 `json:"tolerance_percent"`
	Converged        bool    `json:"converged"`

	// Rounds is how many layouts the balance that set these shares
	// measured, at most [placement.DefaultMaxRounds] — it stops sooner on
	// converging, or where no share could move by a unit — and ZERO when no
	// balance ran: the maintainer measured a split's placement and found it
	// close enough to leave as it was, since a balance moves groups to buy
	// the precision it finds.
	Rounds int `json:"rounds"`
}

// renderBalance is b as the fleet view shows it, or nil when nothing has
// measured the map — a map written before balances were recorded, until the
// maintainer's next change.
func renderBalance(b objstore.Balance) *ObjectBalance {
	if b.Epoch == 0 {
		return nil
	}
	return &ObjectBalance{
		Epoch:            b.Epoch,
		DeviationPercent: 100 * b.Deviation,
		TolerancePercent: 100 * placement.DefaultTolerance,
		Converged:        b.Converged,
		Rounds:           b.Rounds,
	}
}

// ObjectMember is one member as the fleet view renders it: the map's view of
// it, the share of the copies it holds under the stored map, and what its own
// objects lease reports.
type ObjectMember struct {
	MapMember

	// SharePercent is the member's measured share of every group copy the
	// stored map places, 0..100 — what its weight bought once the balancer
	// ran, and 0 for a member taken out or on probation.
	SharePercent float64 `json:"share_percent"`

	// Live is whether the member holds a live objects lease offering a
	// share — the reading the map's maintainer counts presence by.
	Live bool `json:"live"`

	// Health, Repair and Scrub are its lease's reports, and Strays how many
	// referenced copies it holds beyond what the map places on it. Each is
	// absent when the member reported none.
	Health *ObjectHealth `json:"health,omitempty"`
	Repair *ObjectRepair `json:"repair,omitempty"`
	Scrub  *ObjectScrub  `json:"scrub,omitempty"`
	Strays *int          `json:"strays,omitempty"`
}

// ObjectHealth is a member's store health: `ok`, `nearfull`, `full` or
// `failed` — a string, because a newer peer may report a state this build
// does not know, and showing it beats dropping it.
type ObjectHealth struct {
	State       string  `json:"state"`
	Detail      string  `json:"detail,omitempty"`
	UsedPercent float64 `json:"used_percent"`
}

// ObjectRepair is a member's last repair pass: the map epoch it placed by,
// whether it reached every group the member holds, and what it counted —
// Pending being the chunks the map places on the member that it still did not
// hold when the pass ended.
type ObjectRepair struct {
	Epoch       uint64    `json:"epoch"`
	Completed   bool      `json:"completed"`
	Placed      int       `json:"placed"`
	Held        int       `json:"held"`
	Pending     int       `json:"pending"`
	Unreachable int       `json:"unreachable"`
	Missing     int       `json:"missing"`
	At          time.Time `json:"at,omitzero"`
}

// ObjectScrub is where a member's scrub is in its cycle, and what it has found
// in this one: chunks intact, chunks ROTTEN (no longer matching their names)
// and chunks UNREADABLE (bytes the disk would not return). A rotten chunk is
// removed for repair to fetch a good copy, and so is an unreadable one the
// store could remove; the scrub steps past every one of them, so a count that
// keeps rising is a disk failing chunk by chunk rather than a scrub that
// stopped. Error is what stopped it last, absent while it runs.
type ObjectScrub struct {
	CycleStarted time.Time `json:"cycle_started,omitzero"`
	Progress     float64   `json:"progress"`
	Verified     int       `json:"verified"`
	Rotten       int       `json:"rotten"`
	Unreadable   int       `json:"unreadable"`
	Error        string    `json:"error,omitempty"`
}

// RenderObjects is a placed map as the fleet view renders it, joined with the
// live objects leases.
//
// layout is the stored map's own ([objplacement.Map.Layout]) — taken rather than
// computed here, because computing one is groups × members draws and the fleet
// question caches it per map (see [objectLayouts]). leases are the live
// objects leases; one for a node the map does not hold is ignored, as the
// maintainer adds it at its next tick — on probation, if it is one the map
// removed.
func RenderObjects(state objstore.MapState, layout *objplacement.Layout,
	leases []coord.Lease, now time.Time) FleetObjects {

	m := state.Map
	d := m.Draw()
	reports := objectReports(leases)
	copies := layout.Copies()
	total := 0
	for _, n := range copies {
		total += n
	}

	placed := &PlacedObjects{
		Generation: m.Generation, Epoch: m.Epoch,
		Replicas: m.Replicas, Copies: m.Size(), PGs: m.Groups(),
		FailureDomain:   m.FailureDomain,
		DistinctDomains: m.DistinctDomains(), DomainLimited: m.DomainLimited(),
		Hold:    RenderHold(state.State, now),
		Balance: renderBalance(state.Balance),
		Members: make([]ObjectMember, 0, len(m.Members)),
		Removed: RenderRemoved(state.State, d),
	}
	down := map[string]bool{}
	for _, member := range m.Members {
		view, _ := RenderMember(state.State, d, member.Node)
		row := ObjectMember{MapMember: view}
		if total > 0 {
			row.SharePercent = 100 * float64(copies[member.Node]) / float64(total)
		}
		report, live := reports[member.Node]
		row.Live = live
		if live {
			row.Health, row.Repair, row.Scrub, row.Strays = report.health(), report.repair(),
				report.scrub(), report.Strays
		}
		if !live || report.failed() {
			down[member.Node] = true
		}
		placed.Members = append(placed.Members, row)
	}
	placed.DegradedGroups = degradedGroups(layout, down)

	return FleetObjects{State: ObjectMapPlaced, PlacedObjects: placed}
}

// degradedGroups counts the groups whose up set holds a member in down.
//
// A WALK OF THE CACHED LAYOUT, not of the ranking: the up sets are exactly the
// copies the map places, and a member outside a group's up set holds nothing
// of it that anybody is counting on.
func degradedGroups(layout *objplacement.Layout, down map[string]bool) int {
	if len(down) == 0 {
		return 0
	}
	n := 0
	for pg := range layout.Map().Groups() {
		for _, node := range layout.Up(pg) {
			if down[node] {
				n++
				break
			}
		}
	}
	return n
}

// objectReport is one live objects lease, decoded.
type objectReport struct{ objstore.ObjectsMeta }

// objectReports is every live objects lease that offers a share, by node.
//
// THE MAINTAINER'S OWN READING (engine.objectHolders): a lease whose weight
// cannot be read offers nothing and is skipped, so the member it names reads
// as absent here exactly as it counts absent there.
func objectReports(leases []coord.Lease) map[string]objectReport {
	out := make(map[string]objectReport, len(leases))
	for _, lease := range leases {
		node, ok := coord.ObjectsNode(lease.Resource)
		if !ok {
			continue
		}
		meta, ok := objstore.ObjectsFromMeta(lease.Meta)
		if !ok {
			continue
		}
		out[node] = objectReport{meta}
	}
	return out
}

// failed reports whether the member's store says it has failed — the one
// state the maintainer counts as absent. A full store still serves what it
// holds, and a state this build does not know is not one that said it failed.
func (r objectReport) failed() bool {
	return r.Health != nil && disk.HealthState(r.Health.State) == disk.HealthFailed
}

func (r objectReport) health() *ObjectHealth {
	if r.Health == nil {
		return nil
	}
	return &ObjectHealth{State: r.Health.State, Detail: r.Health.Detail,
		UsedPercent: r.Health.UsedPercent}
}

func (r objectReport) repair() *ObjectRepair {
	p := r.Repair
	if p == nil {
		return nil
	}
	return &ObjectRepair{Epoch: p.Epoch, Completed: p.Completed, Placed: p.Placed,
		Held: p.Held, Pending: p.Pending, Unreachable: p.Unreachable,
		Missing: p.Missing, At: p.At}
}

func (r objectReport) scrub() *ObjectScrub {
	s := r.Scrub
	if s == nil {
		return nil
	}
	return &ObjectScrub{CycleStarted: s.CycleStarted, Progress: s.Progress,
		Verified: s.Verified, Rotten: s.Rotten, Unreadable: s.Unreadable, Error: s.Error}
}

// ObjectMapReader is the stored placement map, read as every node reads it.
type ObjectMapReader interface {
	ObjectMap(ctx context.Context) (coord.ObjectMapRecord, bool, error)
}

// objectMap is the fleet's placement map as the fleet view shows it.
//
// FOUR STATES NAMED APART, never folded into an empty member list — a map the
// store would not give up, a fleet with no map yet (where no upload can land),
// one a newer build wrote that this one cannot read, and a map — because each
// sends an operator somewhere different, and "no members" reads as the second
// of them whichever it was.
func (s Sources) objectMap(ctx context.Context, leases []coord.Lease) FleetObjects {
	rec, found, err := s.Objects.ObjectMap(ctx)
	switch {
	case err != nil:
		log.WarnContext(ctx, "fleet_object_map_unavailable", "error", err)
		return FleetObjects{State: ObjectMapUnavailable}
	case !found:
		return FleetObjects{State: ObjectMapNone}
	}
	state, err := objstore.DecodeMapState(rec.Value)
	if err != nil {
		log.WarnContext(ctx, "fleet_object_map_unreadable", "error", err)
		return FleetObjects{State: ObjectMapUnreadable}
	}
	return RenderObjects(state, s.layouts.of(state.Map), leases, s.clock())
}

// objectLayouts is the stored map's layout, computed once per map rather than
// once per request.
//
// A LAYOUT IS GROUPS × MEMBERS DRAWS — measured at 185 ms for two hundred
// members over 8192 groups — and the Nodes screen polls every fifteen seconds
// from every tab that has it open, while the map changes a few times a day. So
// the last map's layout is kept, and a request whose stored map is the same
// one reuses it: an LRU of one, because only the current map is ever asked
// about.
//
// THE SAME ONE is the whole map compared, not its epoch alone: the epoch moves
// with every placement change the maintainer makes, but a layout computed for
// a map it did not describe would render every share on the screen wrong, and
// comparing a few dozen members costs nothing beside a layout.
type objectLayouts struct {
	mu     sync.Mutex
	m      objplacement.Map
	layout *objplacement.Layout
}

// of is m's layout, computed at most once per map. A nil cache computes it
// every time, which is what a caller that never registered the question gets.
//
// THE LOCK IS HELD WHILE A LAYOUT IS COMPUTED, so two requests arriving
// together after a map change compute it once between them rather than twice.
func (c *objectLayouts) of(m objplacement.Map) *objplacement.Layout {
	if c == nil {
		return m.Layout()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.layout == nil || !c.m.Equal(m) {
		c.m = m
		c.m.Members = slices.Clone(m.Members)
		c.layout = m.Layout()
	}
	return c.layout
}
