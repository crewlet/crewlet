package membership

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/placement"
)

// Changes is what happened to who is in a map between two of its records —
// the one a maintainer read and the one it wrote — in the order a log reports
// it: the members that arrived or were trusted, those that left, the runs of
// absence that opened and those that closed.
//
// ONE ACCOUNT FOR EVERY MAP, so the object map and the estate map name the
// same transitions for the same reasons, and each only chooses the words its
// own log uses.
type Changes struct {
	// Admitted is every member of the later record that joined it, joined
	// it on probation, or was trusted again, in node order.
	Admitted []Admission

	// Removed is every member of the earlier record the later one no
	// longer holds, in node order.
	Removed []Departure

	// Absent is every member whose run of absence opened, in node order,
	// with the run as it opened.
	Absent []Opened

	// Returned is every member still held whose run of absence closed, in
	// node order: back for [StableTicks].
	Returned []string
}

// Admit is how a member was admitted.
type Admit string

const (
	// Joined is a node that became a member, placed on.
	Joined Admit = "joined"

	// OnProbation is a node removed for absence and seen back: a member
	// again, placed on nothing until it is trusted.
	OnProbation Admit = "probation"

	// Trusted is a member whose probation ended: placed on again.
	Trusted Admit = "trusted"
)

// Valid reports whether a is a way of admitting this build knows.
func (a Admit) Valid() bool { return a == Joined || a == OnProbation || a == Trusted }

// Admission is one member admitted.
type Admission struct {
	// Member is the member as the later record holds it.
	Member placement.Member

	// How is how it was admitted.
	How Admit

	// PlacedAfterTicks is, for a member [OnProbation], how many more
	// present ticks its probation needs.
	PlacedAfterTicks int
}

// PlacedOn reports whether the admitted member is placed on now: a member
// trusted after its probation is not while it is out — taken out, or barred
// ([State.Barred]), neither of which a probation ending lifts — and a log that
// said "the map places on it again" of such a member told an operator the one
// thing about it that was false.
func (a Admission) PlacedOn() bool { return a.Member.Placeable() }

// Departure is one member removed.
type Departure struct {
	// Node is the member removed.
	Node string

	// Removal is what the later record remembers of it: why it went.
	Removal Removal

	// OnProbation is whether it was on probation when it left — which it
	// leaves the tick it is not seen, with no grace of its own.
	OnProbation bool

	// AbsentTicks is, for a member not on probation, how many ticks its run
	// of absence had counted, the one that removed it included.
	AbsentTicks int
}

// Opened is one run of absence opened.
type Opened struct {
	Node    string
	Absence Absence
}

// Diff is what changed between two records of one map: the state and the
// members of each.
func Diff(before State, beforeMembers []placement.Member, after State, afterMembers []placement.Member) Changes {
	earlier := placement.Draw{Members: beforeMembers}
	later := placement.Draw{Members: afterMembers}
	var c Changes
	for _, m := range afterMembers {
		prior, member := earlier.Member(m.Node)
		switch {
		case !member && m.Probation:
			c.Admitted = append(c.Admitted, Admission{Member: m, How: OnProbation,
				PlacedAfterTicks: StableTicks - after.Removed[m.Node].Present})
		case !member:
			c.Admitted = append(c.Admitted, Admission{Member: m, How: Joined})
		case prior.Probation && !m.Probation:
			c.Admitted = append(c.Admitted, Admission{Member: m, How: Trusted})
		}
	}
	for _, m := range beforeMembers {
		if later.Holds(m.Node) {
			continue
		}
		d := Departure{Node: m.Node, Removal: after.Removed[m.Node], OnProbation: m.Probation}
		if !m.Probation {
			d.AbsentTicks = before.Absence[m.Node].Ticks + 1
		}
		c.Removed = append(c.Removed, d)
	}
	for _, node := range slices.Sorted(maps.Keys(after.Absence)) {
		if _, was := before.Absence[node]; !was {
			c.Absent = append(c.Absent, Opened{Node: node, Absence: after.Absence[node]})
		}
	}
	for _, node := range slices.Sorted(maps.Keys(before.Absence)) {
		if _, still := after.Absence[node]; !still && later.Holds(node) {
			c.Returned = append(c.Returned, node)
		}
	}
	return c
}
