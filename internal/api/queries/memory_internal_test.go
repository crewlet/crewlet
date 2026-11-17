package queries

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// runsEvents is an event log answering its pages in turn, the first to a read
// with no cursor, each saying more follow but the last; whole or not. Like the
// fleet's, a listing carries no payload: only ByID does.
type runsEvents struct {
	FleetEvents
	pages    [][]store.EventRecord
	payloads map[string]json.RawMessage
	complete bool
	asked    *store.ListQuery
	reads    *int
}

func (r runsEvents) List(_ context.Context, q store.ListQuery) (eventfan.Listing, eventfan.Coverage, error) {
	if q.Before == nil {
		*r.asked = q
		*r.reads = 0
	}
	i := min(*r.reads, len(r.pages)-1)
	*r.reads++
	more := i < len(r.pages)-1
	rows := make([]store.EventRecord, len(r.pages[i]))
	for j, rec := range r.pages[i] {
		rec.Payload = nil
		rows[j] = rec
	}
	return eventfan.Listing{Rows: rows, More: more}, eventfan.Coverage{Complete: r.complete}, nil
}

func (r runsEvents) ByID(_ context.Context, id string) (store.EventRecord, eventfan.Coverage, error) {
	payload, ok := r.payloads[id]
	if !ok {
		return store.EventRecord{}, eventfan.Coverage{}, fmt.Errorf("no event %s", id)
	}
	return store.EventRecord{ID: id, Payload: payload}, eventfan.Coverage{Complete: true}, nil
}

func acmeSources(t *testing.T) (Sources, string) {
	t.Helper()
	cfg, err := config.ParseCompany([]byte("name: Acme\nproviders:\n  llm:\n    p: {model: m}\n  sandbox: {fake: true}\n" +
		"roles:\n  - name: CEO\n    handle: ceo\n    llm: p\n"))
	if err != nil {
		t.Fatal(err)
	}
	roster, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	ceo, _ := roster.AgentIDFor(roster.AgentSeatByHandle("ceo"))
	return Sources{Company: func() (*config.Company, *org.Organization) { return cfg, roster }}, ceo.String()
}

// THE ADVISOR READS RUNS THAT ENDED FOR MEMORY OR ON THEIR ROUND CAP off a payload-less listing,
// its week a page at a time: the kind from `error_kind`, the box's ceiling from one record per
// seat; another failure or a seat off the chart is left out, and a partial read is unread.
func TestTheAdvisorReadsTheRunsThatEndedForMemoryOrOnTheirCap(t *testing.T) {
	t.Parallel()
	s, ceo := acmeSources(t)
	run := func(id, agent, kind string, tokens, calls int) store.EventRecord {
		return store.EventRecord{ID: id, AgentID: agent, Failed: true, Tokens: tokens, Calls: calls,
			Tags: map[string]string{"error_kind": kind, "turn_id": "turn-" + id}}
	}
	memory, _ := json.Marshal(types.ExecutorCollected{Failed: true, ErrorKind: types.ExecutorErrorOutOfMemory,
		MemoryBytes: 2 << 30})
	var asked store.ListQuery
	var reads int
	// THE OUT-OF-MEMORY RUNS ON THE SECOND PAGE, past a first page of other failures.
	events := runsEvents{asked: &asked, reads: &reads, complete: true,
		payloads: map[string]json.RawMessage{"r3": memory, "r4": memory},
		pages: [][]store.EventRecord{
			{run("r1", ceo, types.ExecutorErrorLaunchFailed, 10, 1),
				run("r2", "a-gone", types.ExecutorErrorOutOfMemory, 10, 1),
				run("r5", ceo, types.ExecutorErrorMaxTurns, 26_500_000, 30)},
			{run("r3", ceo, types.ExecutorErrorOutOfMemory, 500, 4),
				run("r4", ceo, types.ExecutorErrorOutOfMemory, 700, 6)},
		}}
	s.Events = events
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	got, err := s.endedRuns(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []advisor.EndedRun{
		{Seat: "ceo", Kind: types.ExecutorErrorMaxTurns, Turn: "turn-r5", Tokens: 26_500_000, Rounds: 30},
		{Seat: "ceo", Kind: types.ExecutorErrorOutOfMemory, Turn: "turn-r3", Tokens: 500, Rounds: 4, MemoryBytes: 2 << 30},
		{Seat: "ceo", Kind: types.ExecutorErrorOutOfMemory, Turn: "turn-r4", Tokens: 700, Rounds: 6, MemoryBytes: 2 << 30},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runs = %+v, want %+v", got, want)
	}
	if asked.Type != "executor_collected" || asked.Failed == nil || !*asked.Failed ||
		!asked.Since.Equal(now.Add(-advisor.Window)) || !asked.Until.Equal(now) {
		t.Errorf("asked %+v, want the week's failed collected runs", asked)
	}
	if reads != 2 {
		t.Errorf("read %d pages, want both pages of the week", reads)
	}
	endless := events
	endless.pages = make([][]store.EventRecord, maxFailedRunPages+1)
	for i := range endless.pages {
		endless.pages[i] = []store.EventRecord{run("x", ceo, types.ExecutorErrorLaunchFailed, 0, 0)}
	}
	s.Events = endless
	if _, err := s.endedRuns(t.Context(), now); err == nil {
		t.Error("a week past the bound was read as whole")
	}
	events.complete = false
	s.Events = events
	if _, err := s.endedRuns(t.Context(), now); err == nil {
		t.Error("a fleet that did not answer whole was read as its runs")
	}
}

// A SEAT'S BUDGET STOPS ARE READ ONCE PER WINDOW: a refusal inside a window an earlier one said
// resets later is that stop's, its scope and reset from the first refusal's record. Mutation:
// read the second refusal's record, which the fake lacks, and the read fails.
func TestTheAdvisorReadsEachBudgetStopOnce(t *testing.T) {
	t.Parallel()
	s, ceo := acmeSources(t)
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	morning := now.Add(-30 * time.Hour)
	refusal := func(id string, at time.Time) store.EventRecord {
		return store.EventRecord{ID: id, AgentID: ceo, Time: at, Failed: true,
			Tags: map[string]string{"turn_id": "turn-" + id}}
	}
	resets := morning.Add(10 * time.Hour)
	first, _ := json.Marshal(types.BudgetExhausted{BudgetType: types.BudgetScopeAgent, Period: "day",
		MaxTokens: 400_000, ResetsAt: resets.Format(time.RFC3339)})
	var asked store.ListQuery
	var reads int
	// NEWEST FIRST, as a listing answers.
	s.Events = runsEvents{asked: &asked, reads: &reads, complete: true,
		payloads: map[string]json.RawMessage{"b1": first},
		pages:    [][]store.EventRecord{{refusal("b2", morning.Add(2*time.Hour)), refusal("b1", morning)}}}
	got, err := s.budgetStops(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []advisor.BudgetStop{
		{Seat: "ceo", Turn: "turn-b1", Period: "day", Ceiling: 400_000, At: morning, Resets: resets},
		{Seat: "ceo", Turn: "turn-b2", Period: "day", Ceiling: 400_000, At: morning.Add(2 * time.Hour), Resets: resets},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stops = %+v, want %+v", got, want)
	}
	if asked.Type != "budget_exhausted" {
		t.Errorf("asked %+v, want the week's budget refusals", asked)
	}
}

// A MERGE'S SUBJECT NAMES ITS CHANGE, and a change its repository: a GitHub
// pull request's owner and name, or a Gitlet reference's repository.
func TestAMergesChangeNamesItsRepository(t *testing.T) {
	t.Parallel()
	for subject, want := range map[string][2]string{
		"merge https://github.com/acme/web/pull/42 at 0123abcd (merge)": {"acme/web", "github"},
		"merge web!42 at 0123abcd (squash)":                             {"web", "gitlet"},
		"deploy 0123 of web to prod, shipping web!1":                    {"", ""},
	} {
		repo, github, ok := advisor.ChangeRepo(mergeChange(subject))
		host := map[bool]string{true: "github", false: "gitlet"}[github]
		if !ok {
			host = ""
		}
		if repo != want[0] || host != want[1] {
			t.Errorf("%q names %q on %q, want %q on %q", subject, repo, host, want[0], want[1])
		}
	}
}
