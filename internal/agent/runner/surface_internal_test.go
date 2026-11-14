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
	"github.com/crewlet/crewlet/internal/agent/turn"
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

// A TOOL THE SEAT IS WITHHELD IS ON NONE OF ITS SURFACES.
//
// The registry is the epoch's, so a tool only some seats may call — the health
// tools, which a role's grant opens — is registered for every seat, and the
// engine names what this seat's surfaces must leave out (Config.Withheld).
// Every surface is built from the seat's snapshot, so a withheld tool is not
// offered, not listed to discover and not reachable by activation on the
// executor's, the reviewer's or the onboarding pass's (the last two built from
// the seat's snapshot as it is), while every other tool stays.
//
// Mutation: build any of them from the registry whole and the withheld tool is
// one activation away there.
func TestAWithheldToolIsOnNoSurface(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	for _, name := range []string{"fleet_health", "code_status", "lookup_colleague"} {
		if err := reg.Register(stubTool(name), tools.OriginBuiltin); err != nil {
			t.Fatal(err)
		}
	}
	r, err := New(Config{
		Registry: reg,
		Models:   &phase.Registry{},
		Launcher: stubLauncher{},
		Sessions: testHost(t),
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme"}, Role: &org.Role{Name: "Dev"}},
		Withheld: []string{"code_status", "fleet_health"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, snapshot := range map[string]tools.Snapshot{
		"the reviewer's and the onboarding pass's": r.seatSnapshot(),
		"the executor's": r.executorUniverse(r.seatSnapshot()),
	} {
		for _, withheld := range []string{"fleet_health", "code_status"} {
			if _, held := snapshot.Lookup(withheld); held {
				t.Errorf("%s snapshot holds %s, one activation away", name, withheld)
			}
			if strings.Contains(snapshot.Discoverable(nil), withheld) {
				t.Errorf("%s catalogue lists %s", name, withheld)
			}
		}
		if _, held := snapshot.Lookup("lookup_colleague"); !held {
			t.Errorf("%s snapshot lost lookup_colleague, which nothing withholds", name)
		}
	}
	if got := executorActive(r.executorUniverse(r.seatSnapshot()), nil); slices.Contains(got, "fleet_health") {
		t.Errorf("the executor is offered %v, the withheld tool among them", got)
	}
}

// THE DISCOVERY PAIR REACHES A SURFACE PROVEN READ-ONLY, AS IT WAS BUILT.
// The pair never passes through the registry: it carries only the hints
// [DiscoveryTools] gives it, and only if the phase surface adds it as built.
// Listing reads the phase's own universe and activating changes only which of
// its tools the phase is offered, never anything done as the seat, so a
// reader of the hints — a worker's guard, the phase's known reads — that met
// the pair unannotated would take a read for a write it could not classify.
// The executor holds no pair (every tool its seat holds is on its surface from
// its launch), so the surface read here is the onboarding pass's, the phase
// that discovers.
//
// Mutation: rewrap the pair in surfaceWith as a bare builtin entry, or drop
// the hints from DiscoveryTools — both tools read unproven — red.
func TestTheDiscoveryPairIsProvenReadOnlyOnTheSurface(t *testing.T) {
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
	surface, err := r.surfaceWith(context.Background(), r.ownMeter(), phase.Onboarding, 0,
		r.seatSnapshot(), nil, nil)
	if err != nil {
		t.Fatalf("surfaceWith: %v", err)
	}
	for _, name := range toolname.Discovery() {
		if !slices.Contains(surface.Active(), name) {
			t.Errorf("%s is not on the onboarding surface", name)
			continue
		}
		if hints := surface.AnnotationsOf(name); !mcp.ReadOnlyProven(hints) {
			t.Errorf("%s reaches the surface as %+v, not proven read-only", name, hints)
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

// stubLauncher stands for the engine's launch seam. Only its presence is read:
// a runner refuses to be built without one.
type stubLauncher struct{}

func (stubLauncher) LaunchExecutor(context.Context, AgentRunRequest) (Launched, error) {
	return Launched{}, nil
}

// THE OPEN-WORLD HALF OF THE PREDICATE REACHES THE TURN, or it is dead code.
//
// [turn.Acted] decides whether a broken turn's trigger is given up rather than
// redelivered, and its two halves come from different places: a delivery off
// `Deliveries`, and a tool whose own annotations say it leaves the process off
// `KnownOpenWorld` — a colleague ask, a reaction, a published artifact, none of
// them a delivery — less the reads of the world off `KnownReads`. If `describe`
// stops filling either set, the engine silently loses the ability to tell "this
// turn woke a colleague" from "this turn did nothing", and every such failure
// goes back to replaying its ask twenty-five times; or it reads a turn that
// only searched as one that wrote, and gives up a trigger worth retrying.
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
	// A READ OF THE WORLD: open-world, and positively read-only — in BOTH
	// sets, which is what Acted reads the exclusion from.
	if err := reg.RegisterWith(stubTool("search"), tools.OriginBuiltin,
		tools.Annotations{ReadOnly: mcp.Yes, OpenWorld: mcp.Yes}); err != nil {
		t.Fatal(err)
	}
	// The shape that matters most: a tool nobody classified. It must appear
	// in NEITHER list, so it proves nothing in either direction.
	if err := reg.RegisterWith(stubTool("submit_work"), tools.OriginBuiltin,
		tools.Annotations{}); err != nil {
		t.Fatal(err)
	}
	surface := tools.NewSurface("execute", reg.Snapshot(), []string{"ask", "recall", "search", "submit_work"})

	got := describe(surface)
	if !slices.Contains(got.KnownOpenWorld, "ask") {
		t.Errorf("KnownOpenWorld = %v, want the open-world builtin in it", got.KnownOpenWorld)
	}
	if !slices.Contains(got.KnownOpenWorld, "search") || !slices.Contains(got.KnownReads, "search") {
		t.Errorf("KnownOpenWorld = %v, KnownReads = %v; want the read of the world in both",
			got.KnownOpenWorld, got.KnownReads)
	}
	if turn.Acted([]ledger.Call{{Name: "search"}}, got) || !turn.Acted([]ledger.Call{{Name: "ask"}}, got) {
		t.Error("the surface handed to the turn does not tell a read of the world from a colleague ask")
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

// A CALL THE ENGINE ONLY PRESUMES WROTE REACHES THE TURN AS ONE. An MCP tool
// that no hint marks as a read or as a write delivers by the fence's
// fail-closed default, and so counts as an act; the turn must also know which
// calls it counts by that default alone, or a turn nobody asked for that only
// searched through one is told it "already acted outside the engine". A tool
// its server marks a write is an act, and one it marks a read is neither.
//
// Mutation: stop filling PresumedWrites in describe, and the search is told it
// acted.
func TestTheSurfaceHandedToTheTurnSaysWhichWritesItPresumes(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	for name, ann := range map[string]tools.Annotations{
		"jira_search_issues": {},
		"jira_add_comment":   {ReadOnly: mcp.No, OpenWorld: mcp.Yes},
		"jira_get_issue":     {ReadOnly: mcp.Yes},
	} {
		if err := reg.RegisterWith(stubTool(name), tools.Origin("jira"), ann); err != nil {
			t.Fatal(err)
		}
	}
	got := describe(tools.NewSurface("execute", reg.Snapshot(),
		[]string{"jira_search_issues", "jira_add_comment", "jira_get_issue"}))

	if !slices.Equal(got.PresumedWrites, []string{"jira_search_issues"}) {
		t.Errorf("PresumedWrites = %v, want only the tool its server leaves unmarked", got.PresumedWrites)
	}
	searched := turn.ActedAccount([]ledger.Call{{Name: "jira_search_issues"}}, got)
	if !strings.Contains(searched, "does not mark read-only") || strings.Contains(searched, "already acted") {
		t.Errorf("account = %q, want the search named as a call the engine cannot tell from a write",
			searched)
	}
	commented := turn.ActedAccount([]ledger.Call{{Name: "jira_add_comment"}}, got)
	if !strings.Contains(commented, "already acted outside the engine") {
		t.Errorf("account = %q, want the comment its server marks a write named as an act", commented)
	}
	if read := turn.ActedAccount([]ledger.Call{{Name: "jira_get_issue"}}, got); read != "" {
		t.Errorf("account = %q, want nothing for a read its server marks as one", read)
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
// recorded — where the run's box said it took up the session its launch chose,
// as the job's record keeps it — and then every skill the run loaded over the
// bridge. A reopened run and a carried-on run go on calling through that guard;
// the collection calls nothing through it, and is rebuilt the same way because
// a rebuild of its own is how the collection came to drift from the other two.
// And where the box started a session of its own in that one's place, or said
// nothing, the session read none of what the launch recorded: the guard holds
// what the run loaded alone.
//
// Mutation: leave the run's bridged loads out of the collection's rebuild, and
// its guard holds only what the launch recorded; replay a failed load, and it
// holds a skill the run never read; take the launch's record whatever the box
// said, and a session of the run's own holds a skill it never read.
func TestACollectedRunsGuardHoldsWhatTheRunLoaded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		continued bool
		want      []string
	}{
		"its box took the session up":          {continued: true, want: []string{"chat-conventions", "release-notes"}},
		"its box started a session of its own": {want: []string{"chat-conventions"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			collectedGuard(t, tc.continued, tc.want)
		})
	}
}

// collectedGuard collects a run whose launch recorded one skill and which
// loaded another over the bridge — its box having said it took up the session
// its launch chose where continued — and holds its rebuilt guard to want.
func collectedGuard(t *testing.T, continued bool, want []string) {
	t.Helper()
	r := guardedSkillsRunner(t, &Resume{
		Continued: continued,
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
	})
	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := r.loadedSkills(); !slices.Equal(got, want) {
		t.Errorf("the collected run's guard holds %v, want %v", got, want)
	}
}

// guardedSkillsRunner is a CTO seat's runner whose slack_post three required
// skills guard — chat-conventions, release-notes and standups — over the real
// loader's name, resuming resume where it is not nil.
func guardedSkillsRunner(t *testing.T, resume *Resume) *Runner {
	t.Helper()
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
		Resume:   resume,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// A RUN TAKEN OVER IS HELD TO THE SESSION ITS BOX SAYS IT STARTED THERE. A node
// that takes a run over while it works rebuilds its guard as the job's record
// says — the launch's recorded skills only where the box had said it took up the
// session its launch chose; here it had said nothing — and the box, saying it
// on its first request to the reopened bridge, holds the run to that session
// before any call: the launch's skills and the job's own loads where it
// continued the session its launch chose, the job's own loads alone where it
// started one of its own in that one's place, whose model read nothing else.
//
// Mutation: arm the launch's skills whatever the box says, or leave the job's
// own loads out of what the box's word arms, and a case goes red.
func TestARunTakenOverIsHeldToTheSessionItsBoxSaysItStarted(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		continued bool
		want      []string
	}{
		"its box took the session up":          {continued: true, want: []string{"chat-conventions", "release-notes"}},
		"its box started a session of its own": {want: []string{"chat-conventions"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := guardedSkillsRunner(t, nil)
			reopened, err := r.ReopenAgentRun(context.Background(), execstate.State{
				Version: execstate.Version, Round: 1,
				ActiveTools:  []string{"slack_post", toolname.LoadToolSkill},
				LoadedSkills: []string{"release-notes"},
			}, []BridgedCall{
				{Call: ledger.Call{Name: toolname.LoadToolSkill, Args: map[string]any{"key": "chat-conventions"},
					Result: "Always thread your reply."}, Seq: 1},
			}, nil, nil, nil, Bridged{}, false)
			if err != nil {
				t.Fatalf("ReopenAgentRun: %v", err)
			}
			if got := r.loadedSkills(); !slices.Equal(got, []string{"chat-conventions"}) {
				t.Fatalf("before its box says, the taken-over run's guard holds %v, want the job's own "+
					"load alone: its record says nothing of the session it started", got)
			}
			reopened.Started(tc.continued)
			if got := r.loadedSkills(); !slices.Equal(got, tc.want) {
				t.Errorf("once its box says, the taken-over run's guard holds %v, want %v", got, tc.want)
			}
		})
	}
}

// A REBUILT GUARD REPLAYS ITS OWN JOB'S LOADS, NEVER AN EARLIER JOB'S. A round
// can be carried on in a session of its own — its job before lost a second time,
// or its kept session refused — so the job a rebuild re-enters may not be the
// session an earlier job of the round loaded a skill in: its parked state says
// where its own calls begin, and only a load past that is replayed beside the
// skills its launch was armed with.
//
// Mutation: replay every load of the round's calls, and the guard holds a skill
// the run's model never read.
func TestARebuiltGuardReplaysItsOwnJobsLoadsNeverAnEarlierJobs(t *testing.T) {
	t.Parallel()
	registry := skills.NewRegistry()
	for _, s := range []skills.Skill{
		{Key: "chat-conventions", Title: "Chat conventions", Summary: "how to post",
			Trigger: skills.Trigger{Tool: "slack_post"}, Required: true,
			SourcePageID: "chat-conventions-page"},
		{Key: "release-notes", Title: "Release notes", Summary: "how to announce",
			Trigger: skills.Trigger{Tool: "slack_post"}, Required: true,
			SourcePageID: "release-notes-page"},
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
			// THE JOB OPENED ONCE THE ROUND HELD ONE CALL: an earlier job's.
			State: execstate.State{
				Version: execstate.Version, Round: 1, BridgeFrom: 1,
				ActiveTools: []string{"slack_post", toolname.LoadToolSkill},
			},
			Bridged: []BridgedCall{
				{Call: ledger.Call{Name: toolname.LoadToolSkill, Args: map[string]any{"key": "chat-conventions"},
					Result: "Always thread your reply."}, Seq: 1},
				{Call: ledger.Call{Name: toolname.LoadToolSkill, Args: map[string]any{"key": "release-notes"},
					Result: "Announce on the channel."}, Seq: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got, want := r.loadedSkills(), []string{"release-notes"}; !slices.Equal(got, want) {
		t.Errorf("the rebuilt guard holds %v, want its own job's load alone, %v", got, want)
	}
}

// A REBUILT GUARD NAMES A SKILL AS THE RUN'S HARNESS LISTS IT: the names its
// launch handed its skills under are on the parked state, and every rebuild
// of the run's surface — a reopened run's, a carried-on run's, a collection's
// — hands them to its guard, so a call refused after a restart or a seat move
// names the skill the run's model can load, not a key it was never told.
//
// Mutation: rebuild the guard without the parked names, and the refusal sends
// the run to load_tool_skill, which its harness is never listed.
func TestARebuiltGuardNamesASkillAsTheRunsHarnessListsIt(t *testing.T) {
	t.Parallel()
	registry := skills.NewRegistry()
	if _, err := registry.PutPage(skills.Skill{Key: "standups", Title: "Standups",
		Summary: "how to report", Trigger: skills.Trigger{Tool: "slack_post"}, Required: true,
		SourcePageID: "standups-page"}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	reg := tools.NewRegistry()
	for _, name := range []string{"slack_post", toolname.LoadToolSkill} {
		if err := reg.Register(stubTool(name), tools.OriginBuiltin); err != nil {
			t.Fatalf("Register %s: %v", name, err)
		}
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	r, err := New(Config{
		Registry: reg, Models: &phase.Registry{}, Launcher: stubLauncher{},
		Sessions: testHost(t), Skills: registry,
		Seat: prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Resume: &Resume{State: execstate.State{
			Version: execstate.Version, Round: 1,
			ActiveTools:  []string{"slack_post", toolname.LoadToolSkill},
			NativeSkills: map[string]string{"crewlet:standups": "standups"},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	r.mu.Lock()
	guard := r.guard
	r.mu.Unlock()
	blocked := guard.Blocking("slack_post", "")
	if blocked == nil || blocked.Listed != "crewlet:standups" {
		t.Fatalf("the rebuilt guard refuses as %+v, want the skill named crewlet:standups", blocked)
	}
}
