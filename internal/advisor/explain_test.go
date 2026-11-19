package advisor_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
)

// AN EXPLANATION SAYS ONLY ITS FINDING'S OWN NUMBERS AND RECORDS, in a few
// plain sentences: a number its facts and engine words do not say, a record
// it does not cite, a link, markup or a fourth sentence is not kept.
// Mutation: skip the number check, and the cap of 60 is kept.
func TestAnExplanationSaysOnlyItsFindingsOwnNumbersAndRecords(t *testing.T) {
	t.Parallel()
	e := advisor.Explainable{ID: "f-1", Title: "Agent runs ended on their round cap before they finished.",
		Item:    "2 runs at a cap of 30 rounds, 26.5M tokens",
		Facts:   []advisor.Fact{{Key: "runs", Value: 2}, {Key: "tokens", Value: 26_500_000}, {Key: "cap", Value: 30}},
		Records: []advisor.Record{{Kind: advisor.RecordTurn, ID: "turn-9"}, {Kind: advisor.RecordItem, ID: "ENG-42"}}}
	for _, kept := range []string{
		"Dev's runs stopped at the cap of 30 rounds twice, spending 26.5M tokens with nothing finished. Start with turn-9.",
		"The 2 runs on ENG-42 spent 26,500,000 tokens between them.",
	} {
		if err := advisor.GroundExplanation(kept, e); err != nil {
			t.Errorf("%q was not kept: %v", kept, err)
		}
	}
	for name, text := range map[string]string{
		"a number it does not rest on": "Raising the cap to 60 would let the work finish.",
		"a record it does not cite":    "Look at ENG-7 first.",
		"a link":                       "See https://example.com for the runs.",
		"markup":                       "The **cap** is what stops them.",
		"four sentences":               "It stops. It starts again. It stops again. Nothing finishes.",
		"nothing":                      "  ",
	} {
		if err := advisor.GroundExplanation(text, e); !errors.Is(err, advisor.ErrUngrounded) {
			t.Errorf("%s: %q was kept (%v)", name, text, err)
		}
	}
}

// ONLY A VERDICT NO EXPLANATION WAS WRITTEN FOR IS EXPLAINED: an open finding
// whose explanation is of its current verdict, an applied one and a closed one
// are not asked again, and the most severe come first. Mutation: compare
// nothing, and the explained finding is asked again.
func TestOnlyAVerdictNoExplanationWasWrittenForIsExplained(t *testing.T) {
	t.Parallel()
	stored := func(id string, severity advisor.Severity, state advisor.State, verdict, explained string) advisor.Stored {
		f := capFix("dev", 3, 60)
		f.Severity = severity
		return advisor.Stored{ID: id, State: state, VerdictDigest: verdict, ExplainedDigest: explained,
			Observation: observed(f)}
	}
	got := advisor.Unexplained([]advisor.Stored{
		stored("note", advisor.SeverityNote, advisor.StateOpen, "v1", ""),
		stored("explained", advisor.SeverityCosts, advisor.StateOpen, "v2", "v2"),
		stored("moved", advisor.SeverityCosts, advisor.StateOpen, "v3", "v1"),
		stored("applied", advisor.SeverityBlocks, advisor.StateApplied, "v4", ""),
	})
	if len(got) != 2 || got[0].ID != "moved" || got[1].ID != "note" || got[0].VerdictDigest != "v3" {
		t.Errorf("asked to explain %+v, want the moved verdict, then the note", got)
	}
}

// A REPORT'S SUMMARY SAYS ONLY THE REPORT'S NUMBERS. Mutation: allow any
// number, and the 20% the report never said is kept.
func TestAReportsSummarySaysOnlyTheReportsNumbers(t *testing.T) {
	t.Parallel()
	was := int64(100)
	r := advisor.Report{Metrics: []advisor.Metric{{Key: "turns", Value: 120, Was: &was}},
		Tally: advisor.Tally{Opened: 4, Resolved: 2, Applied: 1},
		Top:   []advisor.ReportFinding{{ID: "f-1", Title: "Agent runs ended on their round cap.", Item: "2 runs"}}}
	if err := advisor.GroundSummary("Turns rose from 100 to 120. The week opened 4 findings and resolved 2; "+
		"1 fix was applied. Start with the round cap, 2 runs.", r); err != nil {
		t.Errorf("a grounded summary was not kept: %v", err)
	}
	if err := advisor.GroundSummary("Turns rose by 20% on the week.", r); !errors.Is(err, advisor.ErrUngrounded) {
		t.Errorf("a number the report never said was kept (%v)", err)
	}
}
