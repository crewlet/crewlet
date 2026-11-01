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
// discovery pair and the first-party tools, its MCP tools left to discover —
// the shape the onboarding pass's surface has, and a worker's. The executor's
// never does: it holds every tool its seat does from its launch
// ([TestTheExecutorHoldsEveryToolItsSeatDoes]). Its pair is bound to this one
// surface, as [runner.DiscoveryTools] binds every pair.
func discoverySurface(t *testing.T) (*tools.Surface, map[string]tools.Callable) {
	t.Helper()
	_, reg := buildWith(t, discoveryEntries(), buildOpts{})
	var surface *tools.Surface
	snapshot := reg.Snapshot()
	active := []string{"lookup_colleague", "reflect"}
	pair := map[string]tools.Callable{}
	for _, tool := range runner.DiscoveryTools(func() *tools.Surface { return surface }) {
		var err error
		if snapshot, err = snapshot.With(tools.Entry{Tool: tool, Origin: tools.OriginBuiltin}); err != nil {
			t.Fatalf("With(%s): %v", tool.Name(), err)
		}
		active = append(active, tool.Name())
		pair[tool.Name()] = tool
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

// THE PAIR LISTS ONE SERVER'S TOOLS AND ACTIVATES ONE OF THEM, which is the
// whole reason it exists on a phase shown server names rather than tool names:
// the onboarding pass, and a worker. The delivery check's correction for a
// wrong-guessed name on such a phase tells the model to use exactly these two
// tools.
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
	// ONE BULLET PER TOOL, description whole. A real server publishes
	// paragraphs, and the entry that used to be cut to its first line was
	// cut exactly where the usable part starts: "Accepts a cursor for
	// paging" is what a phase needs to call the tool correctly, and this
	// listing is the only place it is ever shown. Continuation lines are
	// indented so the shape survives without paying for it in content.
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
	// A named server with no tools and a wrong server name read very
	// differently to a model: one means "ask again later", the other means
	// "you have the name wrong". Listing what exists turns the second into
	// a recoverable call.
	_, pair := discoverySurface(t)
	reply := ask(t, pair, toolname.ListMCPServerTools, map[string]any{"server": "slacck"}).Output
	if !strings.Contains(reply, "slack") {
		t.Errorf("the failure does not name the servers that exist:\n%s", reply)
	}
}

func TestActivatingAGuessedNameSaysHowToFindTheRealOne(t *testing.T) {
	t.Parallel()
	// A miss is almost always a guessed MCP tool name. Saying so, and
	// naming the way to find the real one, is the difference between a
	// recoverable call and a phase that keeps guessing.
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
	// The discovery pair is per-SURFACE for the same reason the submission
	// tools are: activation mutates one surface, and a shared registry
	// would carry one phase's activation into the next — or into the next
	// turn.
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

func TestDiscoveryRefusesAnEmptyArgument(t *testing.T) {
	t.Parallel()
	// A model that calls a tool with no argument gets a failure it can act
	// on. Without the guard, listing an empty server name reports "no tools
	// found on server """ — which reads as a server that exists and is
	// empty — and activating an empty name reports no tool called "", which
	// says nothing at all.
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
// it no domain tools. activate_tool reaches every tool in the registry, MCP
// writes included, so a reviewer offered the discovery pair could post to the
// thread in the middle of judging whether the executor should have. Every
// phase surface used to get the pair, the reviewer's included. A reviewer that
// asks for activate_tool anyway is refused, and slack_post never reaches it.
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
