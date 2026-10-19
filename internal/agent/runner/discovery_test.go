package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

func TestAPhaseCanDiscoverAndActivateAnMCPTool(t *testing.T) {
	t.Parallel()
	// The whole reason discovery exists: the executor is shown SERVER names,
	// not tool names, and the delivery check's own correction for a
	// wrong-guessed name tells the model to use exactly these two tools. If
	// they are not on the surface, that advice is a lie.
	r, prov, _ := fixture(t, &scriptedProvider{
		execute: []llmtest.Response{
			calling(callOf(t, "list_mcp_server_tools", map[string]any{"server": "slack"})),
			calling(callOf(t, "activate_tool", map[string]any{"name": "slack_post"})),
			submitWork(t),
		},
	})
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The fixture activates but never posts, so the honest outcome is
	// blocked — the subject here is the discovery round trip, not delivery.
	if w.Outcome != turn.OutcomeBlocked {
		t.Fatalf("outcome = %s", w.Outcome)
	}

	offered := prov.offeredFor("execute")
	if len(offered) < 3 {
		t.Fatalf("the executor made %d model requests, want the discovery round trip", len(offered))
	}
	// The listing named the server's real tools — what the model read back
	// from the call.
	listing := prov.answerTo("execute", "list_mcp_server_tools")
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
	// And the activation reached the surface: the tool is offered on the
	// round AFTER it, which is what makes activation worth anything.
	if !slices.Contains(offered[2], "slack_post") {
		t.Errorf("the activated tool was not offered next round: %v", offered[2])
	}
	if slices.Contains(offered[0], "slack_post") {
		t.Errorf("the tool was offered before it was activated: %v", offered[0])
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
	// a recoverable round.
	r, prov, _ := fixture(t, &scriptedProvider{
		execute: []llmtest.Response{
			calling(callOf(t, "list_mcp_server_tools", map[string]any{"server": "slacck"})),
			submitWork(t),
		},
	})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	reply := prov.answerTo("execute", "list_mcp_server_tools")
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
		execute: []llmtest.Response{
			calling(callOf(t, "activate_tool", map[string]any{"name": "slack_send_msg"})),
			submitWork(t),
		},
	})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	reply := prov.answerTo("execute", "activate_tool")
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
	// turn. A self_iterate builds a fresh surface, which is correct: its
	// LLM context started over too.
	r, prov, _ := fixture(t, &scriptedProvider{
		execute: []llmtest.Response{
			calling(callOf(t, "activate_tool", map[string]any{"name": "slack_history"})),
			submitWork(t),
		},
	})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, _, err := r.Execute(context.Background(), 2, "try again", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	offered := prov.offeredFor("execute")
	// Round 1 activated it, so round 1's LAST request has it...
	if !slices.Contains(offered[1], "slack_history") {
		t.Errorf("the activation did not reach its own surface: %v", offered[1])
	}
	// ...and the next turn round starts clean.
	last := offered[len(offered)-1]
	if slices.Contains(last, "slack_history") {
		t.Errorf("an activation leaked into the next round: %v", last)
	}
	if !slices.Contains(last, runner.SubmitWorkTool) {
		t.Errorf("the next round lost its own tools: %v", last)
	}
}

// THE DISCOVERY TOOLS ARE EXACTLY THE PAIR internal/toolname NAMES, as a set:
// the skill guard exempts that list, the worker deny list keeps a child off
// it, the ledger skips it and the prompts tell a model to call it, so a third
// discovery tool registered here and not there would be guarded, inherited,
// recorded and never mentioned. Mutation: a third tool in DiscoveryTools, or
// a name changed on one of the two, is red.
func TestTheDiscoveryToolsAreExactlyTheNamedPair(t *testing.T) {
	t.Parallel()
	var got []string
	for _, tool := range runner.DiscoveryTools(func() *tools.Surface { return nil }) {
		got = append(got, tool.Name())
	}
	want := toolname.Discovery()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the runner registers the discovery tools %v, and the list every "+
			"other package reads is %v", got, want)
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
		execute: []llmtest.Response{
			calling(callOf(t, "list_mcp_server_tools", map[string]any{"server": "  "}),
				callOf(t, "activate_tool", map[string]any{})),
			submitWork(t),
		},
	})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	replies := map[string]string{}
	for _, r := range prov.answered("execute") {
		replies[r.Name] = r.Output
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
		execute: []llmtest.Response{submitWork(t)},
		review: []llmtest.Response{
			calling(callOf(t, toolname.Activate, map[string]any{"name": "slack_post"})),
			submitCall(t, runner.SubmitReviewTool, `{"decision":"done","final_artifact":"a"}`),
		},
	})
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := r.Review(context.Background(), 1, w, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	offered := prov.offeredFor("review")
	if len(offered) < 2 {
		t.Fatalf("the reviewer made %d model requests, want the refused activation and its answer",
			len(offered))
	}
	// Refused BY ITS CLI: a name the reviewer was never offered never reaches
	// its tools at all.
	if got := prov.answerTo("review", toolname.Activate); !strings.Contains(got, "No such tool") {
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
