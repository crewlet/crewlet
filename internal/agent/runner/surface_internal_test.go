package runner

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE EXECUTOR NEVER REACHES mark_onboarded, by activation or by catalogue.
//
// A seat that marked itself onboarded from inside the executor would skip
// orientation for ever. It was kept off the executor's active list only, so
// the catalogue the prompt rendered listed it and activate_tool reached it.
//
// Mutation: return the snapshot whole from executorUniverse, and this goes
// red.
func TestTheExecutorCannotReachThePhaseScopedTools(t *testing.T) {
	t.Parallel()
	r, err := New(Config{
		Registry: tools.NewRegistry(),
		Models:   &phase.Registry{},
		Launcher: stubLauncher{},
		Sessions: testHost(t),
		Seat: prompts.Seat{Org: &org.Organization{Name: "Acme"},
			Role: &org.Role{Name: "Engineer"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := tools.NewRegistry().Snapshot().
		With(tools.Entry{Tool: stubTool(toolname.MarkOnboarded), Origin: tools.OriginBuiltin})
	if err != nil {
		t.Fatal(err)
	}
	universe := r.executorUniverse(snapshot)
	if _, held := universe.Lookup(toolname.MarkOnboarded); held {
		t.Error("mark_onboarded is in the executor's snapshot, one activation away")
	}
	if strings.Contains(universe.Discoverable(nil), toolname.MarkOnboarded) {
		t.Error("the executor's catalogue lists mark_onboarded")
	}
	if slices.Contains(executorActive(universe, nil), toolname.MarkOnboarded) {
		t.Error("the executor's run is offered mark_onboarded")
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

// stubLauncher stands for the engine's launch seam. Only its presence is read:
// a runner refuses to be built without one.
type stubLauncher struct{}

func (stubLauncher) LaunchExecutor(context.Context, AgentRunRequest) (Launched, error) {
	return Launched{}, nil
}

// THE OPEN-WORLD HALF OF THE PREDICATE REACHES THE TURN, or it is dead code.
//
// [turn.Acted] decides whether a broken turn's trigger is given up rather than
// redelivered, and its two halves come from different places: MCP-backing off
// `MCPTools`, and a first-party tool that leaves the process off
// `KnownOpenWorld`. Only `a2a_ask` carries that annotation among the builtins, so
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
//
// AND A SURFACE WITH NO WAY TO DISCOVER HAS NOTHING BEHIND DISCOVERY: the
// executor's, which holds every tool it can reach, and whose refusals once
// sent it to a pair of tools it does not hold.
//
// Mutation: report the universe's MCP tools whatever the surface holds, and the
// executor's surface says it has tools to discover.
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
	snapshot, err := reg.Snapshot().With(tools.Entry{Tool: stubTool(toolname.Activate),
		Origin: tools.OriginBuiltin})
	if err != nil {
		t.Fatal(err)
	}
	surface := tools.NewSurface("onboarding", snapshot,
		[]string{"comment_on_work_item", toolname.Activate})

	got := describe(surface)
	if !slices.Contains(got.Discoverable, "mattermost_post_message") {
		t.Errorf("Discoverable = %v, want the MCP tool in it", got.Discoverable)
	}
	if slices.Contains(got.Discoverable, "comment_on_work_item") {
		t.Error("a first-party tool the phase already holds was reported as one " +
			"it has to discover")
	}

	held := tools.NewSurface("execute", reg.Snapshot(),
		[]string{"comment_on_work_item", "mattermost_post_message"})
	if got := describe(held).Discoverable; len(got) != 0 {
		t.Errorf("Discoverable = %v on a surface that holds every tool and no way to discover one",
			got)
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

// A COLLECTED RUN'S GUARD HOLDS WHAT THE RUN LOADED. A collection rebuilds the
// run's surface as the run left it, guard included, by the one rebuild every
// segment that re-enters a run makes (rebuiltSurface): the skills the launch
// recorded, and then every skill the run loaded over the bridge. A
// reopened run and a carried-on run go on calling through that guard; the
// collection calls nothing through it, and is rebuilt the same way because a
// rebuild of its own is how the collection came to drift from the other two.
//
// Mutation: leave the run's bridged loads out of the collection's rebuild, and
// its guard holds only what the launch recorded; replay a failed load, and it
// holds a skill the run never read.
func TestACollectedRunsGuardHoldsWhatTheRunLoaded(t *testing.T) {
	t.Parallel()
	registry := skills.NewRegistry()
	for _, s := range []skills.Skill{
		{Key: "chat-conventions", Title: "Chat conventions", Summary: "how to post",
			Trigger: skills.Trigger{Tool: "slack_post"}, Required: true,
			SourcePageID: "chat-conventions-page"},
		{Key: "release-notes", Title: "Release notes", Summary: "how to announce",
			Trigger: skills.Trigger{Tool: "slack_post"}, Required: true,
			SourcePageID: "release-notes-page"},
		{Key: "standups", Title: "Standups", Summary: "how to report",
			Trigger: skills.Trigger{Tool: "slack_post"}, Required: true,
			SourcePageID: "standups-page"},
	} {
		if _, err := registry.PutPage(s); err != nil {
			t.Fatalf("PutPage %s: %v", s.Key, err)
		}
	}
	reg := tools.NewRegistry()
	for _, name := range []string{"slack_post", toolname.LoadToolSkill} {
		if err := reg.Register(stubTool(name), tools.OriginBuiltin); err != nil {
			t.Fatalf("Register %s: %v", name, err)
		}
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	r, err := New(Config{
		Registry: reg,
		Models:   &phase.Registry{},
		Launcher: stubLauncher{},
		Sessions: testHost(t),
		Skills:   registry,
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Resume: &Resume{
			State: execstate.State{
				Version: execstate.Version, Round: 1,
				ActiveTools:  []string{"slack_post", toolname.LoadToolSkill},
				LoadedSkills: []string{"release-notes"},
			},
			Bridged: []BridgedCall{
				{Call: ledger.Call{Name: toolname.LoadToolSkill, Args: map[string]any{"key": "chat-conventions"},
					Result: "Always thread your reply."}, Seq: 1},
				{Call: ledger.Call{Name: toolname.LoadToolSkill, Args: map[string]any{"key": "standups"},
					Result: "no such skill", Failed: true}, Seq: 2},
				{Call: ledger.Call{Name: "slack_post", Args: map[string]any{"channel": "C1"},
					Result: "posted"}, Seq: 3},
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got, want := r.loadedSkills(), []string{"chat-conventions", "release-notes"}; !slices.Equal(got, want) {
		t.Errorf("the collected run's guard holds %v, want what the launch recorded and what "+
			"the run loaded over the bridge, %v", got, want)
	}
}
