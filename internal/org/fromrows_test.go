package org_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/org"
)

// THE VIEW'S OWN PROPERTIES, as distinct from its equivalence with the
// document derivation.
//
// The equivalence gate lives in [internal/config], because only that package
// can import a parsed document and both derivations at once. What is here is
// everything the view has to be true of on its own: that it terminates, that
// two builders agree, that the refusals a human seat depends on survive the
// move from a tree to a lookup, and what a company of twenty thousand seats
// costs to build.

// A HUMAN SEAT NEVER GETS AN AGENT ID, THROUGH THE VIEW AS THROUGH THE TREE.
//
// A human seat is addressable and never spawned, so it has no agent id at all
// — not a zero one. The refusal is what keeps every human out of `GET /agents`
// (it is that route's SOLE filter), out of the budget breakdown and out of
// agent memory, and a lookup that answered an id for every row would put them
// in all three at once.
func TestAHumanSeatHasNoAgentIDInTheView(t *testing.T) {
	t.Parallel()

	view := org.FromRows(chart.Authored{
		Units: []chart.AuthoredUnit{{Key: "eng", Name: "Engineering"}},
		Seats: []chart.AuthoredSeat{
			{Handle: "ada", Unit: "eng", Kind: chart.SeatAgent, Name: "Ada"},
			{Handle: "sarah", Unit: "eng", Kind: chart.SeatHuman, Name: "Sarah"},
		},
	}.Rows(), org.Settings{Name: "Acme"})

	agent := view.Org.Role("ada")
	if agent == nil {
		t.Fatal("the agent seat did not reach the view")
	}
	if _, ok := view.Org.AgentIDFor(agent); !ok {
		t.Error("an agent seat has no id, so nothing can route to its inbox")
	}

	human := view.Org.Role("sarah")
	if human == nil {
		t.Fatal("the human seat did not reach the view")
	}
	if id, ok := view.Org.AgentIDFor(human); ok {
		t.Errorf("a human seat was given the agent id %s — that refusal is "+
			"GET /agents' only human filter, and an id here puts every "+
			"person in the company into the agent roster, the budget "+
			"breakdown and agent memory at once", id)
	}
	if seat := view.Org.AgentSeatByHandle("sarah"); seat != nil {
		t.Error("a human seat answered an agent lookup, whose callers publish " +
			"to an inbox a human seat does not have")
	}
}

// A SEAT UNDER A UNIT NOTHING DECLARES STAYS AT THE ROOT.
//
// The same answer the document path gives for a `unit:` reference that
// resolves to nothing: the seat is still in the company, and
// [Organization.DanglingRefs] is what reports the reference rather than the
// build refusing it. A chart is assembled in pieces, and every intermediate
// state is one every node applies.
func TestASeatUnderAUnitNothingDeclaresStaysAtTheRoot(t *testing.T) {
	t.Parallel()

	view := org.FromRows(chart.Authored{
		Seats: []chart.AuthoredSeat{
			{Handle: "ada", Unit: "nowhere", Kind: chart.SeatAgent, Name: "Ada"},
		},
	}.Rows(), org.Settings{Name: "Acme"})

	if len(view.Org.Roles) != 1 {
		t.Fatalf("the root holds %d seats, want the one whose unit is not "+
			"there: %+v", len(view.Org.Roles), view.Org.Roles)
	}
	if view.Org.Role("ada") == nil {
		t.Error("the seat is not in the company at all, so a chart being " +
			"assembled in pieces loses whoever arrives before their team")
	}
}

// THE VIEW CARRIES THE POSITION ITS ROWS WERE READ AT.
//
// A company derived from a document is true of that document; one derived from
// a log is true as of a POSITION, and a surface that renders one has to be able
// to say which.
func TestTheViewCarriesThePositionItWasBuiltFrom(t *testing.T) {
	t.Parallel()

	rows := chart.Authored{
		Units: []chart.AuthoredUnit{{Key: "eng", Name: "Engineering"}},
	}.Rows()
	rows.Position.Stream = chart.Domain{}.Stream().Name
	rows.Position.Generation = 2
	rows.Position.Seq = 41

	view := org.FromRows(rows, org.Settings{Name: "Acme"})
	if view.At.Generation != 2 || view.At.Seq != 41 {
		t.Errorf("the view was built at %+v, want the rows' own position — "+
			"without it nothing rendering this company can say what it is "+
			"true as of", view.At)
	}
}

// THE SETTINGS REACH THE VIEW AND THE CHART DOES NOT CARRY THEM.
//
// A mission is prose, a token budget is a meter's ceiling, and a knowledge
// scope is a read rule. None is a fact about the hierarchy, so none is a record
// on the chart's log — and a change to the company's mission therefore wakes
// nobody.
func TestTheSettingsReachTheViewWithoutTouchingTheChart(t *testing.T) {
	t.Parallel()

	settings := org.Settings{
		Name: "Acme", Mission: "ship it", Vision: "everywhere",
		Policies: []string{"be kind"}, TokenBudget: 1000,
		KnowledgeScope: []string{"ENG"},
	}
	view := org.FromRows(chart.Authored{}.Rows(), settings)

	if view.Org.Name != "Acme" || view.Org.Mission != "ship it" {
		t.Errorf("the settings did not reach the view: %+v", view.Org)
	}
	if view.Org.TokenBudget != 1000 {
		t.Errorf("the token budget is %d, want 1000", view.Org.TokenBudget)
	}
	if !slices.Equal(view.Org.KnowledgeScope, []string{"ENG"}) {
		t.Errorf("the knowledge scope is %v", view.Org.KnowledgeScope)
	}
}

// BenchmarkFromRows is what building a large company costs.
//
// NOT A GATE and it carries no threshold: what it is for is the window a
// config apply gets, which is a number somebody has to pick from a measurement
// rather than from a feeling. Twenty thousand seats is an order of magnitude
// past the largest company this engine is designed for, so a build that is
// comfortable here is comfortable everywhere.
//
// # What it measured, and the quadratic it found
//
// On a 4-core runner, over the fan-shaped tree [generatedCompany] builds:
//
//	   200 seats:   0.4 ms,   0.3 MB
//	 2,000 seats:   5.4 ms,   3.8 MB
//	20,000 seats:  43.7 ms,  38.3 MB
//
// Those are the numbers a config apply's window is sized against.
//
// THEY ARE THE NUMBERS AFTER A FIX THIS BENCHMARK IS WHAT FOUND. Before it,
// twenty thousand seats took 1.23 SECONDS and 970 MB — 95% of it in
// `managesIndex.managed`, because lead inheritance makes a root seat the
// effective lead of every unit and its resolved manages set was rebuilt from
// scratch at each one. Keeping it across the walk is behaviour-preserving,
// which the equivalence gate is what proves, and it is pinned against
// regression by [TestALeadsManagedSetIsBuiltOncePerLeadRatherThanPerUnit] —
// a gate rather than this, because a benchmark has no threshold and nothing
// fails when it moves.
//
// A SECOND, MILDER QUADRATIC IS LEFT AS IT IS: the index holds every
// DESCENDANT's handle under every ancestor's key, so a unit N levels down
// contributes its seats to N lists. A fan-shaped company is five levels deep
// and pays almost nothing; the same seats in a four-hundred-deep chain cost
// far more. That shape is not a company, and the index is what makes
// `manages: [engineering]` expand to a division's every seat, which is the
// feature. What this comment buys is that the next person to measure a deep
// chart knows the answer before they look.
func BenchmarkFromRows(b *testing.B) {
	for _, seats := range []int{200, 2_000, 20_000} {
		b.Run(fmt.Sprintf("seats=%d", seats), func(b *testing.B) {
			rows := generatedCompany(seats).Rows()
			settings := org.Settings{Name: "Acme"}
			b.ReportAllocs()
			for b.Loop() {
				_ = org.FromRows(rows, settings)
			}
		})
	}
}

// TestTheBuilderHandlesAGeneratedCompany is the benchmark's correctness half.
//
// A benchmark that built a wrong tree quickly would report a number nobody
// should act on, so the same generator is walked once here and its result
// checked: every seat present, every unit placed, and the leads resolved.
func TestTheBuilderHandlesAGeneratedCompany(t *testing.T) {
	t.Parallel()

	const seats = 2_000
	view := org.FromRows(generatedCompany(seats).Rows(), org.Settings{Name: "Acme"})

	count := 0
	for range view.Org.AllRoles() {
		count++
	}
	if count != seats {
		t.Errorf("the view holds %d seats, want %d", count, seats)
	}
	if len(view.Reparented) != 0 {
		t.Errorf("a generated company was reparented: %v", view.Reparented)
	}
	// EVERY TEAM'S LEAD RESOLVED, which is the derivation a large chart is
	// most likely to get wrong: it cascades down chains the generator makes
	// deep on purpose.
	for u := range view.Org.AllUnits() {
		if u.Lead == "" {
			t.Fatalf("unit %q resolved no lead, so an escalation from it "+
				"reaches nobody", u.Key())
		}
	}
}

// generatedCompany is a chart of n seats in the shape a real company that size
// has: FOUR LEVELS, fanning out.
//
// A division holds divisions holds departments holds teams, ten seats to a
// team and five children to a parent — which puts twenty thousand seats about
// five levels down rather than four hundred. That distinction is not cosmetic
// here, and [BenchmarkFromRows] says why: this derivation is quadratic in
// DEPTH, because the manages index holds every descendant's handle under every
// ancestor's key. A chain of four hundred units therefore costs four hundred
// times what a fan of the same size does, and a generator that built one would
// report a number nobody should size a config apply against.
func generatedCompany(seats int) chart.Authored {
	const perTeam = 10
	const fanOut = 5

	var out chart.Authored
	// THE ROOT LEAD, so every unit below resolves one through the cascade
	// rather than each declaring its own — which is what makes this
	// measure inheritance rather than lookup.
	out.Seats = append(out.Seats, chart.AuthoredSeat{
		Handle: "ceo", Kind: chart.SeatAgent, Name: "Chief",
	})
	placed := 1

	// THE DEPTH IS DERIVED FROM THE SIZE, so the tree stays as shallow as
	// the fan allows: enough levels to hold the teams and not one more.
	// Twenty thousand seats is two thousand teams, which five-way fanning
	// reaches in five levels — a real company's shape, and the one the
	// numbers in [BenchmarkFromRows] are measured over.
	teams := (seats + perTeam - 1) / perTeam
	depth := 1
	for reach := fanOut; reach < teams; reach *= fanOut {
		depth++
	}

	// A BREADTH-FIRST BUILD, so every level is full before the next opens.
	type pending struct {
		key   string
		level int
	}
	out.Units = append(out.Units, chart.AuthoredUnit{
		Key: "org", Name: "Org", Lead: "ceo", Channel: "c-org",
	})
	queue := []pending{{key: "org", level: 0}}
	next := 0

	for len(queue) > 0 && placed < seats {
		at := queue[0]
		queue = queue[1:]
		for range fanOut {
			if placed >= seats {
				break
			}
			key := fmt.Sprintf("unit-%05d", next)
			next++
			out.Units = append(out.Units, chart.AuthoredUnit{
				Key: key, Parent: at.key, Name: key,
			})
			// A LEAF HOLDS THE SEATS. Every other level holds units,
			// which is what makes the seats sit at the bottom of a
			// real chart rather than spread through it.
			if at.level+1 >= depth {
				for range perTeam {
					if placed >= seats {
						break
					}
					out.Seats = append(out.Seats, chart.AuthoredSeat{
						Handle: fmt.Sprintf("seat-%05d", placed),
						Unit:   key, Kind: chart.SeatAgent,
						Name: fmt.Sprintf("Seat %05d", placed),
					})
					placed++
				}
				continue
			}
			queue = append(queue, pending{key: key, level: at.level + 1})
		}
	}
	return out
}
