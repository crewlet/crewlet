package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// THE EQUIVALENCE GATE, and it is the one this whole split turns on.
//
// The org chart is moving out of the company document and onto a log. Two
// derivations therefore exist at once: [config.Company.Organization], which
// parses a document and NORMALIZES a tree in place, and [org.FromRows], which
// is a pure function over the rows an import writes. If they disagree, every
// company that migrates gets a different chart — a different lead, a different
// roster, a different manager — and nothing would say so, because both answers
// are internally consistent.
//
// So this compares them, over every example and fixture this repository ships,
// on the derivations that matter: the tree's shape, each unit's effective lead
// and channel, each seat's placement, and every expanded `manages` list.
//
// # It compares against Normalize run TWICE
//
// Normalize is idempotent only because each step records what was DECLARED
// before it overwrites it — a discipline every future step has to remember. A
// comparison against ONE pass would therefore certify strictly less than the
// contract: it would pass for a Normalize that was right once and wrong on the
// second apply, which is exactly what a hot reload performs. So the document
// side is normalized twice, and the row side once, and all three have to
// agree.
//
// # Why it lives in config rather than in org or chart
//
// It needs all three packages and a parsed document. `config` is the only one
// that can import the other two, which is the same direction the production
// code takes: the chart cannot see config, and org cannot see config.

// TestTheRowDerivationEqualsTheDocumentDerivation is the gate.
func TestTheRowDerivationEqualsTheDocumentDerivation(t *testing.T) {
	t.Parallel()

	for name, path := range equivalenceCorpus(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			company := loadCompany(t, path)

			// THE DOCUMENT SIDE, normalized TWICE. See the header: one
			// pass certifies less than the contract.
			fromDocument, err := company.Organization()
			if err != nil {
				t.Fatalf("derive from the document: %v", err)
			}
			fromDocument.Normalize()

			// THE ROW SIDE: the same company, imported and derived.
			view := org.FromRows(authoredFrom(company).Rows(), settingsOf(company))

			want := shapeOf(t, fromDocument)
			got := shapeOf(t, view.Org)
			if want != got {
				t.Errorf("the two derivations disagree.\n"+
					"  from the document: %s\n"+
					"  from the rows:     %s\n"+
					"A company that migrates would get a different chart — a "+
					"different lead, a different roster, a different manager — "+
					"and nothing would say so, because both answers are "+
					"internally consistent.", want, got)
			}
			if len(view.Reparented) != 0 {
				t.Errorf("the row derivation had to reparent %v to break a "+
					"cycle, and this company has none — so either the "+
					"fixture is cyclic or the cycle check is wrong",
					view.Reparented)
			}
		})
	}
}

// AND THE CONTROL: perturbing ONE derivation makes it red.
//
// Without this the case above passes for two derivations that are both wrong
// in the same way, or for a comparison that compares nothing. It perturbs the
// ROW side — the one under development — in the smallest way that is still a
// real difference: one unit's inherited channel.
func TestTheEquivalenceGateCatchesAPerturbedDerivation(t *testing.T) {
	t.Parallel()
	company := loadCompany(t, exampleNimbus)

	fromDocument, err := company.Organization()
	if err != nil {
		t.Fatalf("derive from the document: %v", err)
	}
	view := org.FromRows(authoredFrom(company).Rows(), settingsOf(company))

	// THE PERTURBATION, applied to the built view rather than to the
	// builder: what this case has to establish is that shapeOf SEES a
	// difference, and a builder flag would test the flag.
	perturbed := view.Org.Units
	if len(perturbed) == 0 {
		t.Fatal("the example declares no units, so there is nothing to perturb")
	}
	perturbed[0].Channel += "-perturbed"

	if shapeOf(t, fromDocument) == shapeOf(t, view.Org) {
		t.Fatal("one unit's channel was changed and the comparison still " +
			"reports the two derivations equal — so the gate above is " +
			"comparing nothing and would pass whatever the builder did")
	}
}

// A CYCLE IN THE ROWS TERMINATES AND IS REPORTED.
//
// The write path refuses one — the batch validator replays every move and
// walks up from the new parent — but a view is built from ROWS, and rows can
// predate that rule or be repaired by hand. A build that recursed into one
// would not produce a wrong answer, it would not terminate.
func TestACycleInTheRowsIsBrokenDeterministicallyAndReported(t *testing.T) {
	t.Parallel()

	rows := chart.Authored{Units: []chart.AuthoredUnit{
		{Key: "alpha", Parent: "beta", Name: "Alpha"},
		{Key: "beta", Parent: "alpha", Name: "Beta"},
		{Key: "gamma", Name: "Gamma"},
	}}.Rows()

	first := org.FromRows(rows, org.Settings{Name: "Acme"})
	if len(first.Reparented) == 0 {
		t.Fatal("a cyclic chart produced no report, so an operator has no " +
			"way to know which team moved")
	}

	// TWO BUILDERS PRODUCE THE SAME VIEW, which is what makes this safe at
	// all: every node builds its own from the same rows, and "whichever we
	// reached first" would make the tree a function of the order the
	// planner returned — two nodes holding different companies while both
	// reporting they were caught up.
	second := org.FromRows(rows, org.Settings{Name: "Acme"})
	if !slices.Equal(first.Reparented, second.Reparented) {
		t.Errorf("two builders reparented %v and %v", first.Reparented,
			second.Reparented)
	}
	if shapeOf(t, first.Org) != shapeOf(t, second.Org) {
		t.Error("two builders produced different trees from one set of rows")
	}
	// AND THE UNIT THAT WAS NOT IN THE CYCLE IS UNTOUCHED.
	if slices.Contains(first.Reparented, "gamma") {
		t.Errorf("an acyclic unit was reparented: %v", first.Reparented)
	}
}

// --- the corpus and the comparison -------------------------------------------- //

// exampleNimbus is the worked two-tier company this repository ships.
const exampleNimbus = "../../examples/nimbus.company.yaml"

// equivalenceCorpus is every company file the gate runs over.
//
// THE EXAMPLES AND THE FIXTURES, because each exists for a different reason:
// the examples are what a founder copies, and the fixtures are the shapes
// somebody already found a bug in — a unit reference that shields, a root seat
// that manages a unit, a lead inherited three levels down.
func equivalenceCorpus(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, pattern := range []string{
		"../../examples/*.company.yaml",
		"testdata/derived/*.company.yaml",
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		for _, path := range matches {
			out[filepath.Base(path)] = path
		}
	}
	if len(out) == 0 {
		t.Fatal("the corpus is empty, so this gate certifies nothing")
	}
	return out
}

func loadCompany(t *testing.T, path string) *config.Company {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	company, err := config.ParseCompany(body)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return company
}

// settingsOf is the company values that are not the chart.
func settingsOf(c *config.Company) org.Settings {
	return org.Settings{
		Name: c.Name, Mission: c.Mission, Vision: c.Vision,
		Policies: slices.Clone(c.Policies), TokenBudget: c.TokenBudget,
		KnowledgeScope: slices.Clone(c.Knowledge.KnowledgeScope),
	}
}

// authoredFrom flattens a parsed company into the chart's authored shape.
//
// THIS IS WHAT AN IMPORT DOES, in test form: it walks the document's nesting
// and states each object's parent, because rows are flat.
func authoredFrom(c *config.Company) chart.Authored {
	var out chart.Authored
	for i := range c.Roles {
		out.Seats = append(out.Seats, authoredSeat(&c.Roles[i], c.Roles[i].Unit))
	}
	var walk func(units []config.Unit, parent string)
	walk = func(units []config.Unit, parent string) {
		for i := range units {
			unit := &units[i]
			key := unit.IdentityKey()
			out.Units = append(out.Units, chart.AuthoredUnit{
				Key: key, Parent: parent,
				Name: unit.Name, Type: string(unit.Type),
				Purpose: unit.Purpose, Goals: slices.Clone(unit.Goals),
				Lead: unit.Lead, Channel: unit.Channel,
				Project: unit.Project, Space: unit.Space,
				KnowledgeRefs: slices.Clone(unit.Knowledge),
			})
			for j := range unit.Roles {
				out.Seats = append(out.Seats,
					authoredSeat(&unit.Roles[j], key))
			}
			walk(unit.Children, key)
		}
	}
	walk(c.Units, "")
	return out
}

func authoredSeat(role *config.Role, unit string) chart.AuthoredSeat {
	seat := role.Seat()
	return chart.AuthoredSeat{
		Handle: seat.Handle(), Unit: unit,
		Kind: chart.SeatKind(seat.Kind), Name: role.Name,
		Email: role.Email, Backstory: role.Backstory, Goal: role.Goal,
		Responsibilities:     slices.Clone(role.Responsibilities),
		BehavioralGuidelines: slices.Clone(role.BehavioralGuidelines),
		Manages:              slices.Clone(role.Manages),
		Project:              role.Project, Space: role.Space,
	}
}

// shapeOf renders the derivations this gate compares, as one string.
//
// # Why a rendering rather than reflect.DeepEqual
//
// The two sides are built by different code and legitimately differ in fields
// no derivation touches — a unit's authored `DeclaredLead` bookkeeping, the
// order two seats landed in a slice. A deep compare would fail on all of it
// and say nothing about which derivation is wrong.
//
// So this renders exactly what a turn READS: where each object sits, what its
// effective lead and channel resolved to, and which handles each `manages`
// list expanded to. A difference in any of those is a company that behaves
// differently; a difference in anything else is not.
func shapeOf(t *testing.T, o *org.Organization) string {
	t.Helper()
	type seatShape struct {
		Handle  string   `json:"handle"`
		Kind    string   `json:"kind"`
		Unit    string   `json:"unit"`
		Manages []string `json:"manages,omitempty"`
	}
	type unitShape struct {
		Key     string `json:"key"`
		Parent  string `json:"parent"`
		Type    string `json:"type"`
		Lead    string `json:"lead"`
		Channel string `json:"channel"`
	}
	var units []unitShape
	var seats []seatShape

	var walk func(list []*org.Unit, parent string)
	walk = func(list []*org.Unit, parent string) {
		for _, u := range list {
			units = append(units, unitShape{
				Key: u.Key(), Parent: parent, Type: string(u.Type),
				Lead: u.Lead, Channel: u.Channel,
			})
			for _, r := range u.Roles {
				seats = append(seats, seatShape{
					Handle: r.Handle(), Kind: string(r.Kind),
					Unit: u.Key(), Manages: sortedCopy(r.Manages),
				})
			}
			walk(u.Children, u.Key())
		}
	}
	walk(o.Units, "")
	for _, r := range o.Roles {
		seats = append(seats, seatShape{
			Handle: r.Handle(), Kind: string(r.Kind),
			Manages: sortedCopy(r.Manages),
		})
	}
	// SORTED, because the two sides walk the tree in different orders and
	// this gate is about WHAT each object resolved to rather than about
	// which slice it landed in.
	slices.SortFunc(units, func(a, b unitShape) int {
		return strings.Compare(a.Key, b.Key)
	})
	slices.SortFunc(seats, func(a, b seatShape) int {
		return strings.Compare(a.Handle, b.Handle)
	})

	rendered, err := json.Marshal(struct {
		Units []unitShape `json:"units"`
		Seats []seatShape `json:"seats"`
	}{units, seats})
	if err != nil {
		t.Fatalf("render the shape: %v", err)
	}
	return string(rendered)
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}
