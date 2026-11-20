package modelpass

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// A DEEP RUN REWRITES WHAT THE WORDING RULE REFUSED, IN THE SAME RUN: its first
// submission is sent back saying which finding breaks the rule and how, the
// numbers standing as cited; the model rewrites it and submits again, and the
// finding is listed as reworded, not dropped. Mutation: take the first
// submission as it is, and the finding is dropped or said by the engine.
func TestADeepRunRewritesWhatTheWordingRuleRefused(t *testing.T) {
	t.Parallel()
	d := planted(t)
	d.Rules = nil
	hits := map[string]any{"key": "hits", "label": "Times it hit its limit", "value": 4,
		"from": "read_1.entries[0].hits", "unit": "count"}
	finding := func(title string) string {
		f := deepFinding("p hit its usage limit 4 times.", hits)
		f["title"] = title
		raw, err := json.Marshal(map[string]any{"findings": []any{f}})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "reading", Calls: []llmtest.Call{{Name: ReadTool, Args: `{"name":"advisor_capacity","params":{}}`}}},
		{Text: "submitting", Calls: []llmtest.Call{{Name: SubmitTool,
			Args: finding("The limit, see https://status.example.com for it")}}},
		{Text: "rewriting", Calls: []llmtest.Call{{Name: SubmitTool, Args: finding("A model entry's limit kept its seats out.")}}},
	}}}}
	got := RunDeep(t.Context(), Deps{Provider: prov, ProviderKey: "p", Host: host(t),
		Now: func() time.Time { return monday }}, DeepData{Data: d, Reads: &fakeReads{}}, DefaultDeepBounds, nil)
	if got.Status != advisor.PassDone || got.Listed != 1 || len(got.Dropped) != 0 ||
		got.Repaired[advisor.RepairReworded] != 1 || got.Repaired[advisor.RepairEngineWords] != 0 {
		t.Fatalf("run = %+v, want the finding listed as reworded", got)
	}
	if got.Findings[0].Title != "A model entry's limit kept its seats out." {
		t.Errorf("title = %q, want the model's rewrite", got.Findings[0].Title)
	}
	at := slices.IndexFunc(prov.Results(), func(r llmtest.Result) bool { return r.Name == SubmitTool })
	if r := prov.Results()[at]; !r.Failed || !strings.Contains(r.Output, "Finding 1") ||
		!strings.Contains(r.Output, "title holds a link") {
		t.Errorf("the first submission was answered %+v, want it sent back saying why", r)
	}
}
