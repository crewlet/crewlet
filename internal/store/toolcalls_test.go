package store_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/store"
)

// callingPhase is a phase record that made the given calls, tagged the way the
// observe writer tags every record ([store.ExtractTags]).
func callingPhase(t *testing.T, id string, at time.Time, agent, turn string,
	calls ...types.ToolExecution) store.EventRecord {

	t.Helper()
	body, err := json.Marshal(map[string]any{
		"agent_id": agent, "role": "Dev", "turn_id": turn, "phase": "execute",
		"tool_executions": calls,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store.EventRecord{ID: id, Type: "agent_phase_completed", Source: "Dev",
		Category: "agent", Time: at, Actor: "Dev", Tags: store.ExtractTags(body), Payload: body}
}

// A SERVER'S RECENT CALLS ARE READ OUT OF THE PHASE RECORDS THAT MADE THEM,
// newest first by when each was handed to its tool: every call whose source is
// the server's, a call the engine refused before it ran included (placed at its
// record's instant, untimed), and nothing of another server's or a builtin's.
// Each answers its outcome by the one rule, never an argument or a result. A
// custody batch not yet settled is listed by no node until one keeps it, and
// the limit cuts the newest.
//
// Mutation: read the calls by `origin` rather than `source`, and the refusal
// vanishes; drop the kept-rows predicate, and the batch in flight is listed;
// stop reading at the first record older than the newest call held rather than
// the limit-th, and the limit-two read misses a call.
func TestAServersRecentCallsAreReadOutOfThePhaseRecordsThatMadeThem(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	base := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
	ran := func(name, server string, started time.Time, ms int, failed bool) types.ToolExecution {
		row := types.ToolExecution{"name": name, "arguments": `{"token":"never-shown"}`,
			"result": "a server's words", "success": !failed, "round": 1,
			"started_at": started.Format(time.RFC3339Nano), "duration_ms": ms,
			"source": mcp.Origin(server), "origin": mcp.Origin(server), "server": server}
		if failed {
			row["error"] = "a server's words"
		}
		return row
	}
	for _, rec := range []store.EventRecord{
		callingPhase(t, "p1", at(60), "seat-1", "t-1",
			ran("github_get_issue", "github", at(58), 120, false),
			types.ToolExecution{"name": "github_close_issue", "success": false, "round": 1,
				"source": "mcp:github", "refusal": "forbidden", "result": "refused"},
			ran("linear_list_issues", "linear", at(59), 40, false),
			types.ToolExecution{"name": "remember", "success": true, "round": 1,
				"source": "builtin", "origin": "builtin"}),
		callingPhase(t, "p2", at(120), "seat-2", "t-2",
			ran("github_search", "github", at(119), 900, true)),
		callingPhase(t, "p3", at(150), "seat-1", "t-3",
			ran("linear_list_issues", "linear", at(149), 10, false)),
	} {
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}
	if err := log.WriteCustody(t.Context(), store.CustodyBatch{ID: "batch-1", Origin: "stateless-1",
		Records: []store.EventRecord{callingPhase(t, "b1", at(170), "seat-3", "t-b",
			ran("github_get_issue", "github", at(169), 5, false))}}, at(171)); err != nil {
		t.Fatalf("write the batch: %v", err)
	}

	read := func(limit int) []store.ToolCall {
		t.Helper()
		got, err := log.ToolCalls(t.Context(), store.ToolCallQuery{Server: "github",
			Since: base, Limit: limit})
		if err != nil {
			t.Fatalf("ToolCalls: %v", err)
		}
		return got
	}
	ms := func(v int64) *int64 { return &v }
	want := []store.ToolCall{
		{At: at(119), AgentID: "seat-2", TurnID: "t-2", Tool: "github_search",
			Outcome: mcp.CallFailed, DurationMS: ms(900), EventID: "p2", Index: 0},
		{At: at(60), AgentID: "seat-1", TurnID: "t-1", Tool: "github_close_issue",
			Outcome: mcp.CallRefused, Refusal: mcp.RefusalForbidden, EventID: "p1", Index: 1},
		{At: at(58), AgentID: "seat-1", TurnID: "t-1", Tool: "github_get_issue",
			Outcome: mcp.CallAnswered, DurationMS: ms(120), EventID: "p1", Index: 0},
	}
	if got := read(0); !equalCalls(got, want) {
		t.Errorf("github's calls are\n%+v\nwant\n%+v", got, want)
	}
	if got := read(2); !equalCalls(got, want[:2]) {
		t.Errorf("github's two newest calls are\n%+v\nwant\n%+v", got, want[:2])
	}
	if err := log.SettleCustody(t.Context(), "batch-1", true); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := read(1); len(got) != 1 || got[0].EventID != "b1" {
		t.Errorf("once the batch is kept its call is the newest: %+v", got)
	}
}

// equalCalls compares two listings value for value, an instant by its
// moment rather than its location.
func equalCalls(a, b []store.ToolCall) bool {
	return slices.EqualFunc(a, b, func(x, y store.ToolCall) bool {
		dx, dy := x.DurationMS, y.DurationMS
		x.DurationMS, y.DurationMS = nil, nil
		if (dx == nil) != (dy == nil) || (dx != nil && *dx != *dy) || !x.At.Equal(y.At) {
			return false
		}
		x.At, y.At = time.Time{}, time.Time{}
		return x == y
	})
}

// THE TURNS THAT CALLED A SERVER'S TOOL ARE LISTED BY IT: by the server alone,
// every turn whose ending tally names any of its tools, and with the tool
// beside it, only the turns that called that one. A turn whose ending names
// none, or another server's, is not listed.
//
// Mutation: read the filter off the tally without the server's term, and
// linear's turn is listed under github's tool.
func TestTheTurnsThatCalledAToolAreListedByIt(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Add(-time.Hour)
	for i, c := range []struct {
		turn  string
		tools []types.ToolTally
	}{
		{"t-get", []types.ToolTally{{Server: "github", Tool: "github_get_issue", Calls: 2}}},
		{"t-search", []types.ToolTally{{Server: "github", Tool: "github_search", Calls: 1, Refused: 1}}},
		{"t-linear", []types.ToolTally{{Server: "linear", Tool: "github_get_issue", Calls: 1}}},
		{"t-none", nil},
	} {
		if err := log.Append(t.Context(), endedTurn(t, c.turn+"-end",
			base.Add(time.Duration(i)*time.Minute), "seat-1", c.turn,
			types.TurnTally{DurationMS: 1_000, Tools: c.tools})); err != nil {
			t.Fatalf("append %s: %v", c.turn, err)
		}
	}
	ids := func(q store.TurnQuery) []string {
		t.Helper()
		got, err := log.Turns(t.Context(), q)
		if err != nil {
			t.Fatalf("Turns(%+v): %v", q, err)
		}
		out := []string{}
		for _, turn := range got {
			out = append(out, turn.TurnID)
		}
		slices.Sort(out)
		return out
	}
	if got := ids(store.TurnQuery{Server: "github"}); !slices.Equal(got, []string{"t-get", "t-search"}) {
		t.Errorf("github's turns are %v, want t-get and t-search", got)
	}
	if got := ids(store.TurnQuery{Server: "github", Tool: "github_get_issue"}); !slices.Equal(got, []string{"t-get"}) {
		t.Errorf("github_get_issue's turns are %v, want t-get alone, never linear's tool of that name", got)
	}
}
