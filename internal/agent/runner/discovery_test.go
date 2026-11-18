package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// discoveryEntries is the provider set the fixture is built on: nothing is
// scripted, because no case here asks a model anything.
func discoveryEntries() []phase.Entry {
	return []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
}

// discoverySurface is the fixture's registry as a surface holding the
// discovery pair and the first-party tools, its MCP tools left to discover:
// the onboarding pass's and a worker's shape, never the executor's
// ([TestTheExecutorHoldsEveryToolItsSeatDoes]). Its pair is bound to this one
// surface, as [runner.DiscoveryTools] binds every pair.
func discoverySurface(t *testing.T) (*tools.Surface, map[string]tools.Callable) {
	t.Helper()
	_, reg := buildWith(t, discoveryEntries(), buildOpts{})
	var surface *tools.Surface
	snapshot := reg.Snapshot()
	active := []string{"lookup_colleague", "reflect"}
	pair := map[string]tools.Callable{}
	for _, entry := range runner.DiscoveryTools(func() *tools.Surface { return surface }) {
		var err error
		if snapshot, err = snapshot.With(entry); err != nil {
			t.Fatalf("With(%s): %v", entry.Name(), err)
		}
		active = append(active, entry.Name())
		pair[entry.Name()] = entry.Tool
	}
	surface = tools.NewSurface("onboarding", snapshot, active)
	return surface, pair
}

// ask calls one of a surface's discovery tools.
func ask(t *testing.T, pair map[string]tools.Callable, name string, args map[string]any) tools.Result {
	t.Helper()
	res, err := pair[name].Call(context.Background(), args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

// THE PAIR LISTS ONE SERVER'S TOOLS AND ACTIVATES ONE OF THEM, on a phase
// shown server names rather than tool names (the onboarding pass, a worker),
// whose delivery-check correction for a guessed name points at these two.
func TestTheDiscoveryPairListsOneServerAndActivatesOneTool(t *testing.T) {
	t.Parallel()
	surface, pair := discoverySurface(t)
	if slices.Contains(toolNames(surface.ToolDefs()), "slack_post") {
		t.Errorf("the tool was offered before it was activated: %v", toolNames(surface.ToolDefs()))
	}
	listing := ask(t, pair, toolname.ListMCPServerTools, map[string]any{"server": "slack"}).Output
	if !strings.Contains(listing, "slack_post") || !strings.Contains(listing, "slack_history") {
		t.Errorf("the listing did not name the server's tools:\n%s", listing)
	}
	if strings.Contains(listing, "lookup_colleague") {
		t.Errorf("the listing leaked a first-party tool:\n%s", listing)
	}
	// ONE BULLET PER TOOL, description whole: past the first line is what a
	// phase needs to call it ("Accepts a cursor for paging"), and this listing
	// is the only place it is shown. Continuation lines are indented.
	if !strings.Contains(listing, "\n  Accepts a cursor") {
		t.Errorf("a description was cut, or its continuation line not indented:\n%s", listing)
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
	// ONE server's tools, not every MCP tool.
	if strings.Contains(listing, "jira_create") {
		t.Errorf("the listing for slack included another server's tools:\n%s", listing)
	}

	if res := ask(t, pair, toolname.Activate, map[string]any{"name": "slack_post"}); res.Failed {
		t.Fatalf("the activation failed: %s", res.Output)
	}
	// The activation reached the surface the pair is bound to.
	if !slices.Contains(toolNames(surface.ToolDefs()), "slack_post") {
		t.Errorf("the activated tool is not on the surface: %v", toolNames(surface.ToolDefs()))
	}
}

func TestAnUnknownServerListsTheOnesThatExist(t *testing.T) {
	t.Parallel()
	// A wrong server name is answered with the servers that exist, so the
	// call is recoverable.
	_, pair := discoverySurface(t)
	reply := ask(t, pair, toolname.ListMCPServerTools, map[string]any{"server": "slacck"}).Output
	if !strings.Contains(reply, "slack") {
		t.Errorf("the failure does not name the servers that exist:\n%s", reply)
	}
}

func TestActivatingAGuessedNameSaysHowToFindTheRealOne(t *testing.T) {
	t.Parallel()
	// A miss is almost always a guessed MCP tool name: the answer names the
	// way to the real one.
	_, pair := discoverySurface(t)
	reply := ask(t, pair, toolname.Activate, map[string]any{"name": "slack_send_msg"}).Output
	if !strings.Contains(reply, "list_mcp_server_tools") {
		t.Errorf("the failure does not say how to find the real name:\n%s", reply)
	}
	if !strings.Contains(reply, "slack_send_msg") {
		t.Errorf("the failure does not name what was tried:\n%s", reply)
	}
}

func TestOneSurfacesActivationDoesNotLeakIntoAnother(t *testing.T) {
	t.Parallel()
	// The pair is per-SURFACE: in a shared registry one phase's activation
	// would carry into the next phase or turn.
	first, firstPair := discoverySurface(t)
	second, _ := discoverySurface(t)
	ask(t, firstPair, toolname.Activate, map[string]any{"name": "slack_history"})
	if !slices.Contains(toolNames(first.ToolDefs()), "slack_history") {
		t.Errorf("the activation did not reach its own surface: %v", toolNames(first.ToolDefs()))
	}
	if slices.Contains(toolNames(second.ToolDefs()), "slack_history") {
		t.Errorf("an activation leaked into another surface: %v", toolNames(second.ToolDefs()))
	}
}

// THE DISCOVERY TOOLS ARE EXACTLY THE PAIR internal/toolname NAMES, as a set:
// the skill guard, the worker deny list, the ledger and the prompts all read
// that list, so a third tool registered here alone would be guarded,
// inherited, recorded and never mentioned. Mutation: a third tool in
// DiscoveryTools, or a name changed on one of the two, is red.
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

func TestDiscoveryRefusesAnEmptyArgument(t *testing.T) {
	t.Parallel()
	// A call with no argument gets a failure it can act on, not "no tools
	// found on server """ (an existing, empty server) or no tool called "".
	_, pair := discoverySurface(t)
	listed := ask(t, pair, toolname.ListMCPServerTools, map[string]any{"server": "  "}).Output
	if !strings.Contains(listed, "Name a server") {
		t.Errorf("an empty server name reported %q", listed)
	}
	// It still says which servers exist, so the call is recoverable.
	if !strings.Contains(listed, "slack") {
		t.Errorf("the refusal does not name the servers that exist: %q", listed)
	}
	if got := ask(t, pair, toolname.Activate, map[string]any{}).Output; !strings.Contains(got, "Name a tool") {
		t.Errorf("an empty tool name reported %q", got)
	}
}

// THE REVIEWER CANNOT DISCOVER A TOOL.
//
// It grades the round from the evidence in front of it, and its prompt promises
// it no domain tools; activate_tool reaches every registered tool, MCP writes
// included, so a reviewer holding it could post to the thread while judging
// whether the executor should have. A reviewer that asks for activate_tool
// anyway is refused, and slack_post never reaches it.
func TestTheReviewerIsOfferedOnlyItsSubmission(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llmtest.Response{
			calling(callOf(t, toolname.Activate, map[string]any{"name": "slack_post"})),
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
	// Refused BY ITS HARNESS: a name the reviewer was never offered never reaches
	// its tools at all.
	if got := prov.answerTo("review", toolname.Activate); !strings.Contains(got, "No such tool") {
		t.Errorf("the reviewer's activation was answered %q, want its harness's refusal", got)
	}
	for i, got := range offered {
		if !slices.Equal(got, []string{runner.SubmitReviewTool}) {
			t.Errorf("review call %d offered %v, want only %s: the discovery pair "+
				"puts every tool in the registry within the reviewer's reach",
				i+1, got, runner.SubmitReviewTool)
		}
	}
}
