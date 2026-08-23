package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

func TestAPhaseCanDiscoverAndActivateAnMCPTool(t *testing.T) {
	t.Parallel()
	// The whole reason discovery exists: a planner is shown SERVER names,
	// not tool names, and the delivery gate's own correction for a
	// wrong-guessed name tells the model to use exactly these two tools. If
	// they are not on the surface, that advice is a lie.
	r, prov, _ := fixture(t, &scriptedProvider{
		plan: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "list_mcp_server_tools",
				Arguments: map[string]any{"server": "slack"}}}},
			{ToolCalls: []llm.ToolCall{{ID: "b", Name: "activate_tool",
				Arguments: map[string]any{"name": "slack_post"}}}},
			submitCall(t, runner.SubmitPlanTool,
				`{"decision":"plan","tools_needed":["slack_post"],"steps":[{"intent":"post"}]}`),
		},
	})
	p, _, err := r.Plan(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if p.Decision != turn.PlanRun {
		t.Fatalf("decision = %s", p.Decision)
	}

	reqs := prov.requestsFor("plan")
	if len(reqs) < 3 {
		t.Fatalf("Plan made %d model calls, want the discovery round trip", len(reqs))
	}
	// The listing named the server's real tools.
	var listing string
	for _, m := range reqs[1].Messages {
		if m.Role == llm.RoleTool && m.Name == "list_mcp_server_tools" {
			listing = m.Content
		}
	}
	if !strings.Contains(listing, "slack_post") || !strings.Contains(listing, "slack_history") {
		t.Errorf("the listing did not name the server's tools:\n%s", listing)
	}
	if strings.Contains(listing, "lookup_colleague") {
		t.Errorf("the listing leaked a first-party tool:\n%s", listing)
	}
	// One line per tool. A real server publishes paragraphs, and an entry
	// that spilled would break the shape a model reads the listing as.
	if strings.Contains(listing, "Accepts a cursor") {
		t.Errorf("a description spilled past its first line:\n%s", listing)
	}
	if n := len(strings.Split(strings.TrimSpace(listing), "\n")); n != 2 {
		t.Errorf("the listing has %d lines for 2 tools:\n%s", n, listing)
	}
	// ONE server's tools, not every MCP tool. A listing that ignored the
	// argument would hand a planner the wall of text discovery exists to
	// avoid.
	if strings.Contains(listing, "jira_create") {
		t.Errorf("the listing for slack included another server's tools:\n%s", listing)
	}
	// And the activation reached the surface: the tool is offered on the
	// round AFTER it, which is what makes activation worth anything.
	if !slices.Contains(toolNames(reqs[2].Tools), "slack_post") {
		t.Errorf("the activated tool was not offered next round: %v", toolNames(reqs[2].Tools))
	}
	if slices.Contains(toolNames(reqs[0].Tools), "slack_post") {
		t.Errorf("the tool was offered before it was activated: %v", toolNames(reqs[0].Tools))
	}
	// The calls are recorded, so the ledger and the delivery gate see them.
	if !slices.Contains(callNames(p.Calls), "activate_tool") {
		t.Errorf("the discovery calls were not recorded: %v", callNames(p.Calls))
	}
}

func TestAnUnknownServerListsTheOnesThatExist(t *testing.T) {
	t.Parallel()
	// A named server with no tools and a wrong server name read very
	// differently to a model: one means "ask again later", the other means
	// "you have the name wrong". Listing what exists turns the second into
	// a recoverable round.
	r, prov, _ := fixture(t, &scriptedProvider{
		plan: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "list_mcp_server_tools",
				Arguments: map[string]any{"server": "slacck"}}}},
			submitCall(t, runner.SubmitPlanTool,
				`{"decision":"plan","tools_needed":["slack_post"],"steps":[{"intent":"post"}]}`),
		},
	})
	if _, _, err := r.Plan(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var reply string
	for _, m := range prov.requestsFor("plan")[1].Messages {
		if m.Role == llm.RoleTool && m.Name == "list_mcp_server_tools" {
			reply = m.Content
		}
	}
	if !strings.Contains(reply, "slack") {
		t.Errorf("the failure does not name the servers that exist:\n%s", reply)
	}
}

func TestActivatingAGuessedNameSaysHowToFindTheRealOne(t *testing.T) {
	t.Parallel()
	// A miss is almost always a guessed MCP tool name. Saying so, and
	// naming the way to find the real one, is the difference between a
	// recoverable round and a phase that keeps guessing.
	r, prov, _ := fixture(t, &scriptedProvider{
		plan: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "activate_tool",
				Arguments: map[string]any{"name": "slack_send_msg"}}}},
			submitCall(t, runner.SubmitPlanTool,
				`{"decision":"plan","tools_needed":["slack_post"],"steps":[{"intent":"post"}]}`),
		},
	})
	if _, _, err := r.Plan(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var reply string
	for _, m := range prov.requestsFor("plan")[1].Messages {
		if m.Role == llm.RoleTool && m.Name == "activate_tool" {
			reply = m.Content
		}
	}
	if !strings.Contains(reply, "list_mcp_server_tools") {
		t.Errorf("the failure does not say how to find the real name:\n%s", reply)
	}
	if !strings.Contains(reply, "slack_send_msg") {
		t.Errorf("the failure does not name what was tried:\n%s", reply)
	}
}

func TestOneSurfacesActivationDoesNotLeakIntoAnother(t *testing.T) {
	t.Parallel()
	// The discovery pair is per-PHASE for the same reason the submission
	// tools are: activation mutates one surface, and a shared registry
	// would carry one phase's activation into the next — or into the next
	// turn.
	r, prov, _ := fixture(t, &scriptedProvider{
		plan: []llm.Completion{
			{ToolCalls: []llm.ToolCall{{ID: "a", Name: "activate_tool",
				Arguments: map[string]any{"name": "slack_history"}}}},
			submitCall(t, runner.SubmitPlanTool,
				`{"decision":"plan","tools_needed":["slack_post"],"steps":[{"intent":"post"}]}`),
		},
		execute: []llm.Completion{text("posted")},
	})
	p, _, err := r.Plan(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, _, err := r.Execute(context.Background(), 1, p, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Execute gets the plan's tools plus the always-on set plus discovery —
	// NOT Plan's activation.
	offered := toolNames(prov.requestsFor("execute")[0].Tools)
	if slices.Contains(offered, "slack_history") {
		t.Errorf("Plan's activation leaked into Execute: %v", offered)
	}
	if !slices.Contains(offered, "slack_post") {
		t.Errorf("Execute lost its own tools: %v", offered)
	}
}

func TestTheEngineSkipsExactlyTheDiscoveryTools(t *testing.T) {
	t.Parallel()
	// A meta-tool is never a delivery, so in a record whose only job is
	// "what already happened that matters" it is pure noise — and the
	// engine names the skip list while this package names the tools. Two
	// places, one fact: renaming a tool here without touching the engine
	// would leave discovery calls in every ledger block, and a reader would
	// see a phase that "did" four things when it delivered one.
	want := []string{runner.ActivateTool, runner.ListMCPToolsTool}
	got := slices.Clone(engine.MetaToolNames())
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the engine skips %v, want exactly the discovery pair %v", got, want)
	}
}

func callNames(calls []ledger.Call) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return out
}

func TestDiscoveryRefusesAnEmptyArgument(t *testing.T) {
	t.Parallel()
	// A model that calls a tool with no argument gets a failure it can act
	// on. Without the guard, listing an empty server name reports "no tools
	// found on server """ — which reads as a server that exists and is
	// empty — and activating an empty name reports no tool called "", which
	// says nothing at all.
	r, prov, _ := fixture(t, &scriptedProvider{
		plan: []llm.Completion{
			{ToolCalls: []llm.ToolCall{
				{ID: "a", Name: "list_mcp_server_tools", Arguments: map[string]any{"server": "  "}},
				{ID: "b", Name: "activate_tool", Arguments: map[string]any{}},
			}},
			submitCall(t, runner.SubmitPlanTool,
				`{"decision":"plan","tools_needed":["slack_post"],"steps":[{"intent":"post"}]}`),
		},
	})
	if _, _, err := r.Plan(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	replies := map[string]string{}
	for _, m := range prov.requestsFor("plan")[1].Messages {
		if m.Role == llm.RoleTool {
			replies[m.Name] = m.Content
		}
	}
	if got := replies["list_mcp_server_tools"]; !strings.Contains(got, "Name a server") {
		t.Errorf("an empty server name reported %q", got)
	}
	// It still says which servers exist, so the round is recoverable.
	if got := replies["list_mcp_server_tools"]; !strings.Contains(got, "slack") {
		t.Errorf("the refusal does not name the servers that exist: %q", got)
	}
	if got := replies["activate_tool"]; !strings.Contains(got, "Name a tool") {
		t.Errorf("an empty tool name reported %q", got)
	}
}
