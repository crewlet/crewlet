package advisor

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// repairRig is a company of one agent seat and a read the findings cite.
func repairRig(t *testing.T) (*Served, Grounding) {
	t.Helper()
	c, err := config.ParseCompanyDocument([]byte("name: Acme\nproviders:\n  llm:\n    p: {model: m}\n" +
		"roles:\n  - name: App Engineer\n    handle: app-engineer\n    llm: p\n"))
	if err != nil {
		t.Fatal(err)
	}
	served := NewServed()
	if err := served.ServeRead("read_1", ReadSource{Read: "work_items"},
		map[string]any{"total": 27, "tokens": 900_000, "turns": 137}, nil); err != nil {
		t.Fatal(err)
	}
	return served, Grounding{Company: c, DryRun: func(Change) error { return errors.New("refused") },
		RecordsOptional: true, Units: true, Headlines: true}
}

func proposal(title string, fix SubmittedFix, facts ...CitedFact) Submitted {
	return Submitted{Title: title, Headline: "App Engineer holds 27 open items.", Why: "It waits.",
		Severity: SeverityCosts, Subject: Subject{Kind: SubjectSeat, Seat: "app-engineer"}, Facts: facts, Fix: fix}
}

var held = CitedFact{Key: "open", Label: "Open items", Value: 27, From: "read_1.total", Unit: UnitItems}

// A PROPOSAL WHOSE FIX IS NOT BUILT STILL STANDS: listed with no change to
// send and a plain line saying what a person would do, counted as listed
// without its fix. Mutation: drop it for its fix, and what the model found
// never reaches the page.
func TestAProposalWhoseFixIsNotBuiltStillStands(t *testing.T) {
	t.Parallel()
	served, g := repairRig(t)
	got := Ground([]Submitted{
		proposal("Its runs end on their cap.", SubmittedFix{Builder: BuildRoundCap, Seat: "app-engineer", Value: 60}, held),
		proposal("An item no read named.", SubmittedFix{Builder: BuildOpenItem, Target: "ENG-9"},
			CitedFact{Key: "turns", Label: "Turns", Value: 137, From: "read_1.turns", Unit: UnitTurns}),
	}, served, g)
	if len(got.Findings) != 2 || len(got.Dropped) != 0 || got.Repaired[RepairNoFix] != 2 {
		t.Fatalf("grounded = %+v, want both listed without their fixes", got)
	}
	words := map[string]string{}
	for _, f := range got.Findings {
		if f.Fix.Kind != FixNone || len(f.Fix.Options) != 0 {
			t.Errorf("%q offers %+v, want no change to send", f.Title, f.Fix)
		}
		words[f.Title] = f.Fix.Sentence
	}
	if !strings.Contains(words["Its runs end on their cap."], "check App Engineer's round cap") ||
		words["An item no read named."] != "Open ENG-9 and decide what it needs." {
		t.Errorf("fix words = %q, want what a person would do", words)
	}
}

// WORDS THE RULE REFUSES ARE SAID BY THE ENGINE FROM THE FINDING'S FACTS: a
// title with a link, markup and a line break, a why past its bound, a label
// missing, a unit not said, a headline saying a number no fact cites, a
// severity it does not know and a seat the chart does not hold are each
// mended, and the finding is listed; a number not where it says is still
// dropped, and counted. Mutation: drop for the words again, and a finding
// with the right numbers is lost for its phrasing.
func TestWordsTheRuleRefusesAreSaidByTheEngine(t *testing.T) {
	t.Parallel()
	served, g := repairRig(t)
	g.DryRun = func(Change) error { return nil }
	worded := Submitted{Title: "See <b>https://example.com/x</b>\nfor [the] details", Headline: "It holds 99 items.",
		Why: strings.Repeat("waits ", 150), Severity: "high", Subject: Subject{Kind: SubjectSeat, Seat: "gone"},
		Facts: []CitedFact{{Key: "open_items", Value: 27, From: "read_1.total"},
			{Key: "turns", Label: "Turns `run`", Value: 137, From: "read_1.turns", Unit: UnitTurns}},
		Fix: SubmittedFix{Builder: BuildNone}}
	wrong := proposal("A number not read.", SubmittedFix{Builder: BuildNone},
		CitedFact{Key: "open", Label: "Open", Value: 28, From: "read_1.total", Unit: UnitItems})
	got := Ground([]Submitted{worded, wrong}, served, g)
	if len(got.Findings) != 1 || got.Dropped[DropWrongFact] != 1 || got.Repaired[RepairEngineWords] != 1 ||
		got.Repaired[RepairSubject] != 1 {
		t.Fatalf("grounded = %+v, want the worded one listed and the wrong number dropped", got)
	}
	f := got.Findings[0]
	for field, text := range map[string]string{"title": f.Title, "why": f.Why, "headline": f.Headline} {
		if !plainWords(text, MaxAnalystWhy) {
			t.Errorf("%s = %q still breaks the rule", field, text)
		}
	}
	if f.Title != "See for the details" || len([]rune(f.Why)) > MaxAnalystWhy || !strings.HasSuffix(f.Why, "…") {
		t.Errorf("title %q, why %d runes: want the words kept and cut to their bound", f.Title, len([]rune(f.Why)))
	}
	if f.Headline != "Open items: 27." || f.Severity != SeverityNote || f.Subject.Kind != SubjectCompany {
		t.Errorf("headline %q, severity %s, subject %+v: want said from its first fact, a note, the company",
			f.Headline, f.Severity, f.Subject)
	}
	if f.Evidence[0].Label != "Open items" || f.Evidence[0].Unit != UnitItems || f.Evidence[1].Label != "Turns run" {
		t.Errorf("facts = %+v, want a label from its key, a unit from its path, and plain words", f.Evidence)
	}
}

// THE RULE'S REASONS ARE SAID SO A MODEL CAN REWRITE: each field that breaks
// it, and how. Mutation: say only that the words were refused, and the model
// cannot tell what to change.
func TestTheRulesReasonsAreSaidSoAModelCanRewrite(t *testing.T) {
	t.Parallel()
	_, g := repairRig(t)
	sub := proposal("See https://example.com", SubmittedFix{}, CitedFact{Key: "open", Value: 27, From: "read_1.total"})
	sub.Headline, sub.Why = "It holds 99 items.", "One line\nand another"
	got := Precheck(sub, g)
	for _, want := range []string{"title holds a link", "why holds a line break or a control character",
		"headline says 99, which no fact cites", "the fact open has no label", "the fact open says no unit"} {
		if !slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, want) }) {
			t.Errorf("reasons %q, want one saying %q", got, want)
		}
	}
	if clean := Precheck(proposal("Plain words.", SubmittedFix{}, held), g); len(clean) != 0 {
		t.Errorf("plain words gave reasons %q", clean)
	}
}
