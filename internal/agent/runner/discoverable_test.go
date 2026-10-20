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
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE EXECUTOR'S SYSTEM PROMPT LISTS NOTHING IT HOLDS.
//
// Every first-party tool is on the executor's surface from its first round,
// and an offered tool's whole definition reaches the model with every request
// — its CLI lists it over MCP, from the phase's session in host mode or the
// bridge in box mode. The system prompt used to list each of them again,
// description and all: with every builtin wired that copy was ~17 KB, most of
// the prompt, paid on every round of every turn. What it lists now is what the model can learn
// nowhere else — the MCP servers it can still discover.
//
// REAL BUILTINS, registered by the builtin package itself, so the
// descriptions a regression would duplicate are the ones production sends.
func TestTheExecutorsPromptListsOnlyWhatItCanStillDiscover(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llmtest.Response{submitWork(t)}}
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
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
	})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	run := prov.runsFor("execute")[0]
	system := run.req.Messages[0].Content

	defs := run.conn.Offered()
	if offered := toolNames(defs); !slices.Contains(offered, builtin.SearchKnowledgeTool) {
		t.Fatalf("the real builtin was not offered (%v), so this case proves nothing", offered)
	}
	for _, def := range defs {
		if strings.Contains(system, "- "+def.Name+":") || strings.Contains(system, "- "+def.Name+"\n") {
			t.Errorf("the system prompt lists %s, which the request already defines", def.Name)
		}
		if first, _, _ := strings.Cut(def.Description, "."); len(first) > 30 && strings.Contains(system, first) {
			t.Errorf("the system prompt repeats %s's description: %q", def.Name, first)
		}
	}
	for _, want := range []string{"\n## Tools to discover\n", "- MCP server `slack`", "- MCP server `jira`"} {
		if !strings.Contains(system, want) {
			t.Errorf("the system prompt lacks %q", want)
		}
	}
	// Not a heading of its own for the definitions the CLI already lists.
	if strings.Contains(system, "## Available tools") {
		t.Error("the system prompt has a second `## Available tools` section")
	}

	// AND THE RECORD SAYS THE SAME: what the phase could still discover,
	// disjoint from what it was offered, so a reader counts no tool twice.
	done := completedPhase(t, pub, "execute")
	for _, name := range done.ToolCatalogue {
		if slices.Contains(done.ToolsAvailable, name) {
			t.Errorf("the record lists %s as both offered and still to discover", name)
		}
	}
	if !slices.Contains(done.ToolCatalogue, "slack_post") {
		t.Errorf("the record's tools to discover = %v, want the MCP tool never activated", done.ToolCatalogue)
	}
}

// anySearcher is a knowledge backend that answers nothing, which is all
// registering search_knowledge needs.
type anySearcher struct{}

func (anySearcher) CanSearch(*org.Role, *org.Organization) bool { return true }
func (anySearcher) Search(context.Context, knowledge.Query) knowledge.Result {
	return knowledge.Result{}
}

// THE REVIEW'S RULES ABOUT A TOOL FOLLOW THE SEAT'S SURFACE.
//
// The sandbox rule is for a seat whose executor could have handed code work
// to run_sandbox — the gate enabled and the tool registered, which is exactly
// what puts it in the executor's universe — and `a2a_ask` is named where the
// registry carries it. Unconditional, both were prose about tools on every
// review of every seat in the company, nearly none of which can call them.
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
	both := func(t *testing.T, reg *tools.Registry) {
		t.Helper()
		for _, name := range []string{toolname.RunSandbox, toolname.A2AAsk} {
			if err := reg.Register(stubTool{name: name}, tools.OriginBuiltin); err != nil {
				t.Fatalf("Register: %v", err)
			}
		}
	}

	plain := review(t, buildOpts{})
	for _, unwanted := range []string{"run_sandbox", "a2a_ask"} {
		if strings.Contains(plain, unwanted) {
			t.Errorf("a seat holding neither tool was told about %s", unwanted)
		}
	}

	// Registered, but the seat's gate is off: the executor's universe never
	// held run_sandbox, so there is no sandbox report to judge.
	ungated := review(t, buildOpts{register: both})
	if strings.Contains(ungated, "run_sandbox") {
		t.Error("a seat whose gate is off was told how to judge a sandbox run")
	}
	if !strings.Contains(ungated, "`a2a_ask`") {
		t.Error("a seat holding a2a_ask was not told the next round can use it")
	}

	gated := review(t, buildOpts{register: both, sandbox: &org.RoleSandbox{Enabled: true}})
	if !strings.Contains(gated, "A successful `run_sandbox` call IS the code work") {
		t.Error("a seat offered run_sandbox was not told how to judge its report")
	}
}
