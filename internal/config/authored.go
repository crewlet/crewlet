package config

import (
	"slices"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/org"
)

// TURNING AN AUTHORED FILE INTO THE TWO HALVES THE ENGINE RUNS.
//
// A company file is one document: an operator writes their whole company in
// it, and that is the product rather than an implementation detail. The
// ENGINE holds the two halves separately — a settings epoch and a chart the
// state log is the write-ahead for — so something has to divide the file, and
// this is it.
//
// # Why both halves are here rather than at the caller
//
// There are three callers and they must not disagree: the boot seed that
// publishes a file's chart to the log, `crewlet config import`, and the
// equivalence gate that holds the row derivation against the document one.
// The gate is the reason this is production code at all — it was a pair of
// helpers inside the test, so what the gate measured was a converter no
// company ever ran through, and a divergence between it and the real import
// was invisible to the one test written to find divergences.
//
// # The nesting is flattened, and that is the whole of the conversion
//
// A document nests units inside units and seats inside units; rows are flat
// and carry `Parent` instead. Nothing else is derived here — a unit's
// inherited lead, a seat's expanded `manages`, the credentials a unit passes
// down are all DERIVATIONS over the finished tree, and writing one into a row
// would store an answer nothing recomputes.

// AuthoredChart is the chart half of a parsed company: its units and its
// seats, flat, with every reference exactly as it was written.
func AuthoredChart(c *Company) chart.Authored {
	var out chart.Authored
	if c == nil {
		return out
	}
	for i := range c.Roles {
		out.Seats = append(out.Seats, authoredSeat(&c.Roles[i], c.Roles[i].Unit))
	}
	var walk func(units []Unit, parent string)
	walk = func(units []Unit, parent string) {
		for i := range units {
			unit := &units[i]
			key := unit.IdentityKey()
			// THE RUNTIME DOCUMENT FROM THE ORG UNIT, and WITHOUT
			// its subtree: Unit() builds the children and the seats
			// recursively, and this walk places each of them as its
			// own row. Encoding the subtree into the parent's blob
			// would store the whole company inside its root.
			flat := unit.Unit()
			flat.Roles, flat.Children = nil, nil
			runtime, _ := org.UnitRuntime(flat)
			out.Units = append(out.Units, chart.AuthoredUnit{
				Key: key, Parent: parent,
				Name: unit.Name, Type: string(unit.Type),
				Purpose: unit.Purpose, Goals: slices.Clone(unit.Goals),
				Lead: unit.Lead, Channel: unit.Channel,
				Project: unit.Project, Space: unit.Space,
				KnowledgeRefs: slices.Clone(unit.Knowledge),
				Runtime:       runtime,
			})
			for j := range unit.Roles {
				out.Seats = append(out.Seats, authoredSeat(&unit.Roles[j], key))
			}
			walk(unit.Children, key)
		}
	}
	walk(c.Units, "")
	return out
}

// authoredSeat is one seat as written, under the unit that contains it.
//
// THE RUNTIME DOCUMENT IS BUILT FROM [org.Role], not from the config type:
// the seat the engine runs is what `Seat()` produces (the per-phase model
// chain resolved from the two spellings, the placement, the cloned
// credentials), and re-deriving any of that here would be a second
// implementation of rules that have one.
//
// AN ENCODING FAILURE IS SWALLOWED, and the field is left empty. It is
// unreachable — a Role is strings, slices and named string types — and the
// alternative is an error return through a pure converter that three callers
// would each have to decide what to do with. An empty runtime is a seat with
// no model and no credentials, which [config.Company.Validate] has already
// refused to be a document and which the view reports as a seat that cannot
// think.
func authoredSeat(role *Role, unit string) chart.AuthoredSeat {
	seat := role.Seat()
	runtime, _ := org.SeatRuntime(seat)
	return chart.AuthoredSeat{
		Handle: seat.Handle(), Unit: unit,
		Kind: chart.SeatKind(seat.EffectiveKind()), Name: role.Name,
		Email: role.Email, Backstory: role.Backstory, Goal: role.Goal,
		Responsibilities:     slices.Clone(role.Responsibilities),
		BehavioralGuidelines: slices.Clone(role.BehavioralGuidelines),
		Manages:              slices.Clone(role.Manages),
		Project:              role.Project, Space: role.Space,
		Runtime: runtime,
	}
}

// OrgSettings is the settings the company VIEW needs: the few company-wide
// values a derived tree carries.
//
// NOT [Settings], which is what a revision STORES — that one holds the
// providers, the integrations and the turn engine, none of which a tree has
// any use for. This is the smaller thing [org.FromRows] takes, and it is
// separate for the reason the org model's own doc gives for not importing
// config: a roster is rendered by a package that must not have to know what
// an MCP server is.
func OrgSettings(c *Company) org.Settings {
	if c == nil {
		return org.Settings{}
	}
	return org.Settings{
		Name: c.Name, Mission: c.Mission, Vision: c.Vision,
		Policies: slices.Clone(c.Policies), TokenBudget: c.TokenBudget,
		KnowledgeScope: slices.Clone(c.Knowledge.KnowledgeScope),
	}
}
