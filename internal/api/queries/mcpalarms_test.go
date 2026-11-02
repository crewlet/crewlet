package queries_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/statelog"
)

// alarmsDown is the instant the fixture evaluation's outages began.
var alarmsDown = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

// mcpEvaluation is a latest evaluation standing every MCP kind, beside a
// node-wide kind and a model kind the MCP answer leaves out.
func mcpEvaluation() []statelog.Alarm {
	return []statelog.Alarm{
		{Kind: statelog.KindApplyLag, Detail: "this node is 2m behind the log", Remedy: "Check the applier."},
		{Kind: statelog.KindLLMLimit, Detail: "default sits out its session limit", Remedy: "Wait.",
			Entries: []statelog.AlarmEntry{{Key: "default", Since: alarmsDown}}},
		{Kind: statelog.KindMCPServerFailing, Detail: "no instance of github is serving", Remedy: "Fix github.",
			Entries: []statelog.AlarmEntry{{Key: "github", Detail: "no instance of github is serving",
				Since: alarmsDown, Seat: "pm"}}},
		{Kind: statelog.KindMCPServerPartial, Detail: "1 of 3 instances of jira is not serving", Remedy: "Fix jira.",
			Entries: []statelog.AlarmEntry{
				{Key: "docs", Detail: "1 of 2 instances of docs is not serving", Since: alarmsDown},
				{Key: "jira", Detail: "1 of 3 instances of jira is not serving", Since: alarmsDown,
					Seat: "backend-dev"},
			}},
		{Kind: statelog.KindMCPServerWaitingLong, Detail: "an instance of github has waited 1h", Remedy: "Open it.",
			Entries: []statelog.AlarmEntry{{Key: "github", Detail: "an instance of github has waited 1h",
				Since: alarmsDown.Add(-time.Hour), Seat: "backend-dev"}}},
		{Kind: statelog.KindMCPToolsRefused, Detail: "2 of linear's tools cannot be offered", Remedy: "Rename.",
			Entries: []statelog.AlarmEntry{{Key: "linear", Detail: "2 of linear's tools cannot be offered"}}},
	}
}

// THE MCP ANSWER CARRIES THE EVALUATION'S MCP ALARMS AND NOTHING ELSE, in the
// table's order, each server with the gesture that fixes it: a server nothing
// serves restarted whole, a partial one only where its failure is (one agent's
// instance, or every node's where a shared server's is down), a wait at the
// integration minting the agent's credential, a refused name in the
// configuration. An instant the condition does not carry is null.
//
// Mutation: restart a failing server's one agent, restart a partial one whole,
// carry a node-wide or a model kind, or send a zero instant, and this fails.
func TestTheMCPAnswerCarriesItsAlarmsWithTheGestureThatFixesEach(t *testing.T) {
	t.Parallel()
	src := mcpSources(t, mcpFleet(t))
	src.Alarms = mcpEvaluation
	got := askMCPStatus(t, src)

	kinds := make([]statelog.Kind, 0, len(got.Alarms))
	for _, a := range got.Alarms {
		kinds = append(kinds, a.Kind)
	}
	if want := []statelog.Kind{statelog.KindMCPServerFailing, statelog.KindMCPServerPartial,
		statelog.KindMCPServerWaitingLong, statelog.KindMCPToolsRefused}; !slices.Equal(kinds, want) {
		t.Fatalf("the answer carries %v, want exactly the MCP kinds %v", kinds, want)
	}
	type fixed struct {
		kind  statelog.Kind
		entry string
		fix   queries.MCPFix
	}
	var fixes []fixed
	for _, a := range got.Alarms {
		for _, e := range a.Entries {
			fixes = append(fixes, fixed{a.Kind, e.Key, e.Fix})
		}
	}
	if want := []fixed{
		{statelog.KindMCPServerFailing, "github", queries.MCPFix{Kind: queries.MCPFixRestart, Server: "github"}},
		{statelog.KindMCPServerPartial, "docs", queries.MCPFix{Kind: queries.MCPFixRestart, Server: "docs"}},
		{statelog.KindMCPServerPartial, "jira", queries.MCPFix{Kind: queries.MCPFixRestart, Server: "jira",
			Seat: "backend-dev"}},
		{statelog.KindMCPServerWaitingLong, "github", queries.MCPFix{Kind: queries.MCPFixIntegration,
			Server: "github", Seat: "backend-dev"}},
		{statelog.KindMCPToolsRefused, "linear", queries.MCPFix{Kind: queries.MCPFixEdit, Server: "linear"}},
	}; !slices.Equal(fixes, want) {
		t.Errorf("fixes = %+v\nwant %+v", fixes, want)
	}
	failing := got.Alarms[0]
	if failing.Detail != "no instance of github is serving" || failing.Remedy != "Fix github." ||
		failing.Entries[0].Seat != "pm" || failing.Entries[0].Since == nil || !failing.Entries[0].Since.Equal(alarmsDown) {
		t.Errorf("failing = %+v, want the evaluation's words, its agent and its instant", failing)
	}
	body := string(mustJSON(t, got))
	if !strings.Contains(body, `"key":"linear","detail":"2 of linear's tools cannot be offered","since":null`) {
		t.Errorf("a refused name's missing instant is not null: %s", body)
	}

	// NO EVALUATION IS NO ALARM, AND STILL A LIST.
	quiet := askMCPStatus(t, mcpSources(t, mcpFleet(t)))
	if quiet.Alarms == nil || len(quiet.Alarms) != 0 {
		t.Errorf("with no evaluation the answer carries %#v, want an empty list", quiet.Alarms)
	}
}

// EVERY MCP KIND THE TABLE RAISES IS FIXED BY A GESTURE THE PAGE KNOWS: a kind a
// later piece adds to the table is carried with its fix, or this fails, rather
// than reaching the answer as nothing at all.
//
// Mutation: drop a kind from the answer's fixes, and this fails.
func TestEveryMCPAlarmKindIsFixedByAGestureThePageKnows(t *testing.T) {
	t.Parallel()
	kinds := statelog.PerServerKinds()
	if len(kinds) == 0 {
		t.Fatal("the table raises no MCP kind, so this proves nothing")
	}
	for _, kind := range kinds {
		src := mcpSources(t, mcpFleet(t))
		src.Alarms = func() []statelog.Alarm {
			return []statelog.Alarm{{Kind: kind, Entries: []statelog.AlarmEntry{{Key: "github", Seat: "pm"}}}}
		}
		got := askMCPStatus(t, src)
		if len(got.Alarms) != 1 || len(got.Alarms[0].Entries) != 1 || !got.Alarms[0].Entries[0].Fix.Kind.Valid() ||
			got.Alarms[0].Entries[0].Fix.Server != "github" {
			t.Errorf("%s is answered as %+v, want one entry with a gesture on github", kind, got.Alarms)
		}
	}
	for _, k := range queries.MCPFixKinds {
		if !k.Valid() {
			t.Errorf("gesture %q is listed and not valid", k)
		}
	}
}
