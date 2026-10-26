package engine

import (
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/org"
)

// codeRoster is the chart as [code.Roster] reads it — every grant a seat holds
// on a repository is judged by it ([code.GrantOf]) — built ONCE an epoch from
// its organization: O(units + seats) once, then O(1) a seat and O(depth) a
// lead chain or a subtree test. Once, because a listing judges every row it
// returns: a page of 500 repositories asked of a walk of the chart a row is
// 500 walks, every one of them inside a read transaction of the estate.
//
// LINEAR BECAUSE EVERY UNIT'S LEAD IS THE INDEX'S ([org.Index.EffectiveLead]),
// never [org.Organization.EffectiveLead]: a team's lead is usually inherited
// from its division, and the chart's own method finds an inherited lead by
// searching the team's subtree and then walking every seat of the chart, which
// asked of every unit made the build units × seats. The build is paid by the
// first agent code read after each activation, on that read's own goroutine,
// with every agent read arriving meanwhile waiting behind it — measured
// (BenchmarkBuildingTheCodeRoster, divisions each led by a seat of their own
// over ten teams of three seats that inherit the lead; four vCPUs of a 2.8 GHz
// Xeon under a load average near 15): 2.9–3.9 ms and 0.97 MB a build for 330
// units and 930 seats, 9.4–11 ms and 2.6 MB for 1 100 and 3 100 — against
// 7.3–9.8 ms and 3.3 MB, and 60–73 ms and 29 MB, through the chart's own
// lookup.
//
// A UNIT REFERENCE IS A DURABLE VALUE, resolved through the chart's own
// durable lookup ([org.Index.UnitByRef]: its key, exactly and then folded,
// before any name, and a name only where one unit alone carries it) and
// never compared with [org.Unit.Key]. A repository's owner is written when it
// is created or declared and outlives the spelling of its unit: a unit that
// gains an `id:` after a repository was made under its name keeps that
// name in every row, and an exact read of a map keyed on Key would lose the
// unit's lead its maintain and its members their write on their own
// repository. So the parent and lead maps are keyed by the unit itself.
//
// Frozen like the organization it is built from: nothing writes it after
// [newCodeRoster], so every goroutine reading the epoch reads it unlocked.
type codeRoster struct {
	units *org.Index
	// seats is every seat by handle, the first in the chart's walk order
	// where two share one (the order [org.Index.SeatByHandle] answers in).
	seats map[string]code.RosterSeat
	// parent is each unit's parent; a top-level unit has none.
	parent map[*org.Unit]*org.Unit
	// lead is each unit's EFFECTIVE lead's handle ([org.Organization.EffectiveLead]:
	// its own, else the nearest ancestor's), "" for none — or for a lead
	// naming no seat, which no seat's handle can equal either.
	lead map[*org.Unit]string
}

// newCodeRoster builds the roster of the chart o. A nil chart is a roster that
// holds no seat and no unit, so every seat it is asked about stands nowhere.
func newCodeRoster(o *org.Organization) codeRoster {
	r := codeRoster{
		units:  o.Index(),
		seats:  map[string]code.RosterSeat{},
		parent: map[*org.Unit]*org.Unit{},
		lead:   map[*org.Unit]string{},
	}
	if o == nil {
		return r
	}
	add := func(role *org.Role, unit *org.Unit) {
		handle := role.Handle()
		if _, taken := r.seats[handle]; taken || handle == "" {
			return
		}
		r.seats[handle] = code.RosterSeat{Handle: handle, Human: role.IsHuman(),
			Tier: code.Tier(role.CodeAccess), Unit: unit.Key()}
	}
	// ROOT SEATS FIRST, then each unit's own seats parents before children:
	// the order [org.Organization.AllRoles] walks, so a handle two seats
	// share answers the seat the chart's own lookup does.
	for _, role := range o.Roles {
		add(role, nil)
	}
	var walk func(u, above *org.Unit)
	walk = func(u, above *org.Unit) {
		if above != nil {
			r.parent[u] = above
		}
		if lead := r.units.EffectiveLead(u); lead != nil {
			r.lead[u] = lead.Handle()
		}
		for _, role := range u.Roles {
			add(role, u)
		}
		for _, child := range u.Children {
			walk(child, u)
		}
	}
	for _, u := range o.Units {
		walk(u, nil)
	}
	return r
}

// Seat is the seat with this handle.
func (r codeRoster) Seat(handle string) (code.RosterSeat, bool) {
	s, ok := r.seats[handle]
	return s, ok
}

// Lead is the effective lead's handle of the unit ref names, or "".
func (r codeRoster) Lead(ref string) string { return r.lead[r.units.UnitByRef(ref)] }

// Leads is the lead chain over the unit ref names, nearest first: the
// effective lead of it and of each unit above it, each handle once — an
// inherited lead is the same handle on every unit it reaches. A scan, not a
// set: the chain is the chart's depth long.
func (r codeRoster) Leads(ref string) []string {
	var out []string
	for u := r.units.UnitByRef(ref); u != nil; u = r.parent[u] {
		if lead := r.lead[u]; lead != "" && !slices.Contains(out, lead) {
			out = append(out, lead)
		}
	}
	return out
}

// Within reports whether the unit ref names is the unit ancestor names or one
// below it — both resolved, so either may be spelled by id or by name.
func (r codeRoster) Within(ref, ancestor string) bool {
	top := r.units.UnitByRef(ancestor)
	if top == nil {
		return false
	}
	for u := r.units.UnitByRef(ref); u != nil; u = r.parent[u] {
		if u == top {
			return true
		}
	}
	return false
}

// rosterOf is the epoch c's roster, built at its first question: a
// [sync.OnceValue] stamped on the epoch as it is installed
// ([Engine.installEpoch]), because most epochs a node installs are never asked
// — only an agent's code read asks — and an apply should not pay a walk of the
// chart for a question nobody puts.
func rosterOf(c *Company) func() codeRoster {
	return sync.OnceValue(func() codeRoster { return newCodeRoster(c.Org) })
}

// codeRoster is the CURRENT epoch's chart as the code host reads a seat's
// grant — read per call, so a revision that moves a seat or a lead is in force
// from its activation on this node, with no restart — or nil where no epoch is
// installed: NO CHART, which says nothing about the seat. Its two readers
// refuse a seat's read on nil as a refusal about THIS NODE — the code reader
// [code.ErrNotServing] ([code.ReaderOptions.Roster]), so the estate's walk
// asks a data node that holds a chart, and the git reads [code.ErrGitUnready]
// with [code.UnreadyNoChart], so another git node is asked — and never answer
// that the seat stands nowhere, which only a chart that denies it may answer,
// nor that every seat reads. [code.GrantOf] itself still holds an agent to
// none on a nil roster, for a judge that asks it with no chart.
func (e *Engine) codeRoster() code.Roster {
	c := e.Company()
	if c == nil || c.roster == nil {
		return nil
	}
	return c.roster()
}
