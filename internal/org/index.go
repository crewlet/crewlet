package org

// Index is the chart's durable lookups — a unit by reference, a seat by
// handle, and a unit's effective lead — built once, for a caller that asks
// them of many references against one chart: the code host's validation and
// its reconcile resolve the owner of every repository a document declares, up
// to ten thousand of them, and its warnings and its grant roster ask the lead
// of every unit those owners name.
//
// # Why it exists
//
// [Organization.UnitByRef] is two passes over every unit, folding each id and
// name, and [Organization.SeatByHandle] a pass over every seat, deriving each
// handle — the right shape for one lookup a request makes, and quadratic for a
// document's worth: ten thousand owners looked up by unit and by seat on a
// chart of 300 units and 900 seats took 4.3–4.5 s and 625 MB through them,
// and 2.4–2.6 ms and 0.61 MB through an Index, its build included
// (BenchmarkResolvingADocumentsOwners, four vCPUs of a 2.8 GHz Xeon under a
// load average near 15) — a cost every data node paid at every apply, inside
// the apply. An Index costs one walk of the chart and then a map lookup a
// reference.
//
// [Organization.EffectiveLead] has the same shape for the same reason: an
// INHERITED lead — a team's lead named on the division above it, the usual
// shape — is found by searching the unit's whole subtree for the name and then
// walking every seat of the chart by name, so asking it of every unit, or of
// every repository's owning unit, costs units × seats. Asked of every unit of
// a chart of 1 100 units and 3 100 seats it took 53–64 ms and 26 MB through
// the chart, and 4.7–7.9 ms and 1.5 MB through an Index built for the purpose
// (BenchmarkAskingEveryUnitsLead, same machine and load) — a map lookup and a
// walk of the seats SHARING the lead's name, which a chart that passed
// admission has one of.
//
// # The same answer, by construction and by test
//
// THE SAME RULE as the methods it stands for, and held to them for every
// reference a chart can be asked (TestTheIndexAnswersWhatTheChartDoes): a unit
// reference folds as [foldUnitKey] folds it, AN ID WINS an ambiguity with
// another unit's name, and within one spelling the first unit in walk order
// answers; a handle is compared exactly, the first seat in walk order
// answering; and a unit's effective lead is the first seat named its lead in
// its OWN subtree's walk order ([Unit.LeadRole]), else the first so named in
// the chart's ([Organization.Role]).
//
// A unit's subtree is ONE RUN of the chart's seat walk ([Organization.AllRoles]
// visits a unit's own seats, then each child's subtree, before anything after
// it), so the Index keeps each unit's run as a span of walk positions and each
// name's seats in walk order: the subtree's first seat of a name is the first
// of that name at or past the span's start, inside it.
//
// BUILD IT FROM A FROZEN CHART, as everything that reads an organization is
// ("Build it, wire it, then treat it as frozen" on [Organization]): it is a
// snapshot of the chart it was built from, and a chart edited after it would
// answer what the Index does not know.
type Index struct {
	unitsByID   map[string]*Unit
	unitsByName map[string]*Unit
	seats       map[string]*Role

	// roles is every seat in the chart's walk order ([Organization.AllRoles]),
	// so a seat's walk position is its index here.
	roles []*Role
	// named is the walk positions of the first and the last seat with each
	// name, and nextNamed, at a seat's position, the position of the next
	// seat with the same name, or -1 — each name's seats in walk order, one
	// slice for the whole chart rather than one per name (the last is what
	// the build links the next one from).
	named     map[string]nameRun
	nextNamed []int
	// spans is each unit's subtree as the half-open run of walk positions
	// its seats hold.
	spans map[*Unit]span

	unitKeys []string
	handles  []string
}

// span is a half-open run [start, end) of walk positions.
type span struct{ start, end int }

// nameRun is the walk positions of the first and the last seat of one name.
type nameRun struct{ first, last int }

// Index builds the chart's [Index]. Nil-safe: a nil organization indexes
// nothing, and every lookup answers nil.
func (o *Organization) Index() *Index {
	x := &Index{
		unitsByID:   map[string]*Unit{},
		unitsByName: map[string]*Unit{},
		seats:       map[string]*Role{},
		named:       map[string]nameRun{},
		spans:       map[*Unit]span{},
	}
	if o == nil {
		return x
	}
	for u := range o.AllUnits() {
		// FIRST IN WALK ORDER WINS within each spelling — the order
		// UnitByRef's passes meet them in.
		if id := foldUnitKey(u.ID); id != "" {
			if _, taken := x.unitsByID[id]; !taken {
				x.unitsByID[id] = u
			}
		}
		if name := foldUnitKey(u.Name); name != "" {
			if _, taken := x.unitsByName[name]; !taken {
				x.unitsByName[name] = u
			}
		}
		x.unitKeys = append(x.unitKeys, u.Key())
	}
	// THE SEATS IN [Organization.AllRoles]'s ORDER — the root seats, then
	// each unit's own seats before each child's subtree — walked here by
	// hand rather than through the iterator, because this walk is the one
	// that sees where each unit's subtree begins and ends.
	add := func(r *Role) {
		at := len(x.roles)
		x.roles = append(x.roles, r)
		x.nextNamed = append(x.nextNamed, -1)
		run, seen := x.named[r.Name]
		if seen {
			x.nextNamed[run.last] = at
		} else {
			run.first = at
		}
		run.last = at
		x.named[r.Name] = run
		handle := r.Handle()
		if _, taken := x.seats[handle]; !taken && handle != "" {
			x.seats[handle] = r
		}
		x.handles = append(x.handles, handle)
	}
	var walk func(u *Unit)
	walk = func(u *Unit) {
		start := len(x.roles)
		for _, r := range u.Roles {
			add(r)
		}
		for _, c := range u.Children {
			walk(c)
		}
		x.spans[u] = span{start: start, end: len(x.roles)}
	}
	for _, r := range o.Roles {
		add(r)
	}
	for _, u := range o.Units {
		walk(u)
	}
	return x
}

// EffectiveLead is [Organization.EffectiveLead] against the indexed chart: the
// seat leading u — the first seat named its lead in u's own subtree, else the
// first so named in the chart, so a lead inherited from an ancestor answers —
// or nil where u names no lead or no seat holds the name. A unit the Index was
// not built over (another chart's, or nil) has no lead here: nil.
//
// A map lookup and a walk of the seats SHARING the lead's name, which a chart
// that passed admission has one of, where the chart's own method searches the
// unit's subtree and then every seat it holds.
func (x *Index) EffectiveLead(u *Unit) *Role {
	if u == nil || u.Lead == "" {
		return nil
	}
	s, held := x.spans[u]
	run, named := x.named[u.Lead]
	if !held || !named {
		return nil
	}
	for at := run.first; at >= 0 && at < s.end; at = x.nextNamed[at] {
		if at >= s.start {
			return x.roles[at]
		}
	}
	return x.roles[run.first]
}

// UnitByRef is [Organization.UnitByRef] against the indexed chart: the unit a
// durable reference names, by its id or its name, or nil.
func (x *Index) UnitByRef(ref string) *Unit {
	folded := foldUnitKey(ref)
	if folded == "" {
		return nil
	}
	if u, ok := x.unitsByID[folded]; ok {
		return u
	}
	return x.unitsByName[folded]
}

// SeatByHandle is [Organization.SeatByHandle] against the indexed chart: the
// seat of either kind with this handle, or nil.
func (x *Index) SeatByHandle(handle string) *Role {
	if handle == "" {
		return nil
	}
	return x.seats[handle]
}

// UnitKeys is every unit's [Unit.Key], in walk order — what a refusal of a
// unit reference names as the units there are. A copy: the caller may keep it.
func (x *Index) UnitKeys() []string { return append([]string(nil), x.unitKeys...) }

// Handles is every seat's handle, in walk order — what a refusal of a handle
// names as the seats there are. A copy: the caller may keep it.
func (x *Index) Handles() []string { return append([]string(nil), x.handles...) }
