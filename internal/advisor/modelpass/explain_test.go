package modelpass

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolsession"
)

func newHost(t *testing.T) *toolsession.Host {
	t.Helper()
	host, err := toolsession.NewHost()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host
}

func explanations(t *testing.T, pairs ...string) string {
	t.Helper()
	var list []map[string]string
	for i := 0; i+1 < len(pairs); i += 2 {
		list = append(list, map[string]string{"id": pairs[i], "text": pairs[i+1]})
	}
	raw, err := json.Marshal(map[string]any{"explanations": list})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// THE PASS EXPLAINS A FINDING ONLY IN ITS OWN NUMBERS: it is handed the open
// findings no explanation was written for, and keeps the explanation of each
// that says only that finding's numbers and records, bound to the verdict it
// read; one saying another number, or naming a finding it was not asked
// about, is dropped and counted. Mutation: keep every submitted explanation,
// and the drops are not counted.
func TestThePassExplainsAFindingOnlyInItsOwnNumbers(t *testing.T) {
	t.Parallel()
	d := planted(t)
	d.Unexplained = []advisor.Explainable{
		{ID: "f-1", Key: "run.round_cap:seat:dev", VerdictDigest: "v1", Item: "2 runs at a cap of 30 rounds",
			Facts: []advisor.Fact{{Key: "runs", Value: 2}, {Key: "cap", Value: 30}}},
		{ID: "f-2", Key: "budget.stopped:seat:qa", VerdictDigest: "v2", Facts: []advisor.Fact{{Key: "hours", Value: 6}}},
	}
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "reading", Calls: []llmtest.Call{{Name: "findings_to_explain", Args: `{}`}}},
		{Text: "explaining", Calls: []llmtest.Call{{Name: ExplainTool, Args: explanations(t,
			"f-1", "Dev's runs hit the cap of 30 rounds twice, so the work started over each time.",
			"f-2", "QA waited 9 hours on its budget.",
			"f-9", "This finding was never asked about.")}}},
		{Text: "submitting", Calls: []llmtest.Call{{Name: SubmitTool, Args: submission(t)}}},
	}}}}
	got := Run(t.Context(), Deps{Provider: prov, ProviderKey: "p", Host: newHost(t),
		Now: func() time.Time { return monday }}, d, Ceiling)
	if len(got.Explained) != 1 || got.Explained[0].ID != "f-1" || got.Explained[0].VerdictDigest != "v1" ||
		got.Explained[0].Key != "run.round_cap:seat:dev" {
		t.Fatalf("explained %+v, want f-1's alone, bound to its verdict", got.Explained)
	}
	if got.Dropped[advisor.DropExplanation] != 2 {
		t.Errorf("dropped %v, want the two others counted", got.Dropped)
	}
}

// A REPORT'S SUMMARY IS KEPT ONLY WHERE ITS NUMBERS ARE THE REPORT'S. Mutation:
// return the submitted words unchecked, and the second summary is kept.
func TestAReportsSummaryIsKeptOnlyWhereItsNumbersAreTheReports(t *testing.T) {
	t.Parallel()
	was := int64(100)
	r := advisor.Report{Week: "2030-W02", Metrics: []advisor.Metric{{Key: "turns", Label: "Turns", Value: 120,
		Unit: advisor.UnitTurns, Was: &was}}, Tally: advisor.Tally{Opened: 4, Resolved: 2}}
	summarise := func(text string) (string, error) {
		raw, err := json.Marshal(map[string]string{"summary": text})
		if err != nil {
			t.Fatal(err)
		}
		prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
			{Text: "summarising", Calls: []llmtest.Call{{Name: SummaryTool, Args: string(raw)}}}}}}}
		return Summary(t.Context(), Deps{Provider: prov, ProviderKey: "p", Host: newHost(t),
			Now: func() time.Time { return monday }}, r, SummaryCeiling)
	}
	kept := "Turns rose from 100 to 120. The week opened 4 findings and resolved 2."
	if got, err := summarise(kept); err != nil || got != kept {
		t.Errorf("a grounded summary: %q (%v)", got, err)
	}
	if got, err := summarise("Turns rose by 20%."); !errors.Is(err, ErrNoSummary) || got != "" {
		t.Errorf("a summary saying a number the report never did: %q (%v), want none kept", got, err)
	}
}
