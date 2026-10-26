package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// discoveryEntries is the provider set a discovery case runs on: the executor
// makes no model call of its own — its run is played — so nothing is scripted.
func discoveryEntries() []phase.Entry {
	return []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
}

func TestAPhaseCanDiscoverAndActivateAnMCPTool(t *testing.T) {
	t.Parallel()
	// The whole reason discovery exists: the executor is shown SERVER names,
	// not tool names, and the delivery check's own correction for a
	// wrong-guessed name tells the model to use exactly these two tools. If
	// they are not on the surface its run calls over the bridge, that advice
	// is a lie.
	launched, req := launch(t, discoveryEntries(), buildOpts{}, 1, "", nil)
	if slices.Contains(toolNames(req.Surface.ToolDefs()), "slack_post") {
		t.Errorf("the tool was offered before it was activated: %v", toolNames(req.Surface.ToolDefs()))
	}
	listed := play(t, req, run{calls: []llm.ToolCall{{ID: "a", Name: "list_mcp_server_tools",
		Arguments: map[string]any{"server": "slack"}}}})
	listing := listed[0].Result
	if !strings.Contains(listing, "slack_post") || !strings.Contains(listing, "slack_history") {
		t.Errorf("the listing did not name the server's tools:\n%s", listing)
	}
	if strings.Contains(listing, "lookup_colleague") {
		t.Errorf("the listing leaked a first-party tool:\n%s", listing)
	}
	// ONE BULLET PER TOOL, description whole. A real server publishes
	// paragraphs, and the entry that used to be cut to its first line was
	// cut exactly where the usable part starts: "Accepts a cursor for
	// paging" is what an executor needs to call the tool correctly, and this
	// listing is the only place it is ever shown. Continuation lines are
	// indented so the shape survives without paying for it in content.
	if !strings.Contains(listing, "Accepts a cursor") {
		t.Errorf("a description was cut at its first line:\n%s", listing)
	}
	if !strings.Contains(listing, "\n  Accepts a cursor") {
		t.Errorf("a continuation line was not indented under its bullet:\n%s", listing)
	}
	bullets := 0
	for line := range strings.SplitSeq(strings.TrimSpace(listing), "\n") {
		if strings.HasPrefix(line, "- ") {
			bullets++
		}
	}
	if bullets != 2 {
		t.Errorf("the listing has %d bullets for 2 tools:\n%s", bullets, listing)
	}
	// ONE server's tools, not every MCP tool. A listing that ignored the
	// argument would hand an executor the wall of text discovery exists to
	// avoid.
	if strings.Contains(listing, "jira_create") {
		t.Errorf("the listing for slack included another server's tools:\n%s", listing)
	}

	activated := play(t, req, run{calls: []llm.ToolCall{
		{ID: "b", Name: "activate_tool", Arguments: map[string]any{"name": "slack_post"}},
		submitWork(t),
	}})
	// And the activation reached the surface: the bridge lists the tool
	// from the call after it, which is what makes activation worth
	// anything.
	if !slices.Contains(toolNames(req.Surface.ToolDefs()), "slack_post") {
		t.Errorf("the activated tool is not on the run's surface: %v", toolNames(req.Surface.ToolDefs()))
	}
	_, w, _ := collect(t, discoveryEntries(), buildOpts{}, launched, nil,
		append(listed, activated...), "", runner.RunRecord{})
	// The run activates but never posts, so the honest outcome is blocked
	// — the subject here is the discovery round trip, not delivery.
	if w.Outcome != turn.OutcomeBlocked {
		t.Fatalf("outcome = %s", w.Outcome)
	}
	// The calls are recorded, so the ledger and the delivery check see them.
	if !slices.Contains(callNames(w.Calls), "activate_tool") {
		t.Errorf("the discovery calls were not recorded: %v", callNames(w.Calls))
	}
}

func TestAnUnknownServerListsTheOnesThatExist(t *testing.T) {
	t.Parallel()
	// A named server with no tools and a wrong server name read very
	// differently to a model: one means "ask again later", the other means
	// "you have the name wrong". Listing what exists turns the second into
	// a recoverable call.
	_, req := launch(t, discoveryEntries(), buildOpts{}, 1, "", nil)
	reply := play(t, req, run{calls: []llm.ToolCall{{ID: "a", Name: "list_mcp_server_tools",
		Arguments: map[string]any{"server": "slacck"}}}})[0].Result
	if !strings.Contains(reply, "slack") {
		t.Errorf("the failure does not name the servers that exist:\n%s", reply)
	}
}

func TestActivatingAGuessedNameSaysHowToFindTheRealOne(t *testing.T) {
	t.Parallel()
	// A miss is almost always a guessed MCP tool name. Saying so, and
	// naming the way to find the real one, is the difference between a
	// recoverable call and a run that keeps guessing.
	_, req := launch(t, discoveryEntries(), buildOpts{}, 1, "", nil)
	reply := play(t, req, run{calls: []llm.ToolCall{{ID: "a", Name: "activate_tool",
		Arguments: map[string]any{"name": "slack_send_msg"}}}})[0].Result
	if !strings.Contains(reply, "list_mcp_server_tools") {
		t.Errorf("the failure does not say how to find the real name:\n%s", reply)
	}
	if !strings.Contains(reply, "slack_send_msg") {
		t.Errorf("the failure does not name what was tried:\n%s", reply)
	}
}

func TestOneSurfacesActivationDoesNotLeakIntoAnother(t *testing.T) {
	t.Parallel()
	// The discovery pair is per-SURFACE for the same reason the submission
	// tools are: activation mutates one surface, and a shared registry
	// would carry one launch's activation into the next — or into the next
	// turn. A self_iterate launches on a fresh surface, which is correct:
	// its run's context started over too.
	launcher := &playLauncher{}
	r, _ := buildWith(t, discoveryEntries(), buildOpts{launcher: launcher})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	first := launcher.last(t)
	play(t, first, run{calls: []llm.ToolCall{{ID: "a", Name: "activate_tool",
		Arguments: map[string]any{"name": "slack_history"}}}})
	if !slices.Contains(toolNames(first.Surface.ToolDefs()), "slack_history") {
		t.Errorf("the activation did not reach its own surface: %v", toolNames(first.Surface.ToolDefs()))
	}
	if _, _, err := r.Execute(context.Background(), 2, "try again", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// ...and the next launch starts clean.
	next := toolNames(launcher.last(t).Surface.ToolDefs())
	if slices.Contains(next, "slack_history") {
		t.Errorf("an activation leaked into the next launch: %v", next)
	}
	if !slices.Contains(next, runner.SubmitWorkTool) {
		t.Errorf("the next launch lost its own tools: %v", next)
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
	_, req := launch(t, discoveryEntries(), buildOpts{}, 1, "", nil)
	replies := map[string]string{}
	for _, c := range play(t, req, run{calls: []llm.ToolCall{
		{ID: "a", Name: "list_mcp_server_tools", Arguments: map[string]any{"server": "  "}},
		{ID: "b", Name: "activate_tool", Arguments: map[string]any{}},
	}}) {
		replies[c.Name] = c.Result
	}
	if got := replies["list_mcp_server_tools"]; !strings.Contains(got, "Name a server") {
		t.Errorf("an empty server name reported %q", got)
	}
	// It still says which servers exist, so the call is recoverable.
	if got := replies["list_mcp_server_tools"]; !strings.Contains(got, "slack") {
		t.Errorf("the refusal does not name the servers that exist: %q", got)
	}
	if got := replies["activate_tool"]; !strings.Contains(got, "Name a tool") {
		t.Errorf("an empty tool name reported %q", got)
	}
}

// THE REVIEWER CANNOT DISCOVER A TOOL.
//
// It grades the round from the evidence in front of it, and its prompt promises
// it no domain tools. activate_tool reaches every tool in the registry, MCP
// writes included, so a reviewer offered the discovery pair could post to the
// thread in the middle of judging whether the executor should have. Every
// phase surface used to get the pair, the reviewer's included. A reviewer that
// asks for activate_tool anyway is refused, and slack_post never reaches it.
func TestTheReviewerIsOfferedOnlyItsSubmission(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llmtest.Response{
			calling(callOf(t, runner.ActivateTool, map[string]any{"name": "slack_post"})),
			submitCall(t, runner.SubmitReviewTool, `{"decision":"done","final_artifact":"a"}`),
		},
	})
	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	offered := prov.offeredFor("review")
	if len(offered) < 2 {
		t.Fatalf("the reviewer made %d model requests, want the refused activation and its answer",
			len(offered))
	}
	// Refused BY ITS CLI: a name the reviewer was never offered never reaches
	// its tools at all.
	if got := prov.answerTo("review", runner.ActivateTool); !strings.Contains(got, "No such tool") {
		t.Errorf("the reviewer's activation was answered %q, want its CLI's refusal", got)
	}
	for i, got := range offered {
		if !slices.Equal(got, []string{runner.SubmitReviewTool}) {
			t.Errorf("review call %d offered %v, want only %s: the discovery pair "+
				"puts every tool in the registry within the reviewer's reach",
				i+1, got, runner.SubmitReviewTool)
		}
	}
}
