package queries

import (
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/placement"
)

// WHO IS IN A PLACEMENT MAP, AS EVERY SURFACE RENDERS IT — for the object
// store's map and the estate map alike.
//
// ONE RENDERING FOR BOTH MAPS, because what it renders is one lifecycle
// (internal/membership, ADR-0008): a member's absence counted in the
// maintainer's ticks, its probation after a removal, an operator's out and
// hold, and the nodes a map removed and still remembers. Both maps' records
// embed [membership.State] beside their own placement, so a rendering written
// per map would be two readings of one record — and the first of them to learn
// a new field of the lifecycle would be the only one showing it.
//
// Every type here is what the MAP alone says. What a member's own lease says
// about it — its store, its repair, what it holds — is each map's own, beside
// these.

// MapHold is an operator's hold on a map: no member is removed however long it
// is gone, until Until.
type MapHold struct {
	Until  time.Time `json:"until"`
	By     string    `json:"by"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// MapMember is one member as the MAP alone describes it — what a gesture
// answer can say without reading the leases.
type MapMember struct {
	Node   string `json:"node"`
	Weight int    `json:"weight"`

	// Domain is the member's value of the map's failure-domain label,
	// absent when the map names none or the node does not carry it.
	Domain string `json:"domain,omitempty"`

	// Out is a member taken out: placed on nothing, while it still serves
	// what it holds. OutBy, OutReason and OutAt are the gesture that took
	// it out.
	Out       bool      `json:"out"`
	OutBy     string    `json:"out_by,omitempty"`
	OutReason string    `json:"out_reason,omitempty"`
	OutAt     time.Time `json:"out_at,omitzero"`

	// Barred is a member barred from the map — an eviction — which is out
	// until it is readmitted, whatever becomes of its membership: a removal
	// does not lift it, as it lifts an out ([membership.State.Barred]), and
	// nor does putting it back ([membership.ErrBarredMember]).
	// Absent while it is not. OutBy, OutReason and OutAt are the bar's where
	// no out of an operator's own was recorded beside it.
	Barred bool `json:"barred,omitempty"`

	// Probation is a node the map removed for absence and has seen back,
	// absent while the member is not on it: read from and repaired from at
	// once, placed on nothing until it has been present and healthy for
	// long enough to be trusted. A member can be out AND on probation — an
	// operator's decision and the maintainer's, each with its own end.
	Probation *MapProbation `json:"probation,omitempty"`

	// Absence is the member's open run of absence, absent while it has
	// none.
	Absence *MapAbsence `json:"absence,omitempty"`
}

// MapProbation is a member's probation, counted in the maintainer's ticks as an
// absence is.
type MapProbation struct {
	// Present is how many consecutive ticks have seen it present and
	// healthy since it came back, and PlacedAfterTicks how many place on
	// it again. A tick that does not see it ends the probation at once —
	// leaving is exactly what it was being watched for — and it is removed
	// again, however far it had got.
	Present          int `json:"present"`
	PlacedAfterTicks int `json:"placed_after_ticks"`

	// RemovedAt is when the map removed it, for display; Reason and Detail
	// the absence that did.
	RemovedAt time.Time                `json:"removed_at"`
	Reason    membership.AbsenceReason `json:"reason"`
	Detail    string                   `json:"detail,omitempty"`
}

// MapAbsence is a member's run of absence, counted in the maintainer's ticks —
// never timed, so a clock that moved cannot stretch or skip it.
type MapAbsence struct {
	// Ticks is how many ticks have counted the member absent or unhealthy
	// in this run, and OutAfterTicks how many remove it — while no hold is
	// in force.
	Ticks         int `json:"ticks"`
	OutAfterTicks int `json:"out_after_ticks"`

	// Present is how many consecutive ticks have seen it back, and
	// ClearAfterTicks how many clear the run. A member back for fewer keeps
	// every tick it has counted, which is what eventually removes one that
	// flaps.
	Present         int `json:"present"`
	ClearAfterTicks int `json:"clear_after_ticks"`

	// Since is when the run began, as the duty holder of the time read its
	// own clock — for display, never compared.
	Since time.Time `json:"since"`

	// Reason is `absent` or `unhealthy`, and Detail what the member said.
	Reason membership.AbsenceReason `json:"reason"`
	Detail string                   `json:"detail,omitempty"`
}

// MapRemoval is a node a map removed for absence, remembers, and has not seen
// back.
type MapRemoval struct {
	Node string `json:"node"`

	// At is when it was removed, for display; Reason and Detail the
	// absence that removed it.
	At     time.Time                `json:"at"`
	Reason membership.AbsenceReason `json:"reason"`
	Detail string                   `json:"detail,omitempty"`

	// Gone is how many consecutive ticks have not seen it present and
	// healthy, and ForgetAfterTicks how many forget it — after which it
	// joins as any new node would. Seen back before that, it is a member
	// again at once, on probation, and placed on after PlacedAfterTicks
	// ticks in a row.
	Gone             int `json:"gone"`
	ForgetAfterTicks int `json:"forget_after_ticks"`
	PlacedAfterTicks int `json:"placed_after_ticks"`
}

// RenderMember is one member of a map as the map describes it — its draw's
// member and the lifecycle state its record embeds — and false when the map
// has no such member.
func RenderMember(s membership.State, d placement.Draw, node string) (MapMember, bool) {
	m, ok := d.Member(node)
	if !ok {
		return MapMember{}, false
	}
	out := MapMember{Node: m.Node, Weight: m.Weight, Domain: m.Domain, Out: m.Out}
	if m.Probation {
		// THE FLAG IS WHAT NODES PLACE BY, and the removal the count that
		// ends it: the maintainer derives the one from the other on every
		// tick, and a write that set one without the other is refused, so
		// a member on probation with no removal remembered is a record no
		// build wrote — rendered on probation with nothing counted, since
		// the flag is what it is placed by.
		r := s.Removed[node]
		out.Probation = &MapProbation{
			Present: r.Present, PlacedAfterTicks: membership.StableTicks,
			RemovedAt: r.At, Reason: r.Reason, Detail: r.Detail,
		}
	}
	bar, barred := s.Barred[node]
	out.Barred = barred
	if m.Out {
		// THE GESTURE BESIDE THE FLAG. The flag is what nodes place by
		// and the gesture the operator's intent; the maintainer derives
		// one from the other, so a member out with no gesture recorded is
		// one whose record a tick has not reconciled yet.
		if g, taken := s.TakenOut[node]; taken {
			out.OutBy, out.OutReason, out.OutAt = g.By, g.Reason, g.At
		} else if barred {
			out.OutBy, out.OutReason, out.OutAt = bar.By, bar.Reason, bar.At
		}
	}
	if run, gone := s.Absence[node]; gone {
		out.Absence = &MapAbsence{
			Ticks: run.Ticks, OutAfterTicks: membership.OutTicks,
			Present: run.Present, ClearAfterTicks: membership.StableTicks,
			Since: run.Since, Reason: run.Reason, Detail: run.Detail,
		}
	}
	return out, true
}

// RenderHold is the hold in force at now, or nil — an expired hold the
// maintainer has not cleared yet removes members again, so it is not rendered
// as holding.
func RenderHold(s membership.State, now time.Time) *MapHold {
	if !s.Hold.Active(now) {
		return nil
	}
	h := s.Hold
	return &MapHold{Until: h.Until, By: h.By, Reason: h.Reason, At: h.At}
}

// MapBar is a node barred from a map — an eviction — that the map does not hold:
// removed for its absence, forgotten, or never seen. Should it come back it
// joins as a member out, placed on nothing, until it is readmitted — which is
// the only gesture that lifts a bar ([membership.Readmit]).
type MapBar struct {
	Node   string    `json:"node"`
	By     string    `json:"by"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// RenderBarred is every node barred from a map that the map does not hold, in
// node order. A barred node the map DOES hold is rendered on its member row
// instead ([MapMember.Barred]), for [RenderRemoved]'s reason.
func RenderBarred(s membership.State, d placement.Draw) []MapBar {
	out := make([]MapBar, 0, len(s.Barred))
	for node, g := range s.Barred {
		if d.Holds(node) {
			continue
		}
		out = append(out, MapBar{Node: node, By: g.By, Reason: g.Reason, At: g.At})
	}
	slices.SortFunc(out, func(a, b MapBar) int { return strings.Compare(a.Node, b.Node) })
	return out
}

// RenderRemoved is every node a map removed for absence, remembers, and has not
// seen back, in node order. A removed node that IS back is a member on
// probation and is rendered on its member row instead ([MapMember.Probation]):
// it is one node, and two lists naming it would each say half of what it is.
//
// A REMOVED NODE THE MAP BARS IS LISTED ONLY AS BARRED ([RenderBarred]), for
// the same reason and a worse one: what a removal says of a node — it rejoins
// on probation the next time it is seen, and putting it back vouches for it now
// — is false of a barred one, which joins placed on nothing and which an in
// refuses ([membership.ErrBarredMember]). Listed under both, every surface
// offered a removed node's put-back for a node only its readmission puts back.
func RenderRemoved(s membership.State, d placement.Draw) []MapRemoval {
	out := make([]MapRemoval, 0, len(s.Removed))
	for node, r := range s.Removed {
		if _, barred := s.Barred[node]; barred || d.Holds(node) {
			continue
		}
		out = append(out, MapRemoval{
			Node: node, At: r.At, Reason: r.Reason, Detail: r.Detail,
			Gone: r.Gone, ForgetAfterTicks: membership.OutTicks,
			PlacedAfterTicks: membership.StableTicks,
		})
	}
	slices.SortFunc(out, func(a, b MapRemoval) int { return strings.Compare(a.Node, b.Node) })
	return out
}
