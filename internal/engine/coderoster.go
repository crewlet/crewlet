package engine

import (
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/org"
)

// codeRoster is the chart as [gitlet.Roster] reads it ([gitlet.GrantOf]),
// built ONCE an epoch because a listing judges every row it returns, each
// inside an estate read transaction. The build is linear
// (BenchmarkBuildingTheCodeRoster) because each unit's lead is the index's
// ([org.Index.EffectiveLead]), never [org.Organization.EffectiveLead], whose
// search for an inherited lead made it units × seats, paid by the first code
// read after each activation with every read behind it waiting.
//
// A unit reference resolves through [org.Index.UnitByRef], never compared with
// [org.Unit.Key]: a repository's owner outlives its unit's spelling (a unit
// that gains an `id:` keeps its name in every row), so the maps are keyed by
// the unit itself. Frozen after [newCodeRoster], so read unlocked.
type codeRoster struct {
	units *org.Index
	// seats is every seat by handle, the first in the chart's walk order
	// where two share one (the order [org.Index.SeatByHandle] answers in).
	seats map[string]gitlet.RosterSeat
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
		seats:  map[string]gitlet.RosterSeat{},
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
		r.seats[handle] = gitlet.RosterSeat{Handle: handle, Human: role.IsHuman(),
			Tier: gitlet.Tier(role.CodeAccess), Unit: unit.Key()}
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
func (r codeRoster) Seat(handle string) (gitlet.RosterSeat, bool) {
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

// rosterOf is the epoch c's roster, built at its first question
// ([Engine.installEpoch] stamps it): only an agent's code read asks, so an
// apply should not pay a walk of the chart nobody may need.
func rosterOf(c *Company) func() codeRoster {
	return sync.OnceValue(func() codeRoster { return newCodeRoster(c.Org) })
}

// codeRoster is the CURRENT epoch's chart as Gitlet reads a seat's grant,
// read per call so a revision is in force from its activation, or nil with no
// epoch: NO CHART, which says nothing about the seat. Its readers refuse on nil
// as about THIS NODE, so another node is asked: the code reader with
// [gitlet.ErrNotServing] ([gitlet.ReaderOptions.Roster]), the git reads with
// [gitlet.ErrGitUnready] and [gitlet.UnreadyNoChart]. Never that the seat
// stands nowhere (only a denying chart says that), nor that every seat reads;
// [gitlet.GrantOf] still holds an agent to none on a nil roster.
func (e *Engine) codeRoster() gitlet.Roster {
	c := e.Company()
	if c == nil || c.roster == nil {
		return nil
	}
	return c.roster()
}
