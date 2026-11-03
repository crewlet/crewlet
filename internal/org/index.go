package org

import (
	"slices"
	"strings"
)

// Index is the chart's durable lookups — a unit by reference, a seat by
// handle, a unit's effective lead, and each seat's place in the hierarchy (its
// manager and managers, its reports, its unit and its unit chain) — built
// once, for a caller that asks them of many references against one chart: the
// code host's validation and its reconcile resolve the
// owner of every repository a document declares, up to ten thousand of them,
// and its warnings and its grant roster ask the lead of every unit those
// owners name; every apply asks the lead of every led unit (the leads'
// automatic reports in [Organization.Normalize]) and of every unit with a
// schedule run as its lead (a human lead's, in [Organization.Validate]), and
// the home unit of every seat with a project of its own; the derived
// hierarchy a configuration write answers with asks every question of every
// seat and every unit; and a spend chart's per-unit bands and the schedules
// listing ask every seat's unit and every lead-targeted schedule's lead.
//
// # Why it exists
//
// [Organization.UnitByRef] is up to five passes over every unit — its key
// exactly, then folded ([NormalizeKey]), a unit with an id before one keyed on
// its name each time, then every name — and [Organization.SeatByHandle] a pass
// over every seat, deriving each handle: the right shape for one lookup a
// request makes, and quadratic for a document's worth. Ten thousand owners
// looked up by unit and by seat on a chart of 300 units and 900 seats took
// 4.3–4.5 s and 625 MB through them (measured while the chart's lookup was two
// passes, folding each id and name, so a floor on what its five cost now),
// and 2.4–2.6 ms and 0.61 MB through an Index, its build included
// (BenchmarkResolvingADocumentsOwners, four vCPUs of a 2.8 GHz Xeon under a
// load average near 15) — a cost every data node paid at every apply, inside
// the apply. An Index costs one walk of the chart and then a map lookup a
// reference.
//
// [Organization.EffectiveLead] has the same shape for the same reason: an
// INHERITED lead — a team's lead named on the division above it, the usual
// shape — is found by searching the unit's whole subtree for the handle and
// then walking every seat of the chart by handle, so asking it of every unit,
// or of every repository's owning unit, costs units × seats. Asked of every
// unit of a chart of 1 100 units and 3 100 seats it took 53–64 ms and 26 MB
// through the chart, and 4.7–7.9 ms and 1.5 MB through an Index built for the
// purpose (BenchmarkAskingEveryUnitsLead, same machine and load; measured
// while a lead was a seat's name — the chart's walk now derives every seat's
// handle it compares, so it costs more, not less) — a map lookup and a walk
// of the seats SHARING the lead's handle, which a chart that passed admission
// has one of. The two passes of every apply that ask it — of every
// led unit for the leads' automatic reports ([Organization.Normalize]), of
// every unit with a schedule run as its lead for a human lead's
// ([Organization.Validate]) — through an Index each, went on that chart from
// 76–108 ms and 29 MB to 13–17 ms and 4.3 MB, and from 63–69 ms and 29 MB to
// 19–23 ms and 4.6 MB with every unit carrying such a schedule, and on one of
// 330 units and 930 seats from 6.3–7.5 ms to 4.1–4.6 ms and from 8.8–10 ms to
// 6.1–8.2 ms (BenchmarkNormalizingAndValidatingALedChart, same machine under a
// load average near 16). Each pass builds its Index at the first unit it asks
// about, so a chart that names no lead pays nothing for one.
//
// [Organization.Manager], [Organization.Role] — and so [Organization.Reports]
// — [Organization.UnitFor] and [Organization.UnitChainFor] are each a walk of
// the chart too, and their callers asked them of every seat: a list of every
// seat's managers and reports is seats² through them, and a map lookup a seat
// here. Every seat's unit took 154–185 ms and 82 MB through the chart on 3 100
// seats and 17–20 ms and 7.5 MB on 930 — UnitFor walks the units through an
// iterator, three and a half million allocations on the larger chart — and
// 7.9–12 ms and 2.1 MB and 2.6–3.6 ms and 0.8 MB through an Index, its build
// included (BenchmarkAskingEverySeatsUnit, same machine under a load average
// near 12).
//
// # The same answer, by construction and by test
//
// THE SAME RULE as the methods it stands for, and held to them for every
// reference a chart can be asked (TestTheIndexAnswersWhatTheChartDoes): a unit
// reference is a unit's KEY exactly, else its key under [NormalizeKey], a unit
// with an id answering before one keyed on its name and within each the first
// in walk order, else a name under the same fold that ONE unit alone carries
// ([Organization.UnitByRef]); a handle is compared exactly, the first seat in
// walk order answering ([Organization.Role] and [Organization.SeatByHandle]
// alike); a unit's effective lead is the first seat whose handle is its lead in
// its OWN subtree's walk order ([Unit.LeadRole]), else the first so named in
// the chart's ([Organization.Role]); a seat's managers are every seat whose
// manages lists its handle, once each, in walk order, the first of them its
// manager; and its unit chain runs from the outermost unit down to the first in
// walk order holding the seat itself, by pointer.
//
// A unit's subtree is ONE RUN of the chart's seat walk ([Organization.AllRoles]
// visits a unit's own seats, then each child's subtree, before anything after
// it), so the Index keeps each unit's run as a span of walk positions and each
// handle's seats in walk order: the subtree's first seat of a handle is the
// first of that handle at or past the span's start, inside it.
//
// BUILD IT FROM A FROZEN CHART, as everything that reads an organization is
// ("Build it, wire it, then treat it as frozen" on [Organization]): it is a
// snapshot of the chart it was built from, and a chart edited after it would
// answer what the Index does not know.
type Index struct {
	// keys is the four spellings [Organization.UnitByRef] tries before a
	// name, in its order: a key exactly on a unit with an id, then on one
	// keyed on its name, then each folded. The first unit in walk order
	// holds each spelling.
	keys [4]map[string]*Unit
	// names is each folded name, holding its unit where ONE unit carries it
	// and nil where two or more do — a name two units share names neither.
	names map[string]*Unit

	// roles is every seat in the chart's walk order ([Organization.AllRoles]),
	// so a seat's walk position is its index here.
	roles []*Role
	// named is the walk positions of the first and the last seat with each
	// handle, and nextNamed, at a seat's position, the position of the next
	// seat with the same handle, or -1 — each handle's seats in walk order,
	// one slice for the whole chart rather than one per handle (the last is
	// what the build links the next one from). A handle is unique once a
	// document passes admission, but an applied revision is not held to
	// admission, so the walk order still decides.
	named     map[string]nameRun
	nextNamed []int
	// spans is each unit's subtree as the half-open run of walk positions
	// its seats hold.
	spans map[*Unit]span
	// managers is, for each handle, the walk positions of the seats listing
	// it in their manages — each seat once, in walk order.
	managers map[string][]int
	// homes is the first unit in walk order holding each seat directly, and
	// parents each unit's parent, the first in walk order where a unit is
	// reached twice; a root seat and a top-level unit have none.
	homes   map[*Role]*Unit
	parents map[*Unit]*Unit

	unitKeys []string
	handles  []string
}

// span is a half-open run [start, end) of walk positions.
type span struct{ start, end int }

// nameRun is the walk positions of the first and the last seat of one handle.
type nameRun struct{ first, last int }

// Index builds the chart's [Index]. Nil-safe: a nil organization indexes
// nothing, and every lookup answers nil.
func (o *Organization) Index() *Index {
	x := &Index{
		keys:     [4]map[string]*Unit{{}, {}, {}, {}},
		names:    map[string]*Unit{},
		named:    map[string]nameRun{},
		spans:    map[*Unit]span{},
		managers: map[string][]int{},
		homes:    map[*Role]*Unit{},
		parents:  map[*Unit]*Unit{},
	}
	if o == nil {
		return x
	}
	claim := func(spelling map[string]*Unit, key string, u *Unit) {
		// FIRST IN WALK ORDER WINS within each spelling — the order
		// UnitByRef's passes meet them in.
		if _, taken := spelling[key]; !taken && key != "" {
			spelling[key] = u
		}
	}
	for u := range o.AllUnits() {
		key, kind := u.Key(), 1
		if strings.TrimSpace(u.ID) != "" {
			kind = 0
		}
		claim(x.keys[kind], key, u)
		claim(x.keys[2+kind], NormalizeKey(key), u)
		if name := NormalizeKey(u.Name); name != "" {
			if _, shared := x.names[name]; shared {
				x.names[name] = nil
			} else {
				x.names[name] = u
			}
		}
		x.unitKeys = append(x.unitKeys, key)
	}
	// THE SEATS IN [Organization.AllRoles]'s ORDER — the root seats, then
	// each unit's own seats before each child's subtree — walked here by
	// hand rather than through the iterator, because this walk is the one
	// that sees where each unit's subtree begins and ends.
	add := func(r *Role) {
		at := len(x.roles)
		x.roles = append(x.roles, r)
		x.nextNamed = append(x.nextNamed, -1)
		handle := r.Handle()
		run, seen := x.named[handle]
		if seen {
			x.nextNamed[run.last] = at
		} else {
			run.first = at
		}
		run.last = at
		x.named[handle] = run
		x.handles = append(x.handles, handle)
		for _, entry := range r.Manages {
			// ONCE A SEAT, however often its list repeats the handle: the
			// chart asks whether a seat's list contains it.
			if listed := x.managers[entry]; len(listed) == 0 || listed[len(listed)-1] != at {
				x.managers[entry] = append(listed, at)
			}
		}
	}
	var walk func(u, above *Unit)
	walk = func(u, above *Unit) {
		if _, reached := x.parents[u]; !reached && above != nil {
			x.parents[u] = above
		}
		start := len(x.roles)
		for _, r := range u.Roles {
			if _, placed := x.homes[r]; !placed {
				x.homes[r] = u
			}
			add(r)
		}
		for _, c := range u.Children {
			walk(c, u)
		}
		x.spans[u] = span{start: start, end: len(x.roles)}
	}
	for _, r := range o.Roles {
		add(r)
	}
	for _, u := range o.Units {
		walk(u, nil)
	}
	return x
}

// Role is [Organization.Role] against the indexed chart: the first seat in
// walk order with this handle, or nil.
func (x *Index) Role(handle string) *Role {
	run, named := x.named[handle]
	if !named || handle == "" {
		return nil
	}
	return x.roles[run.first]
}

// Managers is every seat listing r's handle in its manages, in walk order and
// each once — the seats [Organization.Manager] answers the first of.
func (x *Index) Managers(r *Role) []*Role {
	var out []*Role
	for _, at := range x.managers[r.Handle()] {
		out = append(out, x.roles[at])
	}
	return out
}

// Manager is [Organization.Manager] against the indexed chart: the first seat
// in walk order listing r's handle in its manages, or nil for a top-level
// seat or one with no handle.
func (x *Index) Manager(r *Role) *Role {
	handle := r.Handle()
	if listed := x.managers[handle]; len(listed) > 0 && handle != "" {
		return x.roles[listed[0]]
	}
	return nil
}

// Reports is [Organization.Reports] against the indexed chart: r's direct
// reports in the order r lists them, a handle naming no seat skipped.
func (x *Index) Reports(r *Role) []*Role {
	var out []*Role
	for _, handle := range r.Manages {
		if found := x.Role(handle); found != nil {
			out = append(out, found)
		}
	}
	return out
}

// UnitFor is [Organization.UnitFor] against the indexed chart: the first unit
// in walk order holding r as a direct member, or nil for a root seat or one the
// Index was not built over.
func (x *Index) UnitFor(r *Role) *Unit { return x.homes[r] }

// Parent is the unit holding u against the indexed chart — the first in walk
// order where a unit is reached twice — or nil for a top-level unit or one the
// Index was not built over: a unit's chain upward, a map lookup a step, where
// [Organization.UnitChainTo] walks the chart to find it.
func (x *Index) Parent(u *Unit) *Unit { return x.parents[u] }

// UnitChainFor is [Organization.UnitChainFor] against the indexed chart: the
// units from the outermost down to the first in walk order holding r as a
// direct member, or nothing for a root seat or one the Index was not built
// over.
func (x *Index) UnitChainFor(r *Role) []*Unit {
	var chain []*Unit
	for u := x.homes[r]; u != nil; u = x.parents[u] {
		chain = append(chain, u)
	}
	slices.Reverse(chain)
	return chain
}

// EffectiveLead is [Organization.EffectiveLead] against the indexed chart: the
// seat leading u — the first seat whose handle is its lead in u's own
// subtree, else the first so named in the chart, so a lead inherited from an
// ancestor answers — or nil where u names no lead or no seat holds the
// handle. A unit
// the Index was not built over (another chart's, or nil) has no lead here:
// nil.
//
// A map lookup and a walk of the seats SHARING the lead's handle, which a
// chart that passed admission has one of, where the chart's own method
// searches the unit's subtree and then every seat it holds.
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

// Members is every seat of u's subtree — its own seats, then each child's
// subtree's, in walk order — against the indexed chart: one slice of the
// walk the index already holds, where [Unit.AllRoles] walks the subtree
// again. Nothing for a unit the Index was not built over. A copy: the caller
// may keep it.
func (x *Index) Members(u *Unit) []*Role {
	s, held := x.spans[u]
	if !held {
		return nil
	}
	return slices.Clone(x.roles[s.start:s.end])
}

// UnitByRef is [Organization.UnitByRef] against the indexed chart: the unit a
// durable reference names — by its key, exactly and then folded, or by a name
// one unit alone carries — or nil.
func (x *Index) UnitByRef(ref string) *Unit {
	if ref == "" {
		return nil
	}
	folded := NormalizeKey(ref)
	for i, spelling := range x.keys {
		key := ref
		if i >= 2 {
			key = folded
		}
		if u, ok := spelling[key]; ok {
			return u
		}
	}
	if folded == "" {
		return nil
	}
	return x.names[folded]
}

// SeatByHandle is [Organization.SeatByHandle] against the indexed chart: the
// seat of either kind with this handle, or nil — [Index.Role], as the chart's
// own two are one lookup.
func (x *Index) SeatByHandle(handle string) *Role { return x.Role(handle) }

// UnitKeys is every unit's [Unit.Key], in walk order — what a refusal of a
// unit reference names as the units there are. A copy: the caller may keep it.
func (x *Index) UnitKeys() []string { return append([]string(nil), x.unitKeys...) }

// Handles is every seat's handle, in walk order — what a refusal of a handle
// names as the seats there are. A copy: the caller may keep it.
func (x *Index) Handles() []string { return append([]string(nil), x.handles...) }
