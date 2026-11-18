package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE EXECUTOR'S SYSTEM PROMPT LISTS NOTHING IT HOLDS, AND IT HOLDS EVERY TOOL.
//
// Every tool, first-party and MCP, is on the executor's surface from launch,
// and its whole definition reaches the run over the tool bridge, under its
// server; the harness keeps a large server's tools out of requests that do not
// need them. A copy in the prompt (~17 KB with every builtin wired) would be
// paid on every turn of every run.
//
// REAL BUILTINS, registered by the builtin package itself, so the
// descriptions a regression would duplicate are the ones production sends.
func TestTheExecutorsPromptListsNothingItHolds(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	got := executeRound(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
		pub: pub,
		register: func(t *testing.T, reg *tools.Registry) {
			t.Helper()
			shipped := tools.NewRegistry()
			if _, err := builtin.Register(shipped, builtin.Deps{
				Knowledge: anySearcher{},
				Authorize: builtin.Decide(authz.NoChart{}),
			}); err != nil {
				t.Fatalf("builtin.Register: %v", err)
			}
			held := reg.Snapshot()
			for _, e := range shipped.Snapshot().Entries() {
				if _, taken := held.Lookup(e.Name()); taken {
					continue
				}
				if err := reg.RegisterWith(e.Tool, e.Origin, e.Annotations); err != nil {
					t.Fatalf("Register(%s): %v", e.Name(), err)
				}
			}
		},
	}, 1, "", nil, run{calls: []llm.ToolCall{submitWork(t)}}, runner.RunRecord{})
	system := got.launch.System
	defs := got.launch.Surface.ToolDefs()

	offered := toolNames(defs)
	for _, held := range []string{builtin.SearchKnowledgeTool, "slack_post", "jira_create"} {
		if !slices.Contains(offered, held) {
			t.Fatalf("%s was not offered (%v)", held, offered)
		}
	}
	for _, def := range defs {
		if strings.Contains(system, "- "+def.Name+":") || strings.Contains(system, "- "+def.Name+"\n") {
			t.Errorf("the system prompt lists %s, which the request already defines", def.Name)
		}
		if first, _, _ := strings.Cut(def.Description, "."); len(first) > 30 && strings.Contains(system, first) {
			t.Errorf("the system prompt repeats %s's description: %q", def.Name, first)
		}
	}
	for _, unwanted := range []string{"## Tools to discover", "MCP server `slack`",
		"MCP server `jira`", "## Available tools"} {
		if strings.Contains(system, unwanted) {
			t.Errorf("the system prompt carries %q, which the request already defines", unwanted)
		}
	}

	// AND THE RECORD SAYS THE SAME: every tool held, nothing left to discover.
	done := completedPhase(t, pub, "execute")
	if !slices.Contains(done.ToolsAvailable, "slack_post") {
		t.Errorf("the record's tools held = %v, want the MCP tool among them", done.ToolsAvailable)
	}
	if len(done.ToolCatalogue) != 0 {
		t.Errorf("the record lists %v as still to discover, on a run that held every tool",
			done.ToolCatalogue)
	}
}

// anySearcher is a knowledge backend that answers nothing, which is all
// registering search_knowledge needs.
type anySearcher struct{}

func (anySearcher) CanSearch(*org.Role, *org.Organization) bool { return true }
func (anySearcher) Search(context.Context, knowledge.Query) knowledge.Result {
	return knowledge.Result{}
}

// THE REVIEW'S RULE ABOUT A TOOL FOLLOWS THE SEAT'S SURFACE: `a2a_ask` is
// named only where the registry carries it, not on every seat's review.
func TestTheReviewNamesOnlyTheToolsTheSeatHolds(t *testing.T) {
	t.Parallel()
	review := func(t *testing.T, opts buildOpts) string {
		t.Helper()
		prov := &scriptedProvider{review: []llmtest.Response{
			submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)}}
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, opts)
		if _, err := r.Review(context.Background(), 1, turn.Work{Summary: "s"}, nil); err != nil {
			t.Fatalf("Review: %v", err)
		}
		return prov.requestsFor("review")[0].Messages[0].Content
	}
	asker := func(t *testing.T, reg *tools.Registry) {
		t.Helper()
		if err := reg.Register(stubTool{name: toolname.A2AAsk}, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	if plain := review(t, buildOpts{}); strings.Contains(plain, "a2a_ask") {
		t.Error("a seat not holding a2a_ask was told about it")
	}
	if held := review(t, buildOpts{register: asker}); !strings.Contains(held, "`a2a_ask`") {
		t.Error("a seat holding a2a_ask was not told the next round can use it")
	}
}
