package advisor

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A COST FIX STANDS ONLY WHERE FRESH TOKENS DROVE THEM: a finding about a seat's tokens that offers
// a budget keeps it where the split it read shows fresh input or output drove the tokens (or their
// rise, against the range before), and offers none, saying why, where cache reads drove them or no
// split was read. Mutation: drop the gate, and a jump that was the cache is met with a budget that
// stops the seat's work.
func TestACostFixStandsOnlyWhereFreshTokensDroveThem(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompanyDocument([]byte("name: Acme\nproviders:\n  llm:\n    p: {model: m}\n" +
		"roles:\n  - name: App Engineer\n    handle: app-engineer\n    llm: p\n" +
		"  - name: QA\n    handle: qa\n    llm: p\n    token_budget: {day: 1000000}\n"))
	if err != nil {
		t.Fatal(err)
	}
	served := NewServed()
	answer := map[string]any{
		"now":    map[string]any{"input_tokens": 1_384_000_000, "cache_read_tokens": 1_350_000_000, "output_tokens": 600_000},
		"fresh":  map[string]any{"input_tokens": 300_000_000, "cache_read_tokens": 120_000_000, "output_tokens": 9_000_000},
		"before": map[string]any{"input_tokens": 100_000_000, "cache_read_tokens": 80_000_000, "output_tokens": 3_000_000},
		"turns":  137,
	}
	if err := served.ServeRead("read_1", ReadSource{Read: "tokens"}, answer, nil); err != nil {
		t.Fatal(err)
	}
	fact := func(key, from string, value int64, was string) CitedFact {
		return CitedFact{Key: key, Label: "Tokens " + key, Value: value, From: from, Unit: UnitTokens, WasFrom: was}
	}
	budget := SubmittedFix{Builder: BuildTokenBudget, Seat: "app-engineer", Period: "day", Value: 400_000_000}
	submitted := func(title string, fix SubmittedFix, facts ...CitedFact) Submitted {
		return Submitted{Title: title, Why: "It spent more.", Severity: SeverityCosts,
			Subject: Subject{Kind: SubjectSeat, Seat: "app-engineer"}, Facts: facts, Fix: fix}
	}
	g := Grounding{Company: c, DryRun: func(Change) error { return nil }, RecordsOptional: true}
	findings, dropped := Ground([]Submitted{
		submitted("The cache drove it.", budget, fact("input", "read_1.now.input_tokens", 1_384_000_000, ""),
			fact("cache", "read_1.now.cache_read_tokens", 1_350_000_000, ""), fact("output", "read_1.now.output_tokens", 600_000, "")),
		submitted("Fresh input drove the rise.", budget,
			fact("input", "read_1.fresh.input_tokens", 300_000_000, "read_1.before.input_tokens"),
			fact("cache", "read_1.fresh.cache_read_tokens", 120_000_000, "read_1.before.cache_read_tokens"),
			fact("output", "read_1.fresh.output_tokens", 9_000_000, "read_1.before.output_tokens")),
		submitted("No split was read.", budget, fact("input", "read_1.now.input_tokens", 1_384_000_000, "")),
		submitted("Its runs end on their cap.", SubmittedFix{Builder: BuildRoundCap, Seat: "app-engineer", Value: 60},
			CitedFact{Key: "turns", Label: "Turns", Value: 137, From: "read_1.turns", Unit: UnitTurns}),
		submitted("Its budget is below one run.", SubmittedFix{Builder: BuildTokenBudget, Seat: "qa", Period: "day", Value: 2_000_000},
			fact("input", "read_1.now.output_tokens", 600_000, "")),
	}, served, g)
	if len(dropped) != 0 || len(findings) != 5 {
		t.Fatalf("findings %+v, dropped %v: want all five listed", findings, dropped)
	}
	by := map[string]Fix{}
	for _, f := range findings {
		by[f.Title] = f.Fix
	}
	if f := by["The cache drove it."]; f.Kind != FixNone || !strings.Contains(f.Sentence, "cache reads drove these tokens (1.4B of 1.4B input)") {
		t.Errorf("cache-driven fix = %+v, want none, saying the cache drove them", f)
	}
	if f := by["Fresh input drove the rise."]; f.Kind != FixConfigChange || len(f.Options) != 1 {
		t.Errorf("fresh-driven fix = %+v, want the budget", f)
	}
	if f := by["No split was read."]; f.Kind != FixNone || !strings.Contains(f.Sentence, "not split") {
		t.Errorf("unsplit fix = %+v, want none, saying no split was read", f)
	}
	if f := by["Its runs end on their cap."]; f.Kind != FixConfigChange {
		t.Errorf("a cap on a finding about rounds = %+v, want it kept", f)
	}
	if f := by["Its budget is below one run."]; f.Kind != FixConfigChange {
		t.Errorf("a raise of a budget the work outgrew = %+v, want it kept", f)
	}
}

// A MODEL'S NUMBERS READ AS THE ENGINE'S: a long bare number is grouped by thousands, or said as a
// token count where the finding states it as one; a key, an id, a decimal, a year or a number
// already grouped is left as written. Mutation: leave the digits bare, and a reader counts them.
func TestAModelsNumbersReadAsTheEngines(t *testing.T) {
	t.Parallel()
	f := SayNumbers(Finding{Headline: "Tokens rose from 698252 to 2798109.",
		Why:      "GP-12345 and run abba8d12 in 2026 used 1.5 times the 1,384 turns; 1384667981 tokens, 70000 items.",
		Evidence: []Fact{{Key: "tokens", Label: "Tokens", Value: 2_798_109, Unit: UnitTokens}, {Key: "t", Value: 1_384_667_981, Label: "Input tokens"}}})
	if f.Headline != "Tokens rose from 698,252 to 2.8M." {
		t.Errorf("headline = %q", f.Headline)
	}
	if f.Why != "GP-12345 and run abba8d12 in 2026 used 1.5 times the 1,384 turns; 1.4B tokens, 70,000 items." {
		t.Errorf("why = %q", f.Why)
	}
}
