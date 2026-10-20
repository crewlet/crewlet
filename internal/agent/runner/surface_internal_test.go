package runner

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// A SEAT IS NEVER SHOWN A TOOL THAT WOULD ONLY REFUSE IT.
//
// run_sandbox is registered per EPOCH — the engine wires one launcher for the
// whole company — so every seat's registry carries it the moment any seat can
// run code. Two seats can never use it, and before this filter both learned so
// by calling it: a round spent, on a tool the catalogue vouched for, and
// sometimes a whole turn planned around a box that was never coming.
//
// Out of the UNIVERSE, not only the active list: a surface activates anything
// in its snapshot by name, and the catalogue is read off the same snapshot, so
// a tool kept off the active list alone was still listed and one call away.
//
// Mutate `offersSandbox` to return true and the first three cases go red.
func TestExecutorSurfaceHidesRunSandboxFromSeatsThatCannotUseIt(t *testing.T) {
	t.Parallel()

	gated := &org.Role{Name: "Engineer", Sandbox: &org.RoleSandbox{Enabled: true}}
	ungated := &org.Role{Name: "Chief of Staff"}
	offGate := &org.Role{Name: "Analyst", Sandbox: &org.RoleSandbox{Enabled: false}}

	cases := []struct {
		name     string
		seat     *org.Role
		agentRun AgentLauncher
		want     bool
	}{
		{
			name: "no gate at all — the launcher would answer " +
				"\"this seat's sandbox is not enabled\"",
			seat: ungated,
		},
		{
			name: "a gate that is off — same refusal, and the operator " +
				"turned it off on purpose",
			seat: offGate,
		},
		{
			name: "a box-mode executor already holds a shell, so a " +
				"second box would put the work where the turn cannot see it",
			seat:     gated,
			agentRun: stubLauncher{},
			want:     false,
		},
		{
			name: "a gated seat in host mode is the one that can " +
				"actually launch one",
			seat: gated,
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := New(Config{
				Registry: tools.NewRegistry(),
				Models:   &phase.Registry{},
				Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme"}, Role: tc.seat},
				AgentRun: tc.agentRun,
				Sessions: testHost(t),
			})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := tools.NewRegistry().Snapshot().
				With(tools.Entry{Tool: stubTool(toolname.RunSandbox), Origin: tools.OriginBuiltin})
			if err != nil {
				t.Fatal(err)
			}
			universe := r.executorUniverse(snapshot, tc.agentRun != nil)
			if got := slices.Contains(executorActive(universe), toolname.RunSandbox); got != tc.want {
				t.Errorf("run_sandbox offered = %v, want %v", got, tc.want)
			}
			if _, got := universe.Lookup(toolname.RunSandbox); got != tc.want {
				t.Errorf("run_sandbox reachable by activation = %v, want %v", got, tc.want)
			}
			if got := strings.Contains(universe.Discoverable(nil), toolname.RunSandbox); got != tc.want {
				t.Errorf("run_sandbox in the catalogue = %v, want %v", got, tc.want)
			}
		})
	}
}

// THE EXECUTOR NEVER REACHES mark_onboarded, by activation or by catalogue.
//
// A seat that marked itself onboarded from inside the executor would skip
// orientation for ever. It was kept off the executor's active list only, so
// the catalogue the prompt rendered listed it and activate_tool reached it.
// Every executor, gated or not and on either runtime, is held to it.
func TestTheExecutorCannotReachThePhaseScopedTools(t *testing.T) {
	t.Parallel()
	for _, agentRun := range []bool{false, true} {
		r, err := New(Config{
			Registry: tools.NewRegistry(),
			Models:   &phase.Registry{},
			Seat: prompts.Seat{Org: &org.Organization{Name: "Acme"},
				Role: &org.Role{Name: "Engineer", Sandbox: &org.RoleSandbox{Enabled: true}}},
			Sessions: testHost(t),
		})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := tools.NewRegistry().Snapshot().
			With(tools.Entry{Tool: stubTool(toolname.MarkOnboarded), Origin: tools.OriginBuiltin})
		if err != nil {
			t.Fatal(err)
		}
		universe := r.executorUniverse(snapshot, agentRun)
		if _, held := universe.Lookup(toolname.MarkOnboarded); held {
			t.Errorf("box mode %v: mark_onboarded is in the executor's snapshot, one activation away", agentRun)
		}
		if strings.Contains(universe.Discoverable(nil), toolname.MarkOnboarded) {
			t.Errorf("box mode %v: the executor's catalogue lists mark_onboarded", agentRun)
		}
	}
}

// stubTool is a name on the surface and nothing else — what is under test is
// which names survive the filter, not what any of them do.
type stubTool string

func (s stubTool) Name() string               { return string(s) }
func (s stubTool) Description() string        { return "stub" }
func (s stubTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (s stubTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{}, nil
}

// stubLauncher stands for "this executor runs in box mode". Only its
// presence is read.
type stubLauncher struct{}

func (stubLauncher) LaunchExecutor(context.Context, AgentRunRequest) error { return nil }

// THE OPEN-WORLD HALF OF THE PREDICATE REACHES THE TURN, or it is dead code.
//
// [turn.Acted] decides whether a broken turn's trigger is given up rather than
// redelivered, and its two halves come from different places: MCP-backing off
// `MCPTools`, and a first-party tool that leaves the process off
// `KnownOpenWorld`. Only `a2a_ask` and `run_sandbox` carry that annotation, so
// if `describe` stops filling the field the engine silently loses the ability
// to tell "this turn woke a colleague" from "this turn did nothing" — and
// every such failure goes back to replaying its ask twenty-five times.
func TestTheSurfaceHandedToTheTurnCarriesWhatLeavesTheProcess(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	if err := reg.RegisterWith(stubTool("ask"), tools.OriginBuiltin,
		tools.Annotations{ReadOnly: mcp.No, OpenWorld: mcp.Yes}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterWith(stubTool("recall"), tools.OriginBuiltin,
		tools.Annotations{ReadOnly: mcp.Yes}); err != nil {
		t.Fatal(err)
	}
	// The shape that matters most: a tool nobody classified. It must appear
	// in NEITHER list, so it proves nothing in either direction.
	if err := reg.RegisterWith(stubTool("submit_work"), tools.OriginBuiltin,
		tools.Annotations{}); err != nil {
		t.Fatal(err)
	}
	surface := tools.NewSurface("execute", reg.Snapshot(), []string{"ask", "recall", "submit_work"})

	got := describe(surface)
	if !slices.Contains(got.KnownOpenWorld, "ask") {
		t.Errorf("KnownOpenWorld = %v, want the open-world builtin in it", got.KnownOpenWorld)
	}
	if slices.Contains(got.KnownOpenWorld, "recall") {
		t.Error("a positively read-only tool was reported as leaving the process")
	}
	if slices.Contains(got.KnownOpenWorld, "submit_work") {
		t.Error("an UNANNOTATED tool was reported as leaving the process — the " +
			"whole point of reading the annotation positively is that it cannot")
	}
	if !slices.Contains(got.KnownReads, "recall") {
		t.Errorf("KnownReads = %v, want the read in it", got.KnownReads)
	}
}

// WHAT A SEAT HOLDS AND WHAT IT HAS TO GO AND FIND reach the turn too, or a
// delivery refusal cannot tell them apart. It once could not: every refusal
// sent the seat to `list_mcp_server_tools`, and a seat woken on Worklet
// (whose tools no discovery call lists) went to the only MCP
// server it held and posted in chat instead. See [turn.Remedy].
func TestTheSurfaceHandedToTheTurnSaysWhichToolsSitBehindDiscovery(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	if err := reg.RegisterWith(stubTool("comment_on_work_item"), tools.OriginBuiltin,
		tools.Annotations{}, tools.DeliversTo("work")); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterWith(stubTool("mattermost_post_message"), tools.Origin("mattermost"),
		tools.Annotations{}); err != nil {
		t.Fatal(err)
	}
	surface := tools.NewSurface("execute", reg.Snapshot(), []string{"comment_on_work_item"})

	got := describe(surface)
	if !slices.Contains(got.Discoverable, "mattermost_post_message") {
		t.Errorf("Discoverable = %v, want the MCP tool in it", got.Discoverable)
	}
	if slices.Contains(got.Discoverable, "comment_on_work_item") {
		t.Error("a first-party tool the executor already holds was reported as one " +
			"it has to discover")
	}
}

// A GUARD IS ARMED ONLY WHERE ITS LOADER IS ACTIVE. The triggers are matched
// over the surface's whole universe, so the catalogue and the guard agree on
// an MCP tool the executor activates later — but a loader that sits in the
// universe without being active is one the phase's model may never reach, and
// a guard armed there would refuse a covered tool it had no way to unlock.
//
// Drop the active-list check in guardFor and the first case goes red.
func TestASkillGuardNeedsItsLoaderActive(t *testing.T) {
	t.Parallel()
	registry := skills.NewRegistry()
	if _, err := registry.PutPage(skills.Skill{
		Key: "chat-conventions", Title: "Chat conventions", Summary: "how to post",
		Trigger: skills.Trigger{Tool: "slack_post"}, Required: true,
		SourcePageID: "chat-conventions-page",
	}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	reg := tools.NewRegistry()
	for _, name := range []string{"slack_post", toolname.LoadToolSkill} {
		if err := reg.Register(stubTool(name), tools.OriginBuiltin); err != nil {
			t.Fatalf("Register %s: %v", name, err)
		}
	}
	r := &Runner{cfg: Config{Skills: registry, Seat: prompts.Seat{Role: &org.Role{Name: "CTO", DeclaredHandle: "cto"}}}}

	inactive := tools.NewSurface(phase.Execute.String(), reg.Snapshot(), []string{"slack_post"})
	if g := r.guardFor(phase.Execute, 1, inactive, nil); g.guard != nil {
		t.Error("a guard was armed over a loader the phase never activated")
	}
	active := tools.NewSurface(phase.Execute.String(), reg.Snapshot(), []string{toolname.LoadToolSkill})
	if g := r.guardFor(phase.Execute, 1, active, nil); g.guard == nil {
		t.Error("no guard was armed although the loader is active and slack_post is in the universe")
	}
}
