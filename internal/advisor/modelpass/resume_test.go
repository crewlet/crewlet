package modelpass

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// A DEEP RUN TAKEN OVER MID-AGENDA RESUMES WHERE IT STOOD: the first holder
// answers two of four items and stops; its checkpoint keeps the agenda with
// those verdicts and the read it asked. The next holder starts from it: the
// two items keep their verdicts, the run is told what was read before and
// cites its own reads after it, and it answers only the open items before it
// submits. Mutation: start the second run fresh, and its submission is refused
// with the first two items open again.
func TestADeepRunTakenOverMidAgendaResumesWhereItStood(t *testing.T) {
	t.Parallel()
	d := planted(t)
	d.Unread = []string{advisor.UnreadCapacity}
	down, was := "down", 10.0
	snapshot := advisor.Snapshot{Stats: []advisor.Stat{{Key: "waiting_items", Label: "Items waiting to start",
		Value: 30, Unit: advisor.UnitItems, Was: &was, Better: &down}}}
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
	var mu sync.Mutex
	var kept []Checkpoint
	keep := func(c Checkpoint) { mu.Lock(); kept = append(kept, c); mu.Unlock() }

	first := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "reading", Calls: []llmtest.Call{{Name: SnapshotTool, Args: `{}`},
			{Name: ReadTool, Args: `{"name":"work_items","params":{"assignee":"sre","status_group":"todo"}}`}}},
		{Text: "answering", Calls: []llmtest.Call{
			verdict("A1", "confirmed", "30 items wait against 10", "snapshot.stats[0].value"),
			verdict("A2", "refuted", "the health finding names no blocked work", "read_1.total")}},
	}}}}
	got := RunDeepFrom(t.Context(), Deps{Provider: first, ProviderKey: "p", Host: host(t),
		Now: func() time.Time { return monday }}, DeepData{Data: d, Snapshot: snapshot, Reads: &agendaReads{}},
		DefaultDeepBounds, nil, nil, keep)
	if got.Status == advisor.PassDone {
		t.Fatalf("the first holder's run concluded: %+v", got)
	}
	mu.Lock()
	last := kept[len(kept)-1]
	mu.Unlock()
	if len(last.Agenda) != 4 || last.Agenda[0].Verdict != VerdictConfirmed || last.Agenda[1].Verdict != VerdictRefuted ||
		last.Agenda[2].Verdict != "" || len(last.Reads) != 1 || last.Reads[0].As != "read_1" ||
		last.Reads[0].Name != "work_items" || last.Reads[0].Params["assignee"] != "sre" {
		t.Fatalf("checkpoint = %+v, want two items answered and the read asked", last)
	}

	second := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "reading", Calls: []llmtest.Call{
			{Name: ReadTool, Args: `{"name":"work_items","params":{"assignee":"sre","status_group":"todo"}}`},
			{Name: ReadTool, Args: `{"name":"advisor_capacity"}`}}},
		{Text: "answering", Calls: []llmtest.Call{
			verdict("A3", "confirmed", "agents learn on their main model", "read_2.total"),
			verdict("A4", "unreadable", "the capacity tables could not be read", "advisor_capacity")}},
		{Text: "submitting", Calls: []llmtest.Call{{Name: SubmitTool, Args: `{"findings":[]}`}}},
	}}}}
	var told []string
	resumed := RunDeepFrom(t.Context(), Deps{Provider: second, ProviderKey: "p", Host: host(t),
		Now: func() time.Time { return monday }}, DeepData{Data: d, Snapshot: snapshot, Reads: &agendaReads{}},
		DefaultDeepBounds, func(p Progress) { told = append(told, p.Text) }, &last, keep)
	if resumed.Status != advisor.PassDone || resumed.StoppedBy != StoppedDone || resumed.Resumed != 2 {
		t.Fatalf("resumed run = %+v, want it concluded with two items carried", resumed)
	}
	if a := resumed.Agenda; a[0].Note != "30 items wait against 10" || a[1].Verdict != VerdictRefuted ||
		a[2].Verdict != VerdictConfirmed || a[3].Verdict != VerdictUnreadable {
		t.Errorf("agenda = %+v, want the first two carried and the last two answered now", a)
	}
	opening := ""
	for _, m := range second.Seen()[0].Messages {
		opening += m.Content
	}
	if !strings.Contains(opening, "taken over") || !strings.Contains(opening, "read_1 (work_items assignee=sre status_group=todo)") ||
		!strings.Contains(opening, "answered before: confirmed, 30 items wait against 10") {
		t.Errorf("the resumed run was told %q, want the takeover, the answered items and the reads made before", opening)
	}
	if !slices.ContainsFunc(second.Results(), func(r llmtest.Result) bool {
		return r.Name == ReadTool && strings.Contains(r.Output, `"cite_as":"read_2"`)
	}) {
		t.Errorf("results %+v, want the resumed run's first read cited as read_2", second.Results())
	}
	if !slices.Contains(told, "Picked up where it stopped: 2 of 4 questions already answered") {
		t.Errorf("progress = %q, want the takeover said", told)
	}
}
