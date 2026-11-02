package queries_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// withHistory hands a source the event history and the usage domain a server's
// detail reads its activity from, over one fresh data node.
func withHistory(t *testing.T, s queries.Sources) (queries.Sources, *store.DB) {
	t.Helper()
	db := openStore(t)
	s.Events, s.Usage = fleetOf(db.Events()), db.Replicated()
	return s, db
}

// applyUsage applies one usage record into a data node's replicated estate, as
// the domain's applier does from the log.
func applyUsage(t *testing.T, db *store.DB, r usage.Record, seq uint64) {
	t.Helper()
	body, err := r.Encode()
	if err != nil {
		t.Fatalf("encode %s: %v", r.Subject, err)
	}
	env, err := usage.Domain{}.Envelope(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := statelog.Record{Envelope: env, Payload: body,
		Position: statelog.Position{Stream: usage.Domain{}.Stream().Name, Generation: 1, Seq: seq}}
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := usage.NewApplier().Apply(t.Context(), tx, rec,
			statelog.ApplyOptions{MaxVariables: db.Replicated().Caps().MaxVariables})
		return err
	}); err != nil {
		t.Fatalf("apply %s: %v", r.Subject, err)
	}
}

// A SERVER'S DETAIL ANSWERS HOW IT HAS BEEN USED: its calls per tool over the
// last week, every data node's summed, each with the seats that made them by
// the chart's handle; and its newest calls out of the phase records that made
// them, each naming its seat, its turn, its outcome and its time, and never its
// arguments or its result.
//
// Mutation: drop either node's day from the sum, or name a seat by its id while
// the chart holds it, or carry a call's arguments onto its row, and this is
// red.
func TestAServersDetailAnswersItsUsageAndItsRecentCalls(t *testing.T) {
	t.Parallel()
	backend := mcpFleet(t)
	surface, _ := configSurface(t, mcpCompany)
	s := mcpSources(t, backend)
	s.Config, s.MCPListings = surface, fakeListings{}
	s, db := withHistory(t, s)
	now := time.Now().UTC().Truncate(time.Second)
	s.Now = func() time.Time { return now }
	today := now.Format(time.DateOnly)

	cfg, err := config.ParseCompany([]byte(mcpCompany))
	if err != nil {
		t.Fatal(err)
	}
	organization, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	seatID, ok := organization.AgentIDFor(organization.AgentSeatByHandle("backend-dev"))
	if !ok {
		t.Fatal("backend-dev has no agent id")
	}
	seat := seatID.String()
	// THE DAYS NAME NOBODY (a day with spend and no ended turn's handle
	// derives none), so only the chart can name the seat: what the answer
	// reads first for a seat the chart holds.
	day := func(node string, tools ...usage.ToolCalls) usage.Record {
		return usage.Record{
			RecordEnvelope: usage.RecordEnvelope{Writer: node, Subject: usage.Subject{
				Kind: usage.KindSeat, Node: node, Day: today, Seat: seat}},
			Role: "Backend Dev", Turns: &usage.Turns{Count: 1}, Tools: tools,
		}
	}
	last := now.Add(-time.Minute)
	applyUsage(t, db, day("node-a",
		usage.ToolCalls{Server: "github", Tool: "gh_get_issue", Calls: 3, Failed: 1, TotalMS: 300,
			LastAt: now.Add(-time.Hour)},
		usage.ToolCalls{Server: "linear", Tool: "linear_list", Calls: 9}), 1)
	applyUsage(t, db, day("node-b",
		usage.ToolCalls{Server: "github", Tool: "gh_get_issue", Calls: 2, Refused: 1, TotalMS: 50,
			LastAt: last}), 2)

	phase, err := json.Marshal(map[string]any{
		"agent_id": seat, "role": "Backend Dev", "turn_id": "t-1", "phase": "execute",
		"tool_executions": []types.ToolExecution{{
			"name": "gh_get_issue", "arguments": `{"token":"ghp_never_answered"}`,
			"result": "customer data never answered", "success": true, "round": 1,
			"started_at": last.Format(time.RFC3339Nano), "duration_ms": 25,
			"source": "mcp:github", "origin": "mcp:github", "server": "github",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Events().Append(t.Context(), store.EventRecord{ID: "p-1",
		Type: "agent_phase_completed", Time: now.Add(-30 * time.Second), Category: "agent",
		Actor: "Backend Dev", Tags: store.ExtractTags(phase), Payload: phase}); err != nil {
		t.Fatal(err)
	}

	got, err := s.MCPServer(t.Context(), "github")
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}
	if got.Usage.Days != queries.MCPUsageDays || got.Usage.Until != today || got.Usage.Elided != 0 {
		t.Errorf("the usage window is %s..%s (%d days, elided %d), want the week ending %s",
			got.Usage.Since, got.Usage.Until, got.Usage.Days, got.Usage.Elided, today)
	}
	if len(got.Usage.Tools) != 1 {
		t.Fatalf("github's usage is %+v, want gh_get_issue alone", got.Usage.Tools)
	}
	u := got.Usage.Tools[0]
	if u.Tool != "gh_get_issue" || u.Calls != 5 || u.Failed != 1 || u.Refused != 1 ||
		u.TotalMS != 350 || u.LastAt == nil || !u.LastAt.Equal(last) ||
		len(u.Seats) != 1 || u.Seats[0] != "backend-dev" {
		t.Errorf("gh_get_issue's usage is %+v, want both nodes' days summed, by backend-dev", u)
	}
	if len(got.Calls.Calls) != 1 || !got.Calls.Coverage.Complete {
		t.Fatalf("github's calls are %+v (coverage %+v), want the one call", got.Calls.Calls, got.Calls.Coverage)
	}
	c := got.Calls.Calls[0]
	if c.Seat != "backend-dev" || c.TurnID != "t-1" || c.Tool != "gh_get_issue" ||
		c.Outcome != mcp.CallAnswered || c.DurationMS == nil || *c.DurationMS != 25 || !c.At.Equal(last) {
		t.Errorf("the call is %+v", c)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, words := range []string{"ghp_never_answered", "customer data never answered"} {
		if strings.Contains(string(raw), words) {
			t.Errorf("the detail carries a call's own words %q", words)
		}
	}
}

// A PAGE OF TURNS BY TOOL NAMES THE SERVER: two servers can offer one name, so
// a tool alone is refused with the parameter to add, and a server with its
// tool, or alone, is answered.
//
// Mutation: drop the refusal, and the page lists every turn.
func TestATurnsPageByToolNamesItsServer(t *testing.T) {
	t.Parallel()
	r := registryOver(t, queries.Sources{Events: fleetOf(openStore(t).Events())})
	if _, err := r.Answer(everyGrant(t), "turns", map[string]any{"tool": "gh_get_issue"}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a tool with no server answered %v, want bad params", err)
	}
	for _, params := range []map[string]any{
		{"server": "github"}, {"server": "github", "tool": "gh_get_issue"},
	} {
		if _, err := r.Answer(everyGrant(t), "turns", params); err != nil {
			t.Errorf("turns %v was refused: %v", params, err)
		}
	}
}

// sortedKeys is a JSON object's keys, sorted.
func sortedKeys(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }
