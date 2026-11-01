package org

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// THE INDEX ANSWERS WHAT THE CHART DOES, for every reference a chart can be
// asked: each unit's id, name and key and each seat's handle, in other cases
// and with space around them, references naming nothing, and the empty one —
// over a chart carrying every ambiguity an applied revision can (an id that is
// another unit's name-standing-in-for-a-key, in both orders; one name on two
// units, which names neither; one handle on two seats, which only an admission
// rule refuses). An Index that answered otherwise would make the code host
// record a different owner than the one its validation accepted.
//
// Mutation: let a later unit win a spelling, try a unit keyed on its name
// before one with an id, fold before the exact key, or let a shared name
// answer its first unit, and a row goes red.
func TestTheIndexAnswersWhatTheChartDoes(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T",
		Roles: []*Role{{Name: "CEO", DeclaredHandle: "ceo"}, {Name: "Advisor"}},
		Units: []*Unit{
			{Name: "platform", Roles: []*Role{{Name: "A"}}},
			{Name: "Platform Services", ID: "platform", Roles: []*Role{{Name: "B"}},
				Children: []*Unit{
					{Name: "Infra", ID: "infra", Roles: []*Role{{Name: "C", DeclaredHandle: "ceo"}}},
				}},
			{Name: "Engineering", ID: "eng", Roles: []*Role{{Name: "Dev"}}},
			{Name: "Twice", Roles: []*Role{{Name: "D"}}},
			{Name: "twice", ID: "second-twice", Roles: []*Role{{Name: "E"}}},
			{Name: "Ops", ID: "Engineering", Roles: []*Role{{Name: "F"}}},
		},
	})
	x := o.Index()

	refs := []string{"", "   ", "nobody", "NOBODY", "infra/child"}
	for u := range o.AllUnits() {
		for _, s := range []string{u.ID, u.Name, u.Key()} {
			refs = append(refs, s, strings.ToUpper(s), strings.ToLower(s), "  "+s+"  ")
		}
	}
	for _, ref := range refs {
		if got, want := x.UnitByRef(ref), o.UnitByRef(ref); got != want {
			t.Errorf("UnitByRef(%q): the index answers %s, the chart %s", ref, unitName(got), unitName(want))
		}
	}

	handles := []string{"", "nobody", "CEO", " ceo"}
	for r := range o.AllRoles() {
		handles = append(handles, r.Handle(), strings.ToUpper(r.Handle()))
	}
	for _, h := range handles {
		if got, want := x.SeatByHandle(h), o.SeatByHandle(h); got != want {
			t.Errorf("SeatByHandle(%q): the index answers %s, the chart %s", h, roleName(got), roleName(want))
		}
	}

	var keys, all []string
	for u := range o.AllUnits() {
		keys = append(keys, u.Key())
	}
	for r := range o.AllRoles() {
		all = append(all, r.Handle())
	}
	if got := fmt.Sprint(x.UnitKeys()); got != fmt.Sprint(keys) {
		t.Errorf("UnitKeys() = %s, want every unit's key in walk order, %v", got, keys)
	}
	if got := fmt.Sprint(x.Handles()); got != fmt.Sprint(all) {
		t.Errorf("Handles() = %s, want every seat's handle in walk order, %v", got, all)
	}

	var none *Organization
	if u, r := none.Index().UnitByRef("eng"), none.Index().SeatByHandle("ceo"); u != nil || r != nil {
		t.Errorf("a nil chart's index answers %s and %s, want nothing", unitName(u), roleName(r))
	}

	// EVERY UNIT'S EFFECTIVE LEAD, on a chart with every shape the rule has
	// to tell apart: a lead among the unit's own seats, one in a grandchild,
	// one inherited from the division above (Normalize copies the handle
	// down), one naming nobody, none at all, one in a sibling's subtree, and
	// a handle three seats share — one in the unit's subtree after another in
	// the chart's walk, so the subtree's must win, and for a unit whose
	// subtree holds none of them, one before it and one after, so the
	// chart's first must, and not the next past the unit's own seats. The
	// three are told apart by their names, since they share the handle.
	//
	// Mutations: answer the chart's first seat of the handle always (Dup
	// goes red), the subtree's alone (API, Left and Elsewhere), or read a
	// subtree as running to the chart's end (Elsewhere answers Twin After).
	led := normalized(&Organization{Name: "Led",
		Roles: []*Role{{Name: "Twin Root", DeclaredHandle: "twin"}},
		Units: []*Unit{
			{Name: "Eng", Lead: "eng-lead", Roles: []*Role{{Name: "Eng Lead"}, {Name: "E1"}},
				Children: []*Unit{
					{Name: "API", Roles: []*Role{{Name: "A1"}}},
					{Name: "Web", Lead: "web-lead", Roles: []*Role{{Name: "W1"}},
						Children: []*Unit{{Name: "Web Core", Roles: []*Role{{Name: "Web Lead"}}}}},
				}},
			{Name: "Ops", Lead: "ghost", Roles: []*Role{{Name: "O1"}}},
			{Name: "Plain", Roles: []*Role{{Name: "P1"}}},
			{Name: "Left", Lead: "r1", Roles: []*Role{{Name: "L1"}}},
			{Name: "Right", Roles: []*Role{{Name: "R1"}}},
			{Name: "Dup", Lead: "twin", Roles: []*Role{{Name: "D1"}},
				Children: []*Unit{{Name: "Dup Child", Roles: []*Role{{Name: "Twin Dup", DeclaredHandle: "twin"}}}}},
			{Name: "Elsewhere", Lead: "twin", Roles: []*Role{{Name: "X1"}}},
			{Name: "After", Roles: []*Role{{Name: "Twin After", DeclaredHandle: "twin"}}},
		},
	})
	lx := led.Index()
	answered := 0
	for u := range led.AllUnits() {
		got, want := lx.EffectiveLead(u), led.EffectiveLead(u)
		if got != want {
			t.Errorf("EffectiveLead(%s): the index answers %s, the chart %s", unitName(u), roleName(got),
				roleName(want))
		}
		if want != nil {
			answered++
		}
		// A UNIT'S MEMBERS ARE ITS SUBTREE'S SEATS, as the unit's own walk
		// holds them, in its order.
		if got, want := lx.Members(u), slices.Collect(u.AllRoles()); !slices.Equal(got, want) {
			t.Errorf("Members(%s): the index answers %s, the unit %s", unitName(u), roleNames(got),
				roleNames(want))
		}
	}
	if got := handlesOf(lx.Members(led.Unit("Eng"))); fmt.Sprint(got) != "[eng-lead e1 a1 w1 web-lead]" {
		t.Errorf("the premise: Eng's members are %v, want its own seats then API's, Web's and Web Core's", got)
	}
	if got := lx.Members(o.Unit("Engineering")); got != nil {
		t.Errorf("another chart's unit answered the members %s, want none", roleNames(got))
	}
	// AND THE CASES ARE THE ONES THEY CLAIM TO BE, or the comparison above
	// holds two nils equal and proves nothing.
	for unit, name := range map[string]string{
		"API": "Eng Lead", "Web": "Web Lead", "Left": "R1", "Dup": "Twin Dup", "Elsewhere": "Twin Root",
	} {
		if got := lx.EffectiveLead(led.Unit(unit)); got == nil || got.Name != name {
			t.Errorf("EffectiveLead(%q) = %s, want the seat %q", unit, roleName(got), name)
		}
	}
	if answered != 8 {
		t.Errorf("%d units answered a lead, want 8 (Eng, API, Web, Web Core, Left, Dup, Dup Child, Elsewhere)", answered)
	}
	// A unit the Index was not built over is none of its units, and nil has
	// no lead.
	if got := lx.EffectiveLead(o.Unit("Engineering")); got != nil {
		t.Errorf("another chart's unit answered the lead %s, want none", roleName(got))
	}
	if got := none.Index().EffectiveLead(nil); got != nil {
		t.Errorf("a nil unit answered the lead %s, want none", roleName(got))
	}

	// EVERY SEAT'S PLACE IN THE HIERARCHY — the seat a handle answers, its
	// manager and every seat that manages it, its reports, its unit chain —
	// on a chart with every shape those rules tell apart: a root seat
	// managing across units, a seat three others manage (its lead, which
	// Normalize makes manage its unit's seats, then two lists after it in
	// the walk), a list naming nobody, a handle two seats share (the first
	// in the walk answers it), a seat three units deep and a root seat with
	// no unit — and a list naming one seat twice, which Normalize never
	// leaves, so it is written after: the chart asks whether a list
	// contains a handle, so the seat is its manager once.
	//
	// Mutations: keep a seat once a time its list names it (O1's), keep the
	// last seat of a handle (shared), stop a chain at its home (Deep's), or
	// answer a seat's unit as its outermost (Deep's).
	ranked := normalized(&Organization{Name: "Ranked",
		Roles: []*Role{
			{Name: "CEO", Manages: []string{"vp", "ghost", "vp"}},
			{Name: "Shared Root", DeclaredHandle: "shared"},
		},
		Units: []*Unit{
			{Name: "Eng", Lead: "vp", Roles: []*Role{{Name: "VP"}, {Name: "E1"}},
				Children: []*Unit{
					{Name: "Core", Roles: []*Role{{Name: "C1", Manages: []string{"deep", "shared"}}},
						Children: []*Unit{{Name: "Kernel", Roles: []*Role{{Name: "Deep"}}}}},
				}},
			{Name: "Ops", Roles: []*Role{
				{Name: "Shared Ops", DeclaredHandle: "shared"},
				{Name: "O1", Manages: []string{"deep", "deep"}},
			}},
		},
	})
	twice := ranked.Role("o1")
	twice.Manages = append(twice.Manages, "deep")
	if strings.Count(fmt.Sprint(twice.Manages), "deep") != 2 {
		t.Fatalf("the premise: O1's list is %v, want deep in it twice", twice.Manages)
	}
	rx := ranked.Index()
	handles = []string{"", "ghost", "nobody"}
	for r := range ranked.AllRoles() {
		handles = append(handles, r.Handle())
		if got, want := rx.Manager(r), ranked.Manager(r); got != want {
			t.Errorf("Manager(%s): the index answers %s, the chart %s", roleName(r), roleName(got), roleName(want))
		}
		var managers []*Role
		for m := range ranked.AllRoles() {
			if slices.Contains(m.Manages, r.Handle()) {
				managers = append(managers, m)
			}
		}
		if got := rx.Managers(r); !slices.Equal(got, managers) {
			t.Errorf("Managers(%s): the index answers %s, the chart %s", roleName(r), roleNames(got),
				roleNames(managers))
		}
		if got, want := rx.Reports(r), ranked.Reports(r); !slices.Equal(got, want) {
			t.Errorf("Reports(%s): the index answers %s, the chart %s", roleName(r), roleNames(got), roleNames(want))
		}
		if got, want := rx.UnitFor(r), ranked.UnitFor(r); got != want {
			t.Errorf("UnitFor(%s): the index answers %s, the chart %s", roleName(r), unitName(got), unitName(want))
		}
		if got, want := rx.UnitChainFor(r), ranked.UnitChainFor(r); !slices.Equal(got, want) {
			t.Errorf("UnitChainFor(%s): the index answers %v, the chart %v", roleName(r), unitNames(got),
				unitNames(want))
		}
	}
	for _, handle := range handles {
		if got, want := rx.Role(handle), ranked.Role(handle); got != want {
			t.Errorf("Role(%q): the index answers %s, the chart %s", handle, roleName(got), roleName(want))
		}
	}
	// AND THE CASES ARE THE ONES THEY CLAIM TO BE.
	deep := ranked.Role("deep")
	if got := handlesOf(rx.Managers(deep)); fmt.Sprint(got) != "[vp c1 o1]" {
		t.Errorf("the premise: Deep's managers are %v, want [vp c1 o1] (its lead by Normalize, then two lists)", got)
	}
	if got := unitNames(rx.UnitChainFor(deep)); fmt.Sprint(got) != "[Eng Core Kernel]" {
		t.Errorf("the premise: Deep's chain is %v, want [Eng Core Kernel]", got)
	}
	if got := rx.Role("shared"); got == nil || got.Name != "Shared Root" {
		t.Errorf("the premise: the handle shared answers %s, want the root seat", roleName(got))
	}
	vp := ranked.Role("vp")
	if got := handlesOf(rx.Reports(vp)); !slices.Contains(got, "e1") {
		t.Errorf("the premise: VP's reports are %v, want its unit's E1 among them", got)
	}
	if got := rx.Manager(ranked.Role("ceo")); got != nil || rx.UnitChainFor(ranked.Role("ceo")) != nil {
		t.Errorf("the premise: the root CEO has the manager %s or a unit chain", roleName(got))
	}
	if got := rx.UnitFor(deep); got == nil || got.Name != "Kernel" {
		t.Errorf("the premise: Deep's unit is %s, want Kernel, its own and not the outermost", unitName(got))
	}
	// A seat the Index was not built over has no place in it.
	if got := rx.UnitChainFor(&Role{Name: "Deep"}); got != nil {
		t.Errorf("another chart's seat has the chain %v, want none", unitNames(got))
	}
	if got := rx.UnitFor(&Role{Name: "Deep"}); got != nil {
		t.Errorf("another chart's seat has the unit %s, want none", unitName(got))
	}
}

func handlesOf(seats []*Role) []string {
	out := make([]string, 0, len(seats))
	for _, r := range seats {
		out = append(out, r.Handle())
	}
	return out
}

func unitName(u *Unit) string {
	if u == nil {
		return "no unit"
	}
	return fmt.Sprintf("%q (id %q)", u.Name, u.ID)
}

func roleName(r *Role) string {
	if r == nil {
		return "no seat"
	}
	return fmt.Sprintf("%q (handle %q)", r.Name, r.Handle())
}

// BenchmarkResolvingADocumentsOwners is ten thousand owner lookups — the most
// repositories a code.repos list may declare — against a chart of 300 units
// and 900 seats, through the Index and through the chart's own one-off
// lookups, the cost the Index replaced in the code host's validation and
// reconcile.
func BenchmarkResolvingADocumentsOwners(b *testing.B) {
	o := &Organization{Name: "T"}
	for u := range 300 {
		unit := &Unit{Name: fmt.Sprintf("Unit %d", u), ID: fmt.Sprintf("unit-%d", u)}
		for s := range 3 {
			unit.Roles = append(unit.Roles, &Role{Name: fmt.Sprintf("Seat %d %d", u, s)})
		}
		o.Units = append(o.Units, unit)
	}
	o.Normalize()
	units, seats := make([]string, 10_000), make([]string, 10_000)
	for i := range units {
		units[i] = fmt.Sprintf("Unit %d", i%300)
		seats[i] = Slugify(fmt.Sprintf("Seat %d %d", i%300, i%3))
	}
	b.Run("index", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			x := o.Index()
			for i := range units {
				if x.UnitByRef(units[i]) == nil || x.SeatByHandle(seats[i]) == nil {
					b.Fatal("an owner did not resolve")
				}
			}
		}
	})
	b.Run("chart", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for i := range units {
				if o.UnitByRef(units[i]) == nil || o.SeatByHandle(seats[i]) == nil {
					b.Fatal("an owner did not resolve")
				}
			}
		}
	})
}

// ledChart is a chart of divisions, each led by a seat of its own, with ten
// teams of three seats under each that name no lead and so inherit the
// division's — the usual shape, and the one that makes the chart's own
// [Organization.EffectiveLead] dear: a team's inherited lead is outside its
// subtree, so it is found by a walk of every seat in the chart. The config
// warnings' and the engine roster's benchmarks build the same shape.
func ledChart(divisions int) *Organization {
	o := ledChartAsWritten(divisions)
	o.Normalize()
	return o
}

// ledChartAsWritten is [ledChart] before [Organization.Normalize]: what a
// decoded document hands the chart.
func ledChartAsWritten(divisions int) *Organization {
	o := &Organization{Name: "T"}
	for d := range divisions {
		division := &Unit{Name: fmt.Sprintf("Division %d", d), Lead: Slugify(fmt.Sprintf("Lead %d", d)),
			Roles: []*Role{{Name: fmt.Sprintf("Lead %d", d)}}}
		for t := range 10 {
			team := &Unit{Name: fmt.Sprintf("Team %d %d", d, t)}
			for s := range 3 {
				team.Roles = append(team.Roles, &Role{Name: fmt.Sprintf("Seat %d %d %d", d, t, s)})
			}
			division.Children = append(division.Children, team)
		}
		o.Units = append(o.Units, division)
	}
	return o
}

// BenchmarkAskingEveryUnitsLead is every unit's effective lead asked of a chart
// whose teams inherit their division's lead — 30 divisions (330 units, 930
// seats) and 100 (1 100 units, 3 100 seats) — through an [Index], whose build
// it includes, and through the chart's own [Organization.EffectiveLead]: the
// question the code host's grant roster asks of every unit once an epoch, and
// its `visibility: unit` warning of every repository's owning unit.
func BenchmarkAskingEveryUnitsLead(b *testing.B) {
	for _, divisions := range []int{30, 100} {
		o := ledChart(divisions)
		units := slices.Collect(o.AllUnits())
		name := fmt.Sprintf("%d_units", len(units))
		b.Run(name+"/index", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				x := o.Index()
				for _, u := range units {
					if x.EffectiveLead(u) == nil {
						b.Fatal("a unit has no lead")
					}
				}
			}
		})
		b.Run(name+"/chart", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for _, u := range units {
					if o.EffectiveLead(u) == nil {
						b.Fatal("a unit has no lead")
					}
				}
			}
		})
	}
}

// BenchmarkAskingEverySeatsUnit is every seat's own unit asked of a led chart —
// 330 units and 930 seats, and 1 100 and 3 100 — through an [Index], whose
// build it includes, and through the chart's own [Organization.UnitFor], a walk
// of every unit's seats until one holds the seat: the question every apply's
// tracker projects ask of every seat (a seat's own project names its home
// unit), and every spend chart's per-unit band. The figures are in [Index]'s
// doc.
func BenchmarkAskingEverySeatsUnit(b *testing.B) {
	for _, divisions := range []int{30, 100} {
		o := ledChart(divisions)
		seats := slices.Collect(o.AllRoles())
		name := fmt.Sprintf("%d_seats", len(seats))
		b.Run(name+"/index", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				x := o.Index()
				for _, r := range seats {
					if x.UnitFor(r) == nil {
						b.Fatal("a seat has no unit")
					}
				}
			}
		})
		b.Run(name+"/chart", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for _, r := range seats {
					if o.UnitFor(r) == nil {
						b.Fatal("a seat has no unit")
					}
				}
			}
		})
	}
}

// BenchmarkNormalizingAndValidatingALedChart is what every apply pays for its
// chart on every node — [Organization.Normalize] when the epoch is built and
// [Organization.Validate] when it is admitted — over a led chart of 330 units
// and 930 seats and of 1 100 and 3 100: the two passes that ask a unit for its
// effective lead (the leads' automatic reports of every led unit, a human
// lead's schedules of every unit with a schedule run as its lead) and did so
// through the chart's own method, a walk of every seat for each inherited
// lead. Every unit carries a weekly report run as its lead, so validation
// asks every unit's lead — its worst case. The figures are in [Index]'s doc.
func BenchmarkNormalizingAndValidatingALedChart(b *testing.B) {
	for _, divisions := range []int{30, 100} {
		name := fmt.Sprintf("%d_divisions", divisions)
		b.Run(name+"/normalize", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				o := ledChartAsWritten(divisions)
				b.StartTimer()
				o.Normalize()
			}
		})
		o := ledChart(divisions)
		for u := range o.AllUnits() {
			u.Schedules = []Schedule{{Name: "report", Cron: "0 17 * * 5", Task: "weekly report", Target: TargetLead}}
		}
		b.Run(name+"/validate", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := o.Validate(); err != nil {
					b.Fatalf("a led chart is invalid: %v", err)
				}
			}
		})
	}
}
