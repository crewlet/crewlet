package modelpass

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// fakeReads answers the capacity read alone, and keeps what it was asked.
type fakeReads struct {
	mu    sync.Mutex
	asked []string
}

func (*fakeReads) Questions() []Question {
	return []Question{{Name: "advisor_capacity", Params: "{range}", About: "hours each model entry was out"}}
}

func (r *fakeReads) Answer(_ context.Context, name string, _ map[string]any) (any, error) {
	r.mu.Lock()
	r.asked = append(r.asked, name)
	r.mu.Unlock()
	if name != "advisor_capacity" {
		return nil, errors.New("not on the allowlist")
	}
	return map[string]any{"entries": []any{map[string]any{"entry": "p", "hits": 4, "total_hours": 11,
		"seat_count": 2}}}, nil
}

func deepFinding(headline string, facts ...map[string]any) map[string]any {
	return map[string]any{"headline": headline, "title": "A model entry's limit kept its seats out: " + headline,
		"why": "Every seat on it waits while the limit stands.", "severity": "costs",
		"subject": map[string]any{"kind": "company"}, "records": []map[string]any{}, "facts": facts,
		"fix": map[string]any{"builder": "none"}}
}

func host(t *testing.T) *toolsession.Host {
	t.Helper()
	h, err := toolsession.NewHost()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// A DEEP RUN INVESTIGATES AND LISTS ONLY WHAT ITS READS SHOW: it reads the
// snapshot and the allowlisted reads, says what it is doing as it goes, is
// refused a read off the allowlist, and of what it submits keeps only the
// findings whose every cited number is in an answer it read and whose
// headline says no other number. Mutation: let a headline say a number no
// fact cites, or a read outside the allowlist through, and a check fails.
func TestADeepRunInvestigatesAndListsOnlyWhatItsReadsShow(t *testing.T) {
	t.Parallel()
	d := planted(t)
	reads := &fakeReads{}
	hits := map[string]any{"key": "hits", "label": "Times it hit its limit", "value": 4, "from": "read_1.entries[0].hits"}
	hours := map[string]any{"key": "hours", "label": "Hours out", "value": 11, "from": "read_1.entries[0].total_hours"}
	seats := map[string]any{"key": "seats", "label": "Seats held", "value": 2, "from": "read_1.entries[0].seat_count"}
	raw, err := json.Marshal(map[string]any{"findings": []any{
		deepFinding("2 seats sat out 11 h on 19 Nov: p hit its usage limit 4 times.", hits, hours, seats),
		deepFinding("2 seats sat out 99 h: p hit its usage limit 4 times.", hits, seats),
		deepFinding("p hit its usage limit 5 times.", map[string]any{"key": "hits", "label": "Times",
			"value": 5, "from": "read_1.entries[0].hits"}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "starting", Calls: []llmtest.Call{{Name: SnapshotTool, Args: `{}`}, {Name: ListReadsTool, Args: `{}`},
			{Name: ProgressTool, Args: `{"text":"reading capacity"}`}}},
		{Text: "reading", Calls: []llmtest.Call{
			{Name: ReadTool, Args: `{"name":"advisor_capacity","params":{"range":"7d"}}`},
			{Name: ReadTool, Args: `{"name":"iam_credentials"}`}}},
		{Text: "submitting", Calls: []llmtest.Call{{Name: SubmitTool, Args: string(raw)}}},
	}}}}
	var told []Progress
	got := RunDeep(t.Context(), Deps{Provider: prov, ProviderKey: "p", Host: host(t),
		Now: func() time.Time { return monday }}, DeepData{Data: d, Reads: reads}, DefaultDeepBounds,
		func(p Progress) { told = append(told, p) })
	if got.Status != advisor.PassDone || got.StoppedBy != StoppedDone || len(got.Findings) != 1 {
		t.Fatalf("run = %+v, want one finding, done", got)
	}
	if f := got.Findings[0]; f.Headline != "2 seats sat out 11 h on 19 Nov: p hit its usage limit 4 times." ||
		len(f.Evidence) != 3 {
		t.Errorf("finding = %+v", f)
	}
	if got.Dropped[advisor.DropWords] != 1 || got.Dropped[advisor.DropWrongFact] != 1 {
		t.Errorf("dropped %v, want the 99-hour headline and the wrong count", got.Dropped)
	}
	if len(told) != 1 || told[0].Text != "reading capacity" || len(got.Progress) != 1 {
		t.Errorf("progress told %+v, kept %+v", told, got.Progress)
	}
	if slices.Contains(reads.asked, "iam_credentials") {
		t.Errorf("a read off the allowlist was asked: %v", reads.asked)
	}
}

// A DEEP RUN NEAR ITS TOKEN BOUND IS TOLD TO SUBMIT: its reads answer that
// time is up, it submits what it has, and the run says which bound it
// stopped at. Mutation: let reads go on past the bound, and the run is cut
// off with nothing listed.
func TestADeepRunNearItsBoundIsToldToSubmit(t *testing.T) {
	t.Parallel()
	d := planted(t)
	empty, err := json.Marshal(map[string]any{"findings": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "reading", Calls: []llmtest.Call{{Name: SnapshotTool, Args: `{}`}},
			Usage: llm.Usage{Input: 9_000, Output: 100}},
		{Text: "reading", Calls: []llmtest.Call{{Name: ListReadsTool, Args: `{}`}}},
		{Text: "submitting", Calls: []llmtest.Call{{Name: SubmitTool, Args: string(empty)}}},
	}}}}
	got := RunDeep(t.Context(), Deps{Provider: prov, ProviderKey: "p", Host: host(t),
		Now: func() time.Time { return monday }}, DeepData{Data: d, Reads: &fakeReads{}},
		DeepBounds{Tokens: 10_000, Minutes: 20, Rounds: 80}, nil)
	if got.StoppedBy != StoppedTokens || got.Status != advisor.PassCeiling || got.Reason == "" {
		t.Errorf("run = %+v, want stopped by its tokens, saying so", got)
	}
}
