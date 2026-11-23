package modelpass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// agendaReads answers a work list far past what one round should carry, a
// seat's token split, and fails the capacity read; it keeps what it was asked.
type agendaReads struct {
	mu    sync.Mutex
	asked []string
}

func (*agendaReads) Questions() []Question {
	return []Question{{Name: "work_items", Params: "{assignee, status_group}", About: "work items", Words: "work items"},
		{Name: "tokens", Params: "{seat, previous}", About: "token spend", Words: "token spend"},
		{Name: "advisor_capacity", Params: "{}", About: "hours each model entry was out"}}
}

func (r *agendaReads) Answer(_ context.Context, name string, _ map[string]any) (any, error) {
	r.mu.Lock()
	r.asked = append(r.asked, name)
	r.mu.Unlock()
	switch name {
	case "work_items":
		items := make([]any, 200)
		for i := range items {
			items[i] = map[string]any{"key": fmt.Sprintf("GP-%d", i+1), "assignee": "sre", "status": "todo",
				"title": strings.Repeat("a task that waits ", 8)}
		}
		return map[string]any{"total": 200, "items": items}, nil
	case "tokens":
		return map[string]any{"rows": []any{map[string]any{"handle": "app-engineer", "input_tokens": 1_384_000_000,
			"cache_read_tokens": 1_350_000_000, "output_tokens": 600_000, "turns": 137}},
			"previous": []any{map[string]any{"handle": "app-engineer", "input_tokens": 138_000_000}}}, nil
	}
	return nil, errors.New("the capacity tables are not in service")
}

// THE DEEP RUN ANSWERS ITS WHOLE AGENDA, SHOWS IT LIVE, AND KEEPS ITS CONTEXT SMALL. Its agenda is
// built before round one: the stat that moved the wrong way, what every open rule finding says, and
// what the rules could not read. Submitting with items unanswered is refused, so a run that would
// have stopped after one hypothesis answers every item: confirmed or refuted with numbers it read,
// or unreadable naming the read that failed, and it may add its own. Each read and each verdict is
// a progress line. A large answer reaches the model as an outline it looks into, a read asked again
// is not asked again, and the snapshot is served once. A finding about tokens that does not cite
// the cache reads is dropped; one that does keeps each number's unit and the range before.
// Mutation: drop any of these, and a check here fails (the run as it stood before this test failed
// all of them: it submitted at once, said two lines, and resent every answer whole).
func TestTheDeepRunAnswersItsWholeAgendaInFewTokens(t *testing.T) {
	t.Parallel()
	d := planted(t)
	d.Unread = []string{advisor.UnreadCapacity}
	up, down := "up", "down"
	was := 10.0
	snapshot := advisor.Snapshot{Stats: []advisor.Stat{
		{Key: "waiting_items", Label: "Items waiting to start", Value: 30, Unit: advisor.UnitItems, Was: &was, Better: &down},
		{Key: "done_items", Label: "Items done", Value: 12, Unit: advisor.UnitItems, Was: &was, Better: &up},
	}}
	reads := &agendaReads{}
	verdict := func(id, v, note, from string) llmtest.Call {
		args := map[string]any{"id": id, "verdict": v, "note": note}
		if v == string(VerdictUnreadable) {
			args["read"] = from
		} else {
			args["numbers"] = []any{map[string]any{"label": "The number", "from": from}}
		}
		raw, _ := json.Marshal(args)
		return llmtest.Call{Name: VerdictTool, Args: string(raw)}
	}
	fact := func(key, label, from, unit string, value int64, wasFrom string) map[string]any {
		f := map[string]any{"key": key, "label": label, "value": value, "from": from, "unit": unit}
		if wasFrom != "" {
			f["was_from"] = wasFrom
		}
		return f
	}
	split := deepFinding("app-engineer read 1350000000 cached tokens in 137 turns.",
		fact("cache", "Cache reads", "read_2.rows[0].cache_read_tokens", "tokens", 1_350_000_000, ""),
		fact("input", "Input tokens", "read_2.rows[0].input_tokens", "tokens", 1_384_000_000, "read_2.previous[0].input_tokens"),
		fact("turns", "Turns", "read_2.rows[0].turns", "turns", 137, ""))
	unsplit := deepFinding("app-engineer spent 1384000000 tokens in 137 turns.",
		fact("input", "Input tokens", "read_2.rows[0].input_tokens", "tokens", 1_384_000_000, ""),
		fact("turns", "Turns", "read_2.rows[0].turns", "turns", 137, ""))
	unitless := deepFinding("sre holds 200 items.", map[string]any{"key": "items", "label": "Items",
		"value": 200, "from": "read_1.total"})
	raw, err := json.Marshal(map[string]any{"findings": []any{split, unsplit, unitless}})
	if err != nil {
		t.Fatal(err)
	}
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "starting", Calls: []llmtest.Call{{Name: SnapshotTool, Args: `{}`}, {Name: ListReadsTool, Args: `{}`}}},
		{Text: "reading", Calls: []llmtest.Call{
			{Name: ReadTool, Args: `{"name":"work_items","params":{"assignee":"sre","status_group":"todo"}}`}}},
		{Text: "looking", Calls: []llmtest.Call{{Name: LookTool, Args: `{"at":"read_1.items[150]"}`},
			{Name: ReadTool, Args: `{"name":"work_items","params":{"status_group":"todo","assignee":"sre"}}`},
			{Name: SnapshotTool, Args: `{}`}}},
		{Text: "done already", Calls: []llmtest.Call{{Name: SubmitTool, Args: `{"findings":[]}`}}},
		{Text: "answering", Calls: []llmtest.Call{
			verdict("A1", "confirmed", "30 items wait against 10 the week before", "snapshot.stats[0].value"),
			verdict("A2", "refuted", "the health finding names no blocked work", "read_1.total"),
			{Name: ReadTool, Args: `{"name":"advisor_capacity"}`}}},
		{Text: "answering", Calls: []llmtest.Call{
			verdict("A3", "confirmed", "agents learn on their main model", "read_1.total"),
			verdict("A4", "unreadable", "the capacity tables could not be read", "advisor_capacity"),
			{Name: AddTool, Args: `{"question":"Did app-engineer's tokens jump, and is it the cache?"}`},
			{Name: ReadTool, Args: `{"name":"tokens","params":{"seat":"app-engineer","previous":true}}`}}},
		{Text: "answering", Calls: []llmtest.Call{
			verdict("A5", "refuted", "the jump is cache reads, 1.35B of 1.38B", "read_2.rows[0].cache_read_tokens")}},
		{Text: "submitting", Calls: []llmtest.Call{{Name: SubmitTool, Args: string(raw)}}},
		{Text: "submitting again", Calls: []llmtest.Call{{Name: SubmitTool, Args: string(raw)}}},
		{Text: "submitting as it is", Calls: []llmtest.Call{{Name: SubmitTool, Args: string(raw)}}},
	}}}}
	var told []string
	got := RunDeep(t.Context(), Deps{Provider: prov, ProviderKey: "p", Host: host(t),
		Now: func() time.Time { return monday }}, DeepData{Data: d, Snapshot: snapshot, Reads: reads}, DefaultDeepBounds,
		func(p Progress) { told = append(told, p.Text) })

	// 6a: the agenda, from each source, every item answered; the early submission refused.
	sources := map[string]bool{}
	for _, it := range got.Agenda {
		sources[it.Source] = true
		if it.Verdict == VerdictOpen || it.Verdict == "" || it.Note == "" {
			t.Errorf("agenda item %+v has no verdict", it)
		}
	}
	if len(got.Agenda) != 5 || !sources[AgendaStat] || !sources[AgendaFinding] || !sources[AgendaUnread] ||
		!sources[AgendaModel] || got.Agenda[0].Question == "" || !strings.Contains(got.Agenda[0].Question, "Items waiting") {
		t.Errorf("agenda = %+v, want the waiting stat, both rule findings, the capacity read and the run's own", got.Agenda)
	}
	if n := got.Agenda[0].Numbers; len(n) != 1 || n[0].Value != 30 || got.Agenda[3].Read != "advisor_capacity" {
		t.Errorf("verdicts = %+v, want the stat's 30 cited and the failed read named", got.Agenda)
	}
	results := prov.Results()
	early := slices.IndexFunc(results, func(r llmtest.Result) bool { return r.Name == SubmitTool })
	if early < 0 || !results[early].Failed || !strings.Contains(results[early].Output, "A1, A2, A3, A4") {
		t.Errorf("the early submission was answered %+v, want it refused naming the open items", results[early])
	}
	// 6b: a line per read and per verdict, in words.
	for _, want := range []string{"Reading the company's numbers", "Reading work items for sre, todo",
		"Could not read advisor_capacity", "Confirmed: 30 items wait against 10 the week before",
		"Could not check: the capacity tables could not be read", "Reading token spend for app-engineer"} {
		if !slices.Contains(told, want) {
			t.Errorf("progress %q was not told: %q", want, told)
		}
	}
	// 6c: the big answer as an outline, looked into; nothing read or served twice.
	read := results[slices.IndexFunc(results, func(r llmtest.Result) bool { return r.Name == ReadTool })]
	if len(read.Output) > outlineBytes+1024 || !strings.Contains(read.Output, "look at read_1.items[3] and on") {
		t.Errorf("the 200 items reached the model as %d bytes: %.300s", len(read.Output), read.Output)
	}
	look := results[slices.IndexFunc(results, func(r llmtest.Result) bool { return r.Name == LookTool })]
	if !strings.Contains(look.Output, "GP-151") {
		t.Errorf("look answered %.200s, want item 151", look.Output)
	}
	asked := 0
	for _, name := range reads.asked {
		if name == "work_items" {
			asked++
		}
	}
	if asked != 1 || !slices.ContainsFunc(results, func(r llmtest.Result) bool {
		return r.Name == SnapshotTool && strings.Contains(r.Output, "already")
	}) {
		t.Errorf("work_items asked %d times, results %+v: want once, the snapshot served once", asked, results)
	}
	// 6d: the token split and each number's unit and range before.
	// (A NUMBER THAT SAYS NO UNIT is no longer dropped: its unit is read off
	// where it was cited, and the finding listed.)
	if got.Status != advisor.PassDone || len(got.Findings) != 2 || got.Dropped[advisor.DropTokensUnsplit] != 1 ||
		len(got.Dropped) != 1 || got.Repaired[advisor.RepairEngineWords] != 1 {
		t.Fatalf("run = %+v, want the split and the unitless findings, the unsplit dropped", got)
	}
	at := slices.IndexFunc(got.Findings, func(f advisor.Finding) bool { return strings.Contains(f.Headline, "cached") })
	if at < 0 {
		t.Fatalf("findings = %+v, want the split one", got.Findings)
	}
	f := got.Findings[at]
	input := f.Evidence[slices.IndexFunc(f.Evidence, func(x advisor.Fact) bool { return x.Key == "input" })]
	if input.Unit != advisor.UnitTokens || input.Was == nil || *input.Was != 138_000_000 {
		t.Errorf("input = %+v, want tokens beside the week before's 138M", input)
	}
}
