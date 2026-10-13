package org

// Index is the chart's two durable lookups — a unit by reference and a seat
// by handle — built once, for a caller that asks them of many references
// against one chart: the code host's validation and its reconcile resolve the
// owner of every repository a document declares, up to ten thousand of them.
//
// # Why it exists
//
// [Organization.UnitByRef] is two passes over every unit, folding each id and
// name, and [Organization.SeatByHandle] a pass over every seat, deriving each
// handle — the right shape for one lookup a request makes, and quadratic for a
// document's worth: ten thousand owners looked up by unit and by seat on a
// chart of 300 units and 900 seats took 3.9–4.1 s and 620 MB through them,
// and 1.8–1.9 ms and 0.4 MB through an Index (BenchmarkResolvingADocumentsOwners,
// four vCPUs of a 2.1 GHz Xeon) — a cost every data node paid at every apply,
// inside the apply. An Index costs one walk of the chart and then a map lookup
// a reference.
//
// # The same answer, by construction and by test
//
// THE SAME RULE as the two methods it stands for, and held to them for every
// reference a chart can be asked (TestTheIndexAnswersWhatTheChartDoes): a unit
// reference folds as [foldUnitKey] folds it, AN ID WINS an ambiguity with
// another unit's name, and within one spelling the first unit in walk order
// answers; a handle is compared exactly, the first seat in walk order
// answering.
//
// BUILD IT FROM A FROZEN CHART, as everything that reads an organization is
// ("Build it, wire it, then treat it as frozen" on [Organization]): it is a
// snapshot of the chart it was built from, and a chart edited after it would
// answer what the Index does not know.
type Index struct {
	unitsByID   map[string]*Unit
	unitsByName map[string]*Unit
	seats       map[string]*Role

	unitKeys []string
	handles  []string
}

// Index builds the chart's [Index]. Nil-safe: a nil organization indexes
// nothing, and every lookup answers nil.
func (o *Organization) Index() *Index {
	x := &Index{
		unitsByID:   map[string]*Unit{},
		unitsByName: map[string]*Unit{},
		seats:       map[string]*Role{},
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
	for r := range o.AllRoles() {
		handle := r.Handle()
		if _, taken := x.seats[handle]; !taken && handle != "" {
			x.seats[handle] = r
		}
		x.handles = append(x.handles, handle)
	}
	return x
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
