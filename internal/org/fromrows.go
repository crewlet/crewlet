package org

import (
	"cmp"
	"slices"

	"github.com/crewlet/crewlet/internal/chart"
)

// BUILDING THE COMPANY VIEW FROM CHART ROWS, and why it is a function rather
// than a mutation.
//
// [Organization.Normalize] edits a tree in place: it moves root seats into
// units, cascades leads and channels downward, layers credentials, and
// rewrites every `manages` list. That shape came from a DOCUMENT — parse the
// YAML, fix it up, publish the pointer — and it carries two properties that
// stop being true once the chart is a log:
//
//   - IT IS NOT IDEMPOTENT BY CONSTRUCTION. It is idempotent because each
//     step records what was DECLARED before it overwrites it
//     ([Unit.DeclaredLead]), which is a discipline every future step has to
//     remember. A function over rows has nothing to remember: the rows are
//     the declaration, and the view is a value.
//   - IT CANNOT SAY WHAT IT WAS TRUE OF. A company derived from a document
//     is true of that document; a company derived from a log is true as of a
//     POSITION, and a surface that renders one needs to say which.
//
// So this is a pure function: rows in, an immutable view out, carrying the
// position the rows were read at. Two nodes at one position produce
// byte-identical views, which is the property the whole replicated estate
// rests on and which a mutation could only be tested for.
//
// # It produces the SAME tree Normalize does
//
// Deliberately, and that is the gate this PR turns on: `FromRows` over an
// imported chart must equal `Normalize` over the same document, in every
// derivation, for every example and fixture this repository ships. Until that
// holds the split is not safe to build on — which is why the equivalence test
// exists before any consumer moves.
//
// The order below is Normalize's own, step for step, and the comments say
// where this one has to do something extra because rows are not a tree.

// View is the company as every turn reads it: the authored rows, with every
// derivation applied, at the position they were read at.
//
// IMMUTABLE BY CONVENTION AND BY SHAPE. A live company is served by many
// goroutines at once, so a view that could be edited in place is a data race
// with no owner. A hot reload builds a NEW view and swaps the pointer; it
// never edits the one turns are running against — which is the same rule
// [Organization]'s own doc states, made cheaper to keep by there being no
// mutating method here at all.
type View struct {
	// Org is the derived tree, in the shape forty-two consumers already
	// read.
	//
	// THE SAME TYPE, deliberately. The chart's split is about where the
	// tree COMES FROM, not about what it looks like to a turn — and a view
	// that handed every one of those consumers a new type would be a
	// rewrite of the whole engine rather than a change to one domain.
	Org *Organization

	// At is the chart position these rows were read at, which every answer
	// derived from this view is true as of.
	At chartPosition

	// Reparented names every unit this build had to move to the root to
	// break a cycle, in the order it moved them.
	//
	// REPORTED RATHER THAN SILENT. A cycle cannot reach the rows through
	// the write path — the batch validator refuses one — so a view that
	// finds one is reading rows written by a build that did not have that
	// rule, or rows an operator repaired by hand. Either way the company
	// still has to run, and the operator has to be told which team moved.
	Reparented []string
}

// chartPosition is the position a view was built from.
//
// ITS OWN NAME rather than [statelog.Position] directly, so this package does
// not take a dependency on the framework for one struct: `org` is read by the
// prompt builder, the API and the learning loop, and none of them should have
// to know what a state log is to render a roster.
type chartPosition struct {
	Generation uint32
	Seq        uint64
}

// FromRows builds the company view from one chart read.
//
// THE ORDER IS LOAD-BEARING and is [Organization.Normalize]'s own:
//
//  1. The unit closure and the chains, which is where a cycle is broken.
//  2. Root-seat attachment, so a seat's `unit:` reference puts it in its unit
//     before anything reads unit membership.
//  3. The lead and channel cascade, which reads the chains from step 1.
//  4. Credential inheritance, one level, agents only.
//  5. Auto-management by lead, then manages expansion — both over ONE index,
//     built between them, because neither moves a seat and the membership one
//     captures is the membership both see.
func FromRows(rows chart.Chart, settings Settings) *View {
	view := &View{
		Org: &Organization{
			Name:           settings.Name,
			Mission:        settings.Mission,
			Vision:         settings.Vision,
			Policies:       settings.Policies,
			TokenBudget:    settings.TokenBudget,
			KnowledgeScope: settings.KnowledgeScope,
		},
		At: chartPosition{
			Generation: rows.Position.Generation,
			Seq:        rows.Position.Seq,
		},
	}

	units, reparented := buildUnits(rows)
	view.Reparented = reparented
	view.Org.Units = units
	view.Org.Roles = attachSeats(rows, units)

	// FROM HERE ON IT IS NORMALIZE, unchanged. The steps below read the
	// tree the two above built and are the same code paths a document
	// takes — which is not an accident but the point: two derivations of
	// one rule is how the view and the document stop agreeing.
	for _, u := range view.Org.Units {
		propagateDownward(u, "", "")
	}
	view.Org.inheritMCPEnv()
	index := view.Org.managesIndex()
	view.Org.autoManageByLead(index)
	view.Org.expandManages(index)
	for r := range view.Org.AllRoles() {
		r.Contact.Normalize()
	}
	return view
}

// Settings are the company values that are NOT the chart.
//
// SIX VALUES THAT LEFT THE ORGANIZATION, because none of them is a fact about
// the hierarchy: a mission is prose, a token budget is a meter's ceiling, and
// a knowledge scope is a read rule. They live in the settings document, which
// is still one sealed revision, and reach the view as an argument — so a
// change to the company's mission is not a record on the chart's log and does
// not wake anybody.
type Settings struct {
	Name     string
	Mission  string
	Vision   string
	Policies []string

	TokenBudget    int
	KnowledgeScope []string
}

// buildUnits turns the unit rows into the tree, breaking any cycle.
//
// # Why a cycle has to be handled at all
//
// The write path refuses one: [chart.Batch] replays every move over a working
// copy and walks up from the new parent, so a batch that would close a cycle
// is refused naming both units. But a view is built from ROWS, and rows can
// predate that rule or be repaired by hand — and a build that recursed into
// one would not produce a wrong answer, it would not terminate.
//
// SO A CYCLIC UNIT IS RE-PARENTED TO THE ROOT, ORDERED BY ID. Deterministic
// because every node has to reach the same view from the same rows: picking
// "the one we reached first" would make the tree a function of the row order
// the planner happened to return, and two nodes would then hold different
// companies while both reporting they were caught up.
func buildUnits(rows chart.Chart) ([]*Unit, []string) {
	byKey := make(map[string]*Unit, len(rows.Units))
	// SORTED BY KEY FIRST, so every later walk is deterministic whatever
	// order the read returned. The reader already orders, and this does not
	// trust it: a view is the one value in this engine that two nodes
	// compare byte for byte.
	ordered := slices.Clone(rows.Units)
	slices.SortFunc(ordered, func(a, b chart.Unit) int {
		return cmp.Compare(a.Key, b.Key)
	})
	for _, row := range ordered {
		byKey[row.Key] = unitFrom(row)
	}

	var reparented []string
	var roots []*Unit
	for _, row := range ordered {
		unit := byKey[row.Key]
		parent, hasParent := byKey[row.ParentKey]
		switch {
		case row.ParentKey == "" || !hasParent:
			// THE ORG ROOT, and a parent that is not there is the org
			// root too: a reference naming no unit leaves the object
			// where it is, which is the same rule
			// [Organization.DanglingRefs] reports rather than refuses.
			roots = append(roots, unit)
		case cyclic(byKey, row.Key, row.ParentKey):
			reparented = append(reparented, row.Key)
			roots = append(roots, unit)
		default:
			parent.Children = append(parent.Children, unit)
		}
	}
	return roots, reparented
}

// cyclic reports whether placing key under parent would put it under itself.
//
// A WALK UP FROM THE PARENT, bounded by a visited set: the rows may already
// hold a cycle that does not involve this unit at all, and a walk that trusted
// them to be acyclic would loop for ever on exactly the input this function
// exists for.
func cyclic(byKey map[string]*Unit, key, parent string) bool {
	seen := map[string]bool{}
	for at := parent; at != ""; {
		if at == key || seen[at] {
			return true
		}
		seen[at] = true
		unit, ok := byKey[at]
		if !ok {
			return false
		}
		at = unit.parentKey
	}
	return false
}

// attachSeats puts each seat in its unit, leaving the rest at the root.
//
// THE ROWS ALREADY STATE IT. In a document a root seat's `unit:` reference is
// what moves it, and [Organization.attachRootSeats] performs that move; here
// the placement IS the row, so the "move" is a read. A seat naming a unit that
// is not there stays at the root, which is the same answer the document path
// gives for a reference that resolves to nothing.
func attachSeats(rows chart.Chart, units []*Unit) []*Role {
	byKey := map[string]*Unit{}
	var index func([]*Unit)
	index = func(list []*Unit) {
		for _, u := range list {
			byKey[u.Key()] = u
			index(u.Children)
		}
	}
	index(units)

	ordered := slices.Clone(rows.Seats)
	slices.SortFunc(ordered, func(a, b chart.Seat) int {
		return cmp.Compare(a.Handle, b.Handle)
	})

	var root []*Role
	for _, row := range ordered {
		seat := seatFrom(row, rows.Manages[row.Handle])
		unit, placed := byKey[row.UnitKey]
		if row.UnitKey == "" || !placed {
			root = append(root, seat)
			continue
		}
		unit.Roles = append(unit.Roles, seat)
	}
	return root
}

// unitFrom is one row as the runtime model's unit.
//
// THE LEAD COMES FROM THE ROW'S OWN COLUMN, which is the AUTHORED lead — the
// effective one is what the cascade computes, and a row carrying the computed
// answer would be a derived value written down.
func unitFrom(row chart.Unit) *Unit {
	return &Unit{
		Name:          row.Name,
		ID:            row.Key,
		Type:          UnitType(row.Type),
		Purpose:       row.Purpose,
		Goals:         row.Goals,
		Lead:          row.Lead,
		Channel:       row.Channel,
		Project:       row.Project,
		Space:         row.Space,
		KnowledgeRefs: row.KnowledgeRefs,
		parentKey:     row.ParentKey,
	}
}

// seatFrom is one row as the runtime model's seat.
func seatFrom(row chart.Seat, manages []string) *Role {
	return &Role{
		Name:                 row.Name,
		Kind:                 RoleKind(row.Kind),
		DeclaredHandle:       row.Handle,
		Email:                row.Email,
		Backstory:            row.Backstory,
		Goal:                 row.Goal,
		Responsibilities:     row.Responsibilities,
		BehavioralGuidelines: row.BehavioralGuidelines,
		Manages:              slices.Clone(manages),
		Project:              row.Project,
		Space:                row.Space,
	}
}
