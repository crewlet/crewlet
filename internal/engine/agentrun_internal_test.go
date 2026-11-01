package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	seatpkg "github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/tools"
)

// chainMember is one entry of a fixture's executor chain: its key, its harness, the
// model it names and, for an entry whose plan is spent, that it sits out.
type chainMember struct {
	key, agent, model string
	runIn             config.Placement
	// subscription signs the entry in with a plan login that never leaves
	// the engine host, rather than with an API key that travels.
	subscription bool
	// spent sits the entry out on this node before the launch, as a text
	// call that met the harness's usage-limit line leaves it.
	spent bool
	// env is the entry's env, as resolved.
	env map[string]string
}

// harnessProvider is a REAL harness provider for m: the question every case here
// asks is what the provider's own answer makes the launch do, and a fake would
// answer whatever the case wanted. Its binary does not exist, so no case can
// start a harness on the machine running the suite — except an entry that has to
// sit out, whose stand-in prints the harness's own usage-limit line once.
func harnessProvider(t *testing.T, m chainMember) *harness.Provider {
	t.Helper()
	overrides := map[string]any{"binary": filepath.Join(t.TempDir(), "no-cli-here")}
	auth := harness.Auth{Mode: harness.AuthAPIKey, APIKey: "sk-ant-not-real"}
	if m.subscription {
		auth = harness.Auth{Mode: harness.AuthSubscription}
	}
	if m.spent {
		script := filepath.Join(t.TempDir(), "spent-cli")
		if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\n"+
			"echo 'Usage limit reached · resets 5pm'\n"), 0o755); err != nil {
			t.Fatalf("writing the spent harness: %v", err)
		}
		overrides = map[string]any{
			"binary": script, "complete_args": []any{"run"},
			"output": "text", "prompt_mode": "stdin",
			// A text stream has no events to tell apart, so the profile's
			// event filter goes with it — and so does everything else that
			// reads such a stream: its requests, its MCP failure events, its
			// structured limit and sign-in reports, the cap's signature in it.
			"event_type_path": []any{}, "text_events": []any{},
			"requests": nil, "cap": nil, "limit_events": nil,
			"limit_reset_events": nil, "auth_events": nil,
			"mcp": map[string]any{"failure_events": []any{}},
		}
	}
	provider, err := harness.New(harness.Config{
		Key: m.key, Harness: m.agent, Model: m.model, StateDir: t.TempDir(),
		Overrides: overrides, Timeout: time.Minute, Auth: auth, Env: m.env,
	})
	if err != nil {
		t.Fatalf("harness.New(%s): %v", m.key, err)
	}
	if m.spent {
		_, err := provider.Complete(t.Context(), llm.Request{Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "are you there"},
		}})
		if llm.KindOf(err) != llm.KindRateLimit || provider.SittingOut() == nil {
			t.Fatalf("the spent stand-in answered %v and sits out %v: the fixture could not "+
				"spend the entry's plan", err, provider.SittingOut())
		}
	}
	return provider
}

// chainCompany is a one-seat epoch whose seat's `llm` is members, in order, over
// real harness providers and the providers.llm entries that place them.
func chainCompany(t *testing.T, members ...chainMember) (*Company, *org.Role) {
	t.Helper()
	entries := make([]phase.Entry, 0, len(members))
	configured := make(map[string]config.LLMProvider, len(members))
	keys := make(org.ProviderKeys, 0, len(members))
	for _, m := range members {
		entries = append(entries, phase.Entry{Key: m.key, Provider: harnessProvider(t, m)})
		configured[m.key] = config.LLMProvider{
			Harness: config.Harness(m.agent), Model: m.model, RunIn: m.runIn, Env: m.env,
		}
		keys = append(keys, m.key)
	}
	models, err := phase.NewRegistry(entries)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	seat := &org.Role{Name: "SWE", LLM: keys}
	return &Company{
		Models: models,
		Org:    &org.Organization{Name: "Acme", Roles: []*org.Role{seat}},
		Config: &config.Company{Providers: config.Providers{
			LLM:     configured,
			Sandbox: &config.SandboxProvider{Fake: true},
		}},
	}, seat
}

// executorCompany is [chainCompany] with one claude-code entry that can take a
// launch anywhere.
func executorCompany(t *testing.T) (*Company, *org.Role) {
	t.Helper()
	return chainCompany(t, chainMember{key: "api", agent: "claude-code", model: "claude-golden"})
}

// A TYPED NIL IS NOT A NIL INTERFACE, and this is the bug that class of mistake
// produces here: a nil *agentLauncher assigned into the runner's Launcher field
// is a NON-nil interface, so the runner is built and dereferences nothing at
// its first launch, where a nil interface is refused as the assembly mistake it
// is. A company with no models and a handle no agent seat holds are the two
// shapes with nothing to launch on.
//
// AND EVERY AGENT SEAT GETS ONE, over the turn's pinned company: the member,
// its harness and its cell are the launch's to choose, from that company, so
// nothing about them is resolved here.
func TestEverySeatGetsALauncherOverItsPinnedCompanyAndNoneGetsATypedNil(t *testing.T) {
	t.Parallel()
	c, _ := executorCompany(t)
	e := &Engine{}
	got := e.agentRunFor(c, "swe", &turnctx.Turn{RunID: "t1"}, nil)
	launcher, ok := got.(*agentLauncher)
	if !ok || launcher == nil {
		t.Fatalf("an agent seat got %T, want a launcher", got)
	}
	if launcher.company != c {
		t.Error("the launcher is not over the company the turn was pinned to")
	}
	for name, company := range map[string]*Company{
		"no models": {Org: c.Org, Config: c.Config},
	} {
		if got := e.agentRunFor(company, "swe", &turnctx.Turn{RunID: "t1"}, nil); got != nil {
			t.Errorf("%s: got %T, want a nil interface", name, got)
		}
	}
	if got := e.agentRunFor(c, "nobody", &turnctx.Turn{RunID: "t1"}, nil); got != nil {
		t.Errorf("a handle no seat holds got %T, want a nil interface", got)
	}
}

// launchRig is an engine that can take an executor launch all the way to a
// box — the in-process double for every cell, the double's runner for every
// harness that has one, a durable run store, a queue for the start event and the
// given bridge, mounted — and the runners every launch starts its job on.
func launchRig(t *testing.T, c *Company, bridge *mcpbridge.Bridge) (*Engine, *sandbox.FakeRunner) {
	t.Helper()
	e, runners := launchRigRunners(t, c, bridge)
	return e, runners["claude-code"]
}

// launchRigRunners is [launchRig] answering every harness's runner, by name: a case
// about WHICH runner a launch starts its job on has to see the one it did not.
func launchRigRunners(t *testing.T, c *Company, bridge *mcpbridge.Bridge) (*Engine, map[string]*sandbox.FakeRunner) {
	t.Helper()
	runners := map[string]*sandbox.FakeRunner{
		"claude-code": sandbox.NewFakeRunner("claude-code"),
		"opencode":    sandbox.NewFakeRunner("opencode"),
	}
	providers := map[sandbox.Placement]sandbox.Provider{}
	for _, p := range sandbox.Placements {
		providers[p] = sandbox.NewFakeProvider()
	}
	// THE COMPANY'S OWN PROVIDER-WIDE STEPS, as buildSandbox hands them to
	// the manager, so a launch here provisions what a node's would.
	var defaults []sandbox.SetupStep
	if spec := c.Config.Providers.Sandbox; spec != nil {
		defaults = setupSteps(spec.Setup)
	}
	// AND ITS SAY ON CREDITING COMMITS, as buildSandbox hands it over, so a
	// job here is started asking for what a node's would.
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: providers, DefaultPlacement: sandbox.Direct,
		Runners: map[string]sandbox.Runner{
			"claude-code": runners["claude-code"], "opencode": runners["opencode"],
		},
		DefaultSetup:     defaults,
		AttributeCommits: c.Config.Providers.Sandbox.AttributesCommits(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	q := memory.New()
	pending := sandbox.NewCoordStore(coordmemory.NewFleet())
	// Mounted, as serveAPI mounts it on a node with a listener: a bridge no
	// listener took opens no session.
	_ = bridge.Handler()
	e := &Engine{
		backends: &Backends{Queue: q},
		bridge:   bridge,
	}
	// The engine's own resumer, as buildSandboxRuntime hands it over: the
	// coordinator refuses to be built without one.
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Answers:  testAnswers(),
		Audience: noAudience{},
		Queue:    q, Pending: pending, Manager: manager, Resume: &resumer{engine: e},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	e.useSandbox(pending, coordinator)
	// THE EPOCH'S OWN CATALOGUE, as an apply pins it ([Company.catalogue]):
	// every launch of c's turns places its box by this manager.
	c.catalogue = manager
	e.epoch.current.Store(c)
	return e, runners
}

// launchReadyEngine is [launchRig] over a bridge with somewhere to dial.
func launchReadyEngine(t *testing.T, c *Company) *Engine {
	t.Helper()
	e, _ := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	return e
}

// launcherOn is the launcher a turn on c's seat would be given.
func launcherOn(e *Engine, c *Company, seat *org.Role, turnID string) *agentLauncher {
	return &agentLauncher{
		engine: e, company: c, seat: seat,
		turn: &turnctx.Turn{RunID: turnID, Seat: seat, Org: c.Org, Task: "fix the failing test"},
	}
}

// launchRequest is what the runner hands a launcher: the brief and a surface
// bound to the launcher's own turn, as the runner binds every phase surface —
// the bridge acts as that turn's seat on every call and refuses a surface that
// names none — and a hand-off hook that records what it is told.
func launchRequest(l *agentLauncher, handoffs *[]chain.Fallback) runner.AgentRunRequest {
	return runner.AgentRunRequest{
		Brief: "## Task\nfix the failing test", Round: 1,
		// THE TURN'S FENCE, as the engine builds it for the runner and the
		// runner hands it on: asked again by the launch.
		Fence:   l.engine.seatFence(l.seat.Handle()),
		Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil).ForTurn(l.turn),
		OnFallback: func(f chain.Fallback) {
			if handoffs != nil {
				*handoffs = append(*handoffs, f)
			}
		},
	}
}

// refusedCleanly asserts a refused launch left nothing behind: no row, no live
// bridge session, no job.
func refusedCleanly(t *testing.T, e *Engine, turnID string, jobs *sandbox.FakeRunner) {
	t.Helper()
	if e.bridge != nil && e.bridge.Live() != 0 {
		t.Errorf("%d bridge sessions live for a launch that was refused", e.bridge.Live())
	}
	if rt := e.sandbox.Load(); rt != nil {
		if _, found, err := rt.pending.Get(t.Context(), turnID); err != nil || found {
			t.Errorf("a run row exists for a launch that was refused (found %v, err %v)", found, err)
		}
	}
	if jobs != nil && len(jobs.Started()) != 0 {
		t.Errorf("%d jobs started for a launch that was refused", len(jobs.Started()))
	}
}

// A LAUNCH PLACES ITS RUN IN THE CELL OF THE MEMBER IT RUNS ON: the entry's
// own `run_in`, else the catalogue's default, spelled once — the empty
// placement reaches the manager, which resolves it. The row records the cell,
// which is what the run is collected and reclaimed through on whatever node
// does that.
func TestALaunchPlacesTheRunInItsMembersCell(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		runIn config.Placement
		want  sandbox.Placement
	}{
		"its own run_in":        {runIn: config.PlacementE2B, want: sandbox.E2B},
		"the catalogue default": {want: sandbox.Direct},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, seat := chainCompany(t, chainMember{key: "api", agent: "claude-code",
				model: "claude-golden", runIn: tc.runIn})
			e := launchReadyEngine(t, c)
			l := launcherOn(e, c, seat, "t1")
			if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err != nil {
				t.Fatalf("LaunchExecutor: %v", err)
			}
			row, _, err := e.sandbox.Load().pending.Get(t.Context(), "t1")
			if err != nil || sandbox.Placement(row.Placement) != tc.want {
				t.Fatalf("the run is placed in %q (err %v), want %q", row.Placement, err, tc.want)
			}
		})
	}
}

// FALLBACK AT LAUNCH. A head whose plan is spent on this node is passed over
// for the next member that can take the run, and the run is that member's
// whole: its model, its harness and the entry the job is recorded under — which is
// what the run's spend is charged to. The hand-off is the runner's to publish,
// against the turn, so it is handed to the request's hook naming both entries.
//
// Mutation: launch on the chain's head without asking whether it sits out,
// and the run goes out on the spent entry.
func TestALaunchFallsPastASpentHeadToTheNextMemberAndRunsOnIt(t *testing.T) {
	t.Parallel()
	c, seat := chainCompany(t,
		chainMember{key: "plan", agent: "claude-code", model: "opus", spent: true},
		chainMember{key: "spare", agent: "claude-code", model: "haiku"},
	)
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	var handoffs []chain.Fallback
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, &handoffs)); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	started := jobs.Started()
	if len(started) != 1 || started[0].Model != "haiku" {
		t.Fatalf("started %+v, want one run on the spare entry's model", started)
	}
	row, _, _ := e.sandbox.Load().pending.Get(t.Context(), "t1")
	if row.Launch.ProviderKey != "spare" || row.Launch.Model != "haiku" {
		t.Errorf("the job is recorded under %q/%q, want the member that took it",
			row.Launch.ProviderKey, row.Launch.Model)
	}
	if len(handoffs) != 1 || handoffs[0].From != "plan" || handoffs[0].To != "spare" ||
		handoffs[0].Kind != llm.KindRateLimit {
		t.Errorf("hand-offs = %+v, want one from the spent head to the spare", handoffs)
	}
}

// A LAUNCH RUNS ON ITS MEMBER'S OWN HARNESS. The box's runner is the picked
// member's, never a constant and never the chain head's: an OpenCode entry's
// model and login handed to Claude Code is a run that can read neither — and
// on a fallback the head is exactly the member that was NOT picked. So an
// OpenCode member alone, and one taking the launch from a spent Claude Code
// head, both start on the OpenCode runner, with the entry's model as written,
// and the row records the harness the job runs.
func TestALaunchRunsOnItsMembersOwnHarness(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		members []chainMember
	}{
		{"an opencode member alone", []chainMember{
			{key: "oc", agent: "opencode", model: "anthropic/claude-sonnet-5"},
		}},
		{"an opencode member after a spent claude-code head", []chainMember{
			{key: "plan", agent: "claude-code", model: "opus", spent: true},
			{key: "oc", agent: "opencode", model: "anthropic/claude-sonnet-5"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, seat := chainCompany(t, tc.members...)
			e, runners := launchRigRunners(t, c, bridgeFor(t, mcpbridge.Options{
				Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
			}))
			l := launcherOn(e, c, seat, "t1")
			if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err != nil {
				t.Fatalf("LaunchExecutor: %v", err)
			}
			if got := runners["claude-code"].Started(); len(got) != 0 {
				t.Fatalf("the claude-code runner started %d jobs for an opencode member", len(got))
			}
			started := runners["opencode"].Started()
			if len(started) != 1 || started[0].Model != "anthropic/claude-sonnet-5" {
				t.Fatalf("the opencode runner started %+v, want one job on the entry's model as written",
					started)
			}
			row, _, _ := e.sandbox.Load().pending.Get(t.Context(), "t1")
			if row.CodingAgent != "opencode" || row.Launch.ProviderKey != "oc" {
				t.Errorf("the row records %q on %q, want the opencode job the member took",
					row.CodingAgent, row.Launch.ProviderKey)
			}
		})
	}
}

// A MEMBER WITH NO IN-BOX RUNNER IS PASSED OVER SILENTLY. Its harness can serve the
// seat's text phases and never its executor, which is a fact of the
// configuration rather than a refusal of the moment: config says so once, as
// a warning, and announcing a hand-off on every launch would publish a
// fallback nothing fell from.
func TestALaunchPassesOverAMemberWithNoRunnerAndAnnouncesNothing(t *testing.T) {
	t.Parallel()
	c, seat := chainCompany(t,
		chainMember{key: "codex", agent: "codex", model: "gpt-golden"},
		chainMember{key: "api", agent: "claude-code", model: "claude-golden"},
	)
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	var handoffs []chain.Fallback
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, &handoffs)); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	if started := jobs.Started(); len(started) != 1 || started[0].Model != "claude-golden" {
		t.Fatalf("started %+v, want one run on the claude-code entry", started)
	}
	if len(handoffs) != 0 {
		t.Errorf("hand-offs = %+v, want none for a member that could never take the run", handoffs)
	}
}

// EVERY RUNNER MEMBER SITTING OUT IS AN EXHAUSTED CHAIN, the same
// [*chain.Error] an exhausted call reports, each member's refusal on it — so
// the launch reads as a chain at its limit ([chain.Limited]), on which the seat
// is parked until the first member is tried again, exactly as for a reviewer's
// call — and nothing is opened for it.
//
// Mutation: drop the refusals from chain.Pick's error, and the launch is no
// limited chain.
func TestALaunchWithEveryRunnerSittingOutIsAnExhaustedChain(t *testing.T) {
	t.Parallel()
	c, seat := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus", spent: true})
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	_, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil))
	var exhausted *chain.Error
	if !errors.As(err, &exhausted) || len(exhausted.Attempted) != 1 || exhausted.Attempted[0] != "plan" {
		t.Fatalf("LaunchExecutor = %v, want the chain exhausted on its one runner", err)
	}
	if llm.KindOf(err) != llm.KindRateLimit {
		t.Errorf("the exhausted launch classifies as %v, want the rate limit it sat out on",
			llm.KindOf(err))
	}
	if limit, limited := chain.Limited(err); !limited || limit.RetryAt.IsZero() {
		t.Errorf("chain.Limited = %+v, %v; want the launch read as a chain at its limit", limit, limited)
	}
	refusedCleanly(t, e, "t1", jobs)
}

// A CHAIN WITH NO RUNNER MEMBER IS REFUSED NAMING IT, and the harnesses that could
// take it: config refuses this chain on every write, so reaching it is a
// company built by hand, and the operator's to fix rather than a retry's. The
// chain is said as a list and the harnesses as the set an entry picks from, each
// quoted, never as Go prints a slice or a list joined by hand.
//
// Mutation: print the chain with brackets or the harnesses joined by commas and
// this goes red.
func TestAChainWithNoRunnerMemberIsRefusedNamingIt(t *testing.T) {
	t.Parallel()
	c, seat := chainCompany(t, chainMember{key: "codex", agent: "codex", model: "gpt-golden"},
		chainMember{key: "gemini", agent: "gemini-cli", model: "gemini-golden"})
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	_, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil))
	if err == nil || !strings.Contains(err.Error(), `("codex" and "gemini")`) ||
		!strings.Contains(err.Error(), `"claude-code" or "opencode"`) {
		t.Fatalf("LaunchExecutor = %v, want a refusal naming the chain and the runners", err)
	}
	// CLASSED AS THE CONFIGURATION IT IS, which whoever is told the run did
	// not start reads: an operator's edit clears it, never a wait.
	// Mutation: drop the class from executorRunners' refusal and this fails.
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
		t.Errorf("the refusal is classed %q (%v), want %q", why, ok, sandbox.WhyConfiguration)
	}
	refusedCleanly(t, e, "t1", jobs)
}

// A COMPANY WITH NO MODELS AT ALL IS REFUSED AS CONFIGURATION. No such company
// is given a launcher ([Engine.agentRunFor]), so only one built by hand meets
// the refusal, but what it is told is still what clears it: a person's edit to
// providers.llm, which no wait makes.
//
// Mutation: drop the class from executorRunners' refusal of no providers and
// this goes red.
func TestALaunchForACompanyWithNoModelsIsConfiguration(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	c.Models = nil
	l := launcherOn(e, c, seat, "t1")
	_, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil))
	if !errors.Is(err, phase.ErrNoProviders) {
		t.Fatalf("a launch for a company with no models = %v, want ErrNoProviders", err)
	}
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
		t.Errorf("the refusal is classed %q (%v), want %q", why, ok, sandbox.WhyConfiguration)
	}
	refusedCleanly(t, e, "t1", jobs)
}

// A SEAT THAT MOVES WHILE ITS EXECUTOR LAUNCHES IS THE MOVE, whatever the
// launch met, and the dispatcher hands the delivery on to the seat's holder.
//
// The box is made and set up between the run's row and its attach, minutes for
// some setups, and the seat can move meanwhile. What the launch meets then
// need not say so: the seat's next holder ends a launching run without fencing
// it, so the attach finds that ending or no record, and only a row that holder
// stamped first refuses it as a seat not held. Returned as the launch's own
// failure, each was a broken turn, NAKed onto the failure path's backoff,
// where a seat that moved is deferred to its holder at once. The turn's own
// fence is what knows, so it is asked again; where it still holds, a newer
// lease's refusal is the launch's alone, a disagreement the heartbeat
// settles, and NAKed as before.
//
// Mutation: drop the fence asked again and every moved row NAKs; ask it only
// of a seat-not-held refusal and the ended row does; answer the fence's error
// alone and the store's ErrSeatNotHeld is gone from the rows that carry it;
// wrap the move whatever the fence says and the unnoticed lease defers.
func TestASeatThatMovesWhileItsExecutorLaunchesIsHandedOn(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		// held is what this node's seat lease answers for the seat.
		held bool
		// stamp has a newer lease claim the run's row as its box is made;
		// recover has the seat's next holder recover the seat then.
		stamp, recover bool
		// moved closes the turn's fence as the box is made; letGo closes it
		// once the launch has asked it the first time.
		moved, letGo bool
		notHeld      bool
		want         queue.Outcome
	}{
		"a newer lease stamped the row as its box was made": {
			held: true, stamp: true, moved: true, notHeld: true, want: queue.OutcomeDefer},
		"the next holder ended the launching row as its box was made": {
			held: true, recover: true, moved: true, want: queue.OutcomeDefer},
		"the seat was let go once the turn's fence had passed": {
			letGo: true, notHeld: true, want: queue.OutcomeDefer},
		"a newer lease this node has not noticed": {
			held: true, stamp: true, notHeld: true, want: queue.OutcomeNak},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, seat := executorCompany(t)
			e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
				Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
			}))
			pending := e.sandbox.Load().pending
			cell := &movingCell{FakeProvider: sandbox.NewFakeProvider()}
			manager, err := sandbox.NewManager(sandbox.ManagerOptions{
				Providers:        map[sandbox.Placement]sandbox.Provider{sandbox.Direct: cell},
				DefaultPlacement: sandbox.Direct,
				Runners:          map[string]sandbox.Runner{"claude-code": jobs},
			})
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			// THIS NODE WRITES UNDER ITS SEAT LEASE, as a fleet node does, and
			// the seat's next holder recovers it over the same run store.
			coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
				Answers: testAnswers(), Audience: noAudience{}, Queue: e.backends.Queue,
				Pending: pending, Manager: manager, Resume: &resumer{engine: e}, Node: "node-a",
				Lease: func(string) (sandbox.Fence, bool) {
					return sandbox.Fence{Owner: "node-a:1", Epoch: 3}, tc.held
				},
			})
			if err != nil {
				t.Fatalf("NewCoordinator: %v", err)
			}
			successor, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
				Answers: testAnswers(), Audience: noAudience{}, Queue: e.backends.Queue,
				Pending: pending, Manager: manager, Resume: &resumer{engine: e}, Node: "node-b",
			})
			if err != nil {
				t.Fatalf("NewCoordinator: %v", err)
			}
			t.Cleanup(coordinator.Stop)
			t.Cleanup(successor.Stop)
			e.useSandbox(pending, coordinator)
			c.catalogue = manager

			var moved atomic.Bool
			cell.creating = func(ctx context.Context) {
				moved.Store(tc.moved)
				if tc.stamp {
					if won, err := pending.ClaimOwnership(ctx, "t1", "node-b:1", 7); err != nil || !won {
						t.Errorf("the newer lease's claim = %v, %v", won, err)
					}
				}
				if tc.recover {
					if err := successor.RecoverSeat(ctx, seat.Handle(), "node-b:1", 7); err != nil {
						t.Errorf("the next holder's recovery: %v", err)
					}
				}
			}
			l := launcherOn(e, c, seat, "t1")
			req := launchRequest(l, nil)
			var asked atomic.Int32
			req.Fence = func() error {
				if moved.Load() || (tc.letGo && asked.Add(1) > 1) {
					return fmt.Errorf("%w: %q was held at epoch 3 and is not held here any more",
						seatpkg.ErrSeatMoved, seat.Handle())
				}
				return nil
			}
			var (
				launchErr error
				deferred  []string
			)
			d := &Dispatcher{
				NoteDeferred: func(handle string) { deferred = append(deferred, handle) },
				Turn: func(ctx context.Context, _ Request) (turn.Result, error) {
					_, launchErr = l.LaunchExecutor(ctx, req)
					// AS THE RUNNER WRAPS IT on its way to the dispatcher.
					return turn.Result{}, fmt.Errorf("runner: %s: launching the executor's run: %w",
						phase.Execute, launchErr)
				},
			}
			got := d.Dispatch(t.Context(), seat.Handle(), []*events.Event{
				events.New(types.TaskAssigned{TaskID: "task-1"}, events.TraceContext{}),
			})
			if launchErr == nil {
				t.Fatal("the launch went ahead on a run its seat's move refused")
			}
			if isMove := errors.Is(launchErr, seatpkg.ErrSeatMoved); isMove != (tc.want == queue.OutcomeDefer) {
				t.Errorf("the launch answered %v: the seat's move in it is %v", launchErr, isMove)
			}
			if notHeld := errors.Is(launchErr, sandbox.ErrSeatNotHeld); notHeld != tc.notHeld {
				t.Errorf("the launch answered %v: the run store's seat-not-held in it is %v, want %v",
					launchErr, notHeld, tc.notHeld)
			}
			if got.Outcome != tc.want {
				t.Errorf("the delivery's outcome is %v, want %v", got.Outcome, tc.want)
			}
			if handedOn := len(deferred) == 1; handedOn != (tc.want == queue.OutcomeDefer) {
				t.Errorf("the seat host was told of a deferral %d times", len(deferred))
			}
			if started := jobs.Started(); len(started) != 0 {
				t.Errorf("%d jobs started on a run the launch did not keep", len(started))
			}
		})
	}
}

// movingCell is a cell whose Create runs creating first: what lands on the
// run's row and on its seat while a launch makes the run's box.
type movingCell struct {
	*sandbox.FakeProvider
	creating func(ctx context.Context)
}

func (c *movingCell) Create(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	if c.creating != nil {
		c.creating(ctx)
	}
	return c.FakeProvider.Create(ctx, spec)
}

// THE GUARD JUDGES THE PICKED MEMBER'S LOGIN, the one the box runs under. A
// remote run on a plan login that cannot follow it is refused before any box
// exists; the same seat whose plan entry sits out falls back to an API-key
// entry whose key travels, and launches. A guard that asked the chain's head
// would refuse the second launch — and wave through a fallback onto a member
// with nothing to sign in with.
func TestTheCredentialGuardJudgesThePickedMembersLogin(t *testing.T) {
	t.Parallel()
	bridge := func() *mcpbridge.Bridge {
		return bridgeFor(t, mcpbridge.Options{
			Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
		})
	}
	plan := chainMember{key: "plan", agent: "claude-code", model: "opus",
		runIn: config.PlacementE2B, subscription: true}
	key := chainMember{key: "api", agent: "claude-code", model: "claude-golden", runIn: config.PlacementE2B}

	c, seat := chainCompany(t, plan, key)
	e, jobs := launchRig(t, c, bridge())
	l := launcherOn(e, c, seat, "t1")
	_, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil))
	var credErr *SandboxCredentialError
	if !errors.As(err, &credErr) || !strings.Contains(err.Error(), `"plan"`) {
		t.Fatalf("a remote run on a plan login that cannot follow it = %v, want it refused "+
			"naming the entry", err)
	}
	refusedCleanly(t, e, "t1", jobs)

	plan.spent = true
	c, seat = chainCompany(t, plan, key)
	e, jobs = launchRig(t, c, bridge())
	l = launcherOn(e, c, seat, "t1")
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err != nil {
		t.Fatalf("the fallback onto an entry whose key travels was refused: %v", err)
	}
	if started := jobs.Started(); len(started) != 1 || started[0].Env["ANTHROPIC_API_KEY"] != "sk-ant-not-real" {
		t.Errorf("the run does not carry the API-key entry's key: %+v", started)
	}
}

// THE BUDGET FLOOR IS ASKED AT THE LAUNCH, and below it the launch is a BUDGET
// refusal — what the dispatcher parks the seat on rather than redelivering into
// the same refusal — with nothing opened: no row, no session, no box. An
// unreadable budget is refused too, but not as a budget refusal: it is the
// store's moment, and a redelivery's backoff rides it out.
//
// Mutation: drop the floor from the launch, and the starved seat launches.
func TestALaunchBelowTheExecutorFloorIsABudgetRefusal(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	seat.TokenBudget = org.TokenCeilings{period.Day: 30000}
	fleet := coordmemory.NewFleet()
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	e.backends.Fleet = fleet
	id, _ := c.Org.AgentIDFor(seat)
	if _, err := fleet.PostCharge(t.Context(), coord.AgentScope(id.String()), 10000,
		coord.WindowsAt(time.Now(), time.UTC)); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	l := launcherOn(e, c, seat, "t1")
	_, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil))
	if !errors.Is(err, toolloop.ErrBudgetExhausted) ||
		!strings.Contains(err.Error(), "executor_min_budget_tokens") {
		t.Fatalf("a launch with 20000 tokens left under a 25000 floor = %v, want a budget "+
			"refusal naming the floor", err)
	}
	refusedCleanly(t, e, "t1", jobs)
}

// The floor's three answers, at the boundary the launch asks them at.
func TestTheExecutorFloorIsThreeValued(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	if err := executorHeadroom(ctx, nil, 25000); err != nil {
		t.Errorf("an uncapped seat was refused: %v", err)
	}
	if err := executorHeadroom(ctx, staticRemaining{left: 1}, 0); err != nil {
		t.Errorf("a company with the floor off refused a launch: %v", err)
	}
	if err := executorHeadroom(ctx, staticRemaining{left: 25000}, 25000); err != nil {
		t.Errorf("a seat exactly at the floor was refused: %v", err)
	}
	if err := executorHeadroom(ctx, staticRemaining{left: 24999}, 25000); !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Errorf("a seat one token below the floor = %v, want a budget refusal", err)
	}
	down := errors.New("store down")
	err := executorHeadroom(ctx, staticRemaining{err: down}, 25000)
	if err == nil || errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Errorf("an unreadable budget = %v, want refused, and not as an exhausted one", err)
	}
	// NOR AS ANY SANDBOX CLASS, and with what kept the counter from being
	// read still in it: a store that did not answer is the caller's to word,
	// not a budget somebody spent or a setup somebody has to fix.
	if why, ok := sandbox.WhyOf(err); ok {
		t.Errorf("an unreadable budget is classed %q", why)
	}
	if !errors.Is(err, down) {
		t.Errorf("the refusal %v lost what kept the budget from being read", err)
	}
}

// staticRemaining is a headroom reader that answers what the case says.
type staticRemaining struct {
	left int
	err  error
}

func (r staticRemaining) Remaining(context.Context) (int, error) { return r.left, r.err }

// A SEAT A PERSON STOPPED LAUNCHES NOTHING. The runner asked the seat's fence
// when its phase opened; a stop placed while the turn built its prompt must
// still keep the box from being bought, so the launch asks again before it
// opens anything — and its refusal is the stop, which the runner records as a
// stopped phase rather than a failed one.
func TestAStoppedSeatLaunchesNothing(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	id, _ := c.Org.AgentIDFor(seat)
	e.pauses.pauses = map[uuid.UUID]coord.SeatPause{id: {
		Seat: id, By: "jane", ByKind: iam.ActorHuman, StopRunning: true, At: time.Now(),
	}}
	e.pauses.known = true
	l := launcherOn(e, c, seat, "t1")
	_, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil))
	if !turn.Stopped(err) {
		t.Fatalf("a launch on a stopped seat = %v, want the stop", err)
	}
	refusedCleanly(t, e, "t1", jobs)
}

// A LAUNCHED RUN IS TOLD HOW TO GET A PERSON'S ANSWER. The run's surface
// carries `ask_and_wait` and the collection parks on what it records, and the
// run's system text is the only place that says when to use it: without it a
// run guessed rather than parked. Both ways are said (the tools for a question
// the run can go on without, `ask_and_wait` with its options for one it
// cannot) with the names the chart gives the seat.
func TestALaunchedRunIsToldHowToGetAPersonsAnswer(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	started := jobs.Started()
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want 1", len(started))
	}
	for _, said := range []string{"`" + builtin.AskAndWaitTool + "` with", "`options`",
		"`" + builtin.CommentOnWorkTool + "` with `ask`"} {
		if !strings.Contains(started[0].System, said) {
			t.Errorf("the run's system text does not say %q:\n%s", said, started[0].System)
		}
	}
}

// A LAUNCH PLACES ITS BOX BY ITS EPOCH'S CATALOGUE. The member, its run_in and
// the seat's box settings come from the turn's pinned epoch, so the cells, the
// engine-wide setup steps and the default turn cap have to come from the same
// one: an apply landing between the turn's start and its launch swaps the
// node's live manager, and a launch that read that one placed a pinned entry by
// a catalogue its own revision never had.
func TestALaunchPlacesItsBoxByItsEpochsCatalogue(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	// THE NEXT REVISION'S CATALOGUE, live on the node: another default
	// cell, a setup step of its own and another turn cap.
	next, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:        map[sandbox.Placement]sandbox.Provider{sandbox.E2B: sandbox.NewFakeProvider()},
		DefaultPlacement: sandbox.E2B,
		Runners:          map[string]sandbox.Runner{"claude-code": jobs},
		DefaultMaxTurns:  7,
		DefaultSetup:     []sandbox.SetupStep{{Name: "next-revision", Brief: "next-revision is set up"}},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	e.sandbox.Load().coordinator.SetManager(next)

	l := launcherOn(e, c, seat, "t1")
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	row, _, _ := e.sandbox.Load().pending.Get(t.Context(), "t1")
	if row.Placement != string(sandbox.Direct) {
		t.Errorf("the run was placed in %q, want its epoch's default cell, direct", row.Placement)
	}
	started := jobs.Started()
	if len(started) != 1 || started[0].Limits.MaxTurns == 7 {
		t.Fatalf("started %+v, want one job under its epoch's turn cap", started)
	}
	if strings.Contains(started[0].System, "next-revision") {
		t.Errorf("the run was provisioned with the live revision's setup step:\n%s", started[0].System)
	}
}

// A LAUNCH CARRIES ITS TURN'S DEADLINE ONTO THE RUN. The turn's own segments
// stop starting rounds once its wall-clock cap ends, and the run is the one
// part of the turn that can outlast it — so the row says when, for the poll to
// end a run that does. Every turn but a scheduled fire carries none.
func TestALaunchCarriesItsTurnsDeadline(t *testing.T) {
	t.Parallel()
	for name, deadline := range map[string]time.Time{
		"a scheduled fire's cap": time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC),
		"no cap":                 {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, seat := executorCompany(t)
			e, _ := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
				Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
			}))
			l := launcherOn(e, c, seat, "t1")
			l.turn.Deadline = deadline
			if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err != nil {
				t.Fatalf("LaunchExecutor: %v", err)
			}
			row, _, _ := e.sandbox.Load().pending.Get(t.Context(), "t1")
			if !row.Deadline.Equal(deadline) {
				t.Errorf("the run carries deadline %v, want its turn's %v", row.Deadline, deadline)
			}
		})
	}
}

// A LAUNCH IS HELD TO THE GRANT ITS TURN WAS ADMITTED UNDER. The fence it asks
// is the turn's own, handed on by the runner, which captured the seat's grant
// when the turn began — so a seat that moved while the prompt was built, and
// even one that came back under a new grant, launches nothing. A fence the
// launch built for itself captured the grant at the moment it asked, compared
// that grant with itself, and could not see either.
func TestALaunchAsksItsTurnsOwnFence(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	req := launchRequest(l, nil)
	moved := fmt.Errorf("%w: %q was held at epoch 3 and is now held at epoch 4", seatpkg.ErrSeatMoved, "swe")
	asked := 0
	req.Fence = func() error { asked++; return moved }
	_, err := l.LaunchExecutor(t.Context(), req)
	if !errors.Is(err, seatpkg.ErrSeatMoved) || asked != 1 {
		t.Fatalf("a launch whose turn's grant moved = %v (fence asked %d times), want the move", err, asked)
	}
	refusedCleanly(t, e, "t1", jobs)

	// AND NONE AT ALL IS AN ASSEMBLY FAULT, never an open fence.
	req.Fence = nil
	if _, err := l.LaunchExecutor(t.Context(), req); err == nil || !strings.Contains(err.Error(), "no fence") {
		t.Fatalf("a launch with no fence from its turn = %v, want it refused", err)
	}
	refusedCleanly(t, e, "t1", jobs)
}

// A RUN WITH NO SANDBOX RUNTIME IS REFUSED, NAMING WHAT TO CONFIGURE — and so is
// one whose pinned company dropped its catalogue while the node's runtime
// carries on finishing the runs already out: a launch then would mint a box on
// a catalogue the company no longer configures.
func TestAnExecutorIsRefusedWithoutACatalogueForItsCompany(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	bare := &Engine{}
	bare.epoch.current.Store(c)
	l := launcherOn(bare, c, seat, "t1")
	_, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil))
	if err == nil || !strings.Contains(err.Error(), "providers.sandbox") {
		t.Errorf("a launch on a node with no runtime = %v, want it refused naming providers.sandbox", err)
	}
	// A NODE THAT RUNS NO SANDBOX IS CONFIGURATION, which no wait clears.
	// Mutation: drop the class from the refusal and this fails.
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
		t.Errorf("a node with no runtime: the refusal %v is classed %q (%v), want %q", err,
			why, ok, sandbox.WhyConfiguration)
	}

	dropped, seat := executorCompany(t)
	e, jobs := launchRig(t, dropped, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	dropped.Config.Providers.Sandbox = nil
	l = launcherOn(e, dropped, seat, "t1")
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err == nil ||
		!strings.Contains(err.Error(), "providers.sandbox") {
		t.Errorf("a launch for a company with no catalogue = %v, want it refused", err)
	}
	refusedCleanly(t, e, "t1", jobs)
}

// EACH RUN'S BOX IS GIVEN ITS OWN JOB'S ENDPOINT, never another's.
//
// The endpoint names the job the store opened for the run — which is what lets
// a node that takes the run over serve the same session under the token the
// box holds — so it is minted inside the launch, once the job has a name, and
// added to a server list of the job's own. A run handed another's endpoint, or
// one minted before its job existed, calls a session that is not its own or
// none at all.
//
// Mutation: mint the endpoint before the launch names the job, or for a job
// other than the row's, and the token here names no job the row holds.
func TestEachRunsBoxIsGivenItsOwnJobsEndpoint(t *testing.T) {
	t.Parallel()
	c, seat := chainCompany(t, chainMember{key: "api", agent: "claude-code",
		model: "claude-golden", runIn: config.PlacementE2B})
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	for _, runID := range []string{"t1", "t2"} {
		l := launcherOn(e, c, seat, runID)
		if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err != nil {
			t.Fatalf("LaunchExecutor(%s): %v", runID, err)
		}
	}
	started := jobs.Started()
	if len(started) != 2 {
		t.Fatalf("%d jobs started, want 2", len(started))
	}
	prefix := "https://engine.example.com" + mcpbridge.PathPrefix
	agent, _ := c.Org.AgentIDFor(seat)
	for i, runID := range []string{"t1", "t2"} {
		url := started[i].MCPServers[config.ToolServerName].URL
		token, ok := strings.CutPrefix(url, prefix)
		if !ok {
			t.Fatalf("run %s's box was given %q, not this node's bridge", runID, url)
		}
		run, found, err := e.sandbox.Load().pending.Get(t.Context(), runID)
		if err != nil || !found {
			t.Fatalf("run %s has no row (found %v, err %v)", runID, found, err)
		}
		if !strings.Contains(token, run.LaunchID+"~"+agent.String()+"~"+runID) {
			t.Errorf("run %s's endpoint does not name its own job %s and its seat", runID, run.LaunchID)
		}
		if got, reason := e.bridge.Miss(token); got != runID || reason != "" {
			t.Errorf("run %s's endpoint resolves to run %q (%s)", runID, got, reason)
		}
	}
}

// THE DURABLE LOG IS THE RESUME'S WHOLE RECORD, so the conversion has to keep
// every field the delivery check, the citations, the ledger and the executor's
// own record read — the bridge's measurement of each call included: its place
// in the order the run made its calls, when it ran, for how long and who
// answered, which is what the record's timeline places the run's calls by.
//
// Mutation: drop the place, the start, the duration or the origin from the
// conversion, and this goes red.
func TestABridgedCallSurvivesTheRoundTripToTheLedger(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 6, 15, 12, 1, 2, 0, time.UTC)
	got := bridgedCalls([]sandbox.BridgeCall{
		{Name: "slack_post", Args: `{"channel":"C1"}`, Output: "posted", Seq: 2,
			StartedAt: at, DurationMS: 1500, Origin: "mcp:slack", Server: "slack"},
		{Name: "jira_create", Args: "not json", Failed: true, Seq: 1},
	})
	if len(got) != 2 {
		t.Fatalf("converted %d calls, want 2", len(got))
	}
	if got[0].Name != "slack_post" || got[0].Result != "posted" || got[0].Failed {
		t.Errorf("call = %+v", got[0])
	}
	if got[0].Seq != 2 || !got[0].StartedAt.Equal(at) || got[0].Duration != 1500*time.Millisecond {
		t.Errorf("the bridge's measurement was lost: place %d, started %s, took %s",
			got[0].Seq, got[0].StartedAt, got[0].Duration)
	}
	if got[0].Origin != "mcp:slack" || got[0].Server != "slack" {
		t.Errorf("who answered was lost: origin %q, server %q", got[0].Origin, got[0].Server)
	}
	// AN UNMEASURED CALL STAYS UNMEASURED: a zero start is "not timed",
	// never a call placed at the epoch.
	if !got[1].StartedAt.IsZero() || got[1].Duration != 0 {
		t.Errorf("an unmeasured call gained a measurement: %+v", got[1])
	}
	if got[0].Args["channel"] != "C1" {
		t.Errorf("arguments were lost: %v", got[0].Args)
	}
	// UNDECODABLE ARGUMENTS KEEP THE CALL. One ledger line renders worse;
	// failing the resume over it would lose the whole turn.
	if got[1].Name != "jira_create" || !got[1].Failed || got[1].Seq != 1 {
		t.Errorf("a call with bad arguments was dropped or reshaped: %+v", got[1])
	}
	if got[1].Args != nil {
		t.Errorf("undecodable arguments became %v, want nil", got[1].Args)
	}
}

// A SEAT WITH NO role.sandbox BLOCK STILL RUNS ITS EXECUTOR.
//
// An executor is placed by its own providers.llm entry's `run_in`, so it runs
// in a box whether or not that block was ever written — and assembling the run
// environment used to range over the nil block's Env and panic on the seat's
// very first launch.
func TestASeatWithNoSandboxBlockStillRunsItsExecutor(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	seat := &org.Role{Name: "SWE"}
	env := e.runEnvironment(t.Context(), nil, seat, nil, nil).env
	if env == nil {
		t.Fatal("a seat with no sandbox block assembled no environment")
	}
	// The engine's own tool-agnostic facts still land.
	if env["CREWLET_AGENT_HANDLE"] == "" {
		t.Errorf("the agent identity was dropped: %v", env)
	}
	// EVERY per-seat override read at launch, not only the environment:
	// the round cap was nil-guarded and the pause TTL was not, and the
	// launch panicked on the second after passing the first.
	if got := pauseTTL(nil); got != nil {
		t.Errorf("pauseTTL(nil) = %v, want nil (inherit)", *got)
	}
	if got := maxTurnsFor(nil); got != nil {
		t.Errorf("maxTurnsFor(nil) = %v, want nil (inherit)", *got)
	}
}

// A SEAT'S SANDBOX BLOCK IS FOUND WHEREVER IT IS DECLARED.
//
// The lookup walked only the top-level `roles:`, so a seat under `units:` —
// most of a real company's seats — always answered nil, and every per-seat box
// setting (its setup steps, its MCP scope, its env, its pause TTL, its round
// cap) was silently dropped.
func TestASeatsSandboxBlockIsFoundInsideAUnit(t *testing.T) {
	t.Parallel()
	never := 0.0
	nested, deep := 12, 40
	c := composed(t, &config.Company{
		Roles: []config.Role{{Name: "CEO"}},
		Units: []config.Unit{{
			Name:  "Platform",
			Roles: []config.Role{{Name: "Nested", Sandbox: &config.RoleSandbox{MaxTurns: &nested}}},
			Children: []config.Unit{{
				Name: "Infra",
				Roles: []config.Role{{Name: "Deep", Sandbox: &config.RoleSandbox{
					MaxTurns: &deep, PauseTTLSeconds: &never,
				}}},
			}},
		}},
	})
	// ADDRESSED BY HANDLE, derived from the name where the seat declares
	// none — the same value every other surface addresses a seat by.
	for handle, want := range map[string]int{"nested": nested, "deep": deep} {
		gate := seatSandbox(c, handle)
		if gate == nil {
			t.Fatalf("seat %q inside a unit found no sandbox block", handle)
		}
		if gate.MaxTurns == nil || *gate.MaxTurns != want {
			t.Errorf("seat %q found a block that is not the one it wrote: %+v", handle, gate)
		}
	}
	// The seat at depth keeps its own settings, not the shallower one's.
	if deep := seatSandbox(c, "deep"); deep.PauseTTLSeconds == nil || *deep.PauseTTLSeconds != 0 {
		t.Errorf("the nested seat's own settings were lost: %+v", deep)
	}
	// A seat with no block still answers nil, and a handle nobody holds too.
	if seatSandbox(c, "ceo") != nil || seatSandbox(c, "nobody") != nil {
		t.Error("a seat with no block, or no such seat, answered a block")
	}

	// TWO SEATS OF ONE NAME GET THEIR OWN BLOCKS. A display name is an
	// admission rule rather than a runnable one, so a revision stored
	// before it runs with duplicates — and a walk matching on the name
	// handed whichever came first. A seat would be given its namesake's
	// setup steps, credentials and round cap.
	namesakes := composed(t, &config.Company{
		Roles: []config.Role{{Name: "Engineer", Handle: "plat-eng"}},
		Units: []config.Unit{{
			Name: "Product",
			Roles: []config.Role{{Name: "Engineer", Handle: "prod-eng",
				Sandbox: &config.RoleSandbox{MaxTurns: &nested}}},
		}},
	})
	if got := seatSandbox(namesakes, "plat-eng"); got != nil {
		t.Errorf("the seat that wrote no block was handed %+v — its namesake's", got)
	}
	if got := seatSandbox(namesakes, "prod-eng"); got == nil || got.MaxTurns == nil {
		t.Errorf("the seat that wrote a block found %+v", got)
	}

	// AND A COMPANY THAT COULD BE AMBIGUOUS IS REFUSED BEFORE IT RUNS.
	//
	// An INDEX cannot be ambiguous, because a handle is the seat's identity
	// in it. What makes that safe rather than merely convenient is the
	// refusal here: a company carrying two seats of one handle never
	// becomes an organization at all, so no lookup is ever handed one.
	ambiguous := &config.Company{
		Roles: []config.Role{{Name: "Twin"}},
		Units: []config.Unit{{
			Name:  "Platform",
			Roles: []config.Role{{Name: "Twin", Sandbox: &config.RoleSandbox{MaxTurns: &nested}}},
		}},
	}
	if _, err := ambiguous.Organization(); err == nil {
		t.Error("a company with two seats of one handle built an organization, " +
			"so a per-handle lookup could be handed either of them")
	}
}

// composed is the runtime company a document describes: its settings and the
// org derived from them.
//
// A RUNNING NODE builds its org from the document it applied, and so does
// this: it is what a fixture's seats are. A document that will not build is
// a fixture with no seats, and a case about a seat would then pass over an
// empty company — so it fails loudly here instead.
func composed(t *testing.T, cfg *config.Company) *Company {
	t.Helper()
	o, err := cfg.Organization()
	if err != nil {
		t.Fatalf("this fixture's company does not build an org: %v", err)
	}
	return &Company{Config: cfg, Org: o}
}

// A NODE THAT NOTHING REACHES REFUSES AN EXECUTOR, AND SAYS WHICH SETTING.
//
// A bridge that neither serves the route nor joined a fleet relay — a node
// serving the API with api.port 0 — used to mint an endpoint naming this node:
// the box launched, every tool call it made found nothing listening, and the
// turn was rescued as incomplete with no sign of why. The refusal names
// api.port, because the message for an unset base URL would send the operator
// to a variable that is already set, and is classed as the configuration it
// is, which a person's setting clears and no wait does.
//
// Mutation: drop the class from the refusal and this goes red.
func TestAnExecutorIsRefusedOnANodeThatServesNoBridge(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	e.bridge = bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	})
	l := launcherOn(e, c, seat, "t1")
	_, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil))
	if err == nil {
		t.Fatal("an executor launched on a node whose bridge nothing reaches")
	}
	if !strings.Contains(err.Error(), "api.port") {
		t.Errorf("the refusal does not name the setting that fixes it: %v", err)
	}
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
		t.Errorf("the refusal is classed %q (%v), want %q", why, ok, sandbox.WhyConfiguration)
	}
	refusedCleanly(t, e, "t1", jobs)
}

// A SURFACE BOUND TO NO SEAT IS REFUSED BY NAME, and not as a missing bridge.
//
// Every bridged call acts as the surface's seat, so the bridge refuses to open
// a session for a surface with none — and the launcher used to read that empty
// answer as "no bridge URL", sending an operator to a variable that was set on
// a node whose bridge was mounted and serving. The runner always binds its
// phase surface, so this is an assembly fault, and the refusal says so.
func TestAnExecutorOnASurfaceWithNoSeatIsRefusedAsAnAssemblyFault(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	req := launchRequest(l, nil)
	req.Surface = tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil)
	_, err := l.LaunchExecutor(t.Context(), req)
	if err == nil {
		t.Fatal("an executor launched over a surface bound to no seat")
	}
	if strings.Contains(err.Error(), mcpbridge.BaseURLVar) ||
		!strings.Contains(err.Error(), "bound to no seat") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
	refusedCleanly(t, e, "t1", jobs)
}

// THE SEAT'S SYSTEM PROMPT REACHES THE RUN AS ITS STANDING INSTRUCTIONS, and
// the turn's ask as its task, each on its own field all the way to the runner —
// which hands the first to the harness on the channel that adds to its own system
// prompt and runs the second. Dropped here, the run went out with the box's
// framing alone and a seat that did not know whose work it was doing.
func TestAnExecutorRunCarriesTheSeatsSystemPromptApartFromItsAsk(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	const system = "You are Agent SWE at Acme.\nSEAT-IDENTITY-MARKER"
	req := launchRequest(l, nil)
	req.System = system
	if _, err := l.LaunchExecutor(t.Context(), req); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	started := jobs.Started()
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want the executor's", len(started))
	}
	// After the one note that says this is the role and the coding tool's own
	// prompt is not, and ahead of what the box provides.
	if !strings.Contains(started[0].System, "\n\n"+system+"\n\n## Your environment") ||
		strings.Index(started[0].System, system) > strings.Index(started[0].System, "## Your environment") {
		t.Errorf("the run's standing instructions do not lead with the seat's system prompt:\n%s",
			started[0].System)
	}
	// THE ASK ONCE: it already opens with the turn's task, so nothing frames
	// that task after it a second time.
	if started[0].Brief != req.Brief {
		t.Errorf("the run's prompt is not the turn's ask alone:\n%s", started[0].Brief)
	}
}

// A RUN RECORDS THE AGENT ID OF THE TURN THAT LAUNCHED IT. The id is derived
// from the company's name and the seat's handle, so an apply that renames the
// company mid-turn changes it; the run's durable row must carry the id of the
// organization the turn is pinned to, which is the id every other event of
// that turn carries, not the one the engine's current company would derive.
func TestARunRecordsTheAgentIDOfItsPinnedTurn(t *testing.T) {
	t.Parallel()
	pinned, seat := executorCompany(t)
	e := &Engine{}
	e.epoch.current.Store(&Company{
		Config: pinned.Config, Models: pinned.Models,
		Org: &org.Organization{Name: "Acme Renamed", Roles: []*org.Role{seat}},
	})
	l := launcherOn(e, pinned, seat, "t1")
	l.turn.Org = pinned.Org
	want, ok := org.DeriveAgentID("Acme", seat.Handle())
	if !ok {
		t.Fatal("the fixture derives no agent id")
	}
	if got := l.runTurnRef(t.Context()).AgentID; got != want.String() {
		t.Errorf("the run records agent id %q, want %q: the id of the turn's own organization", got, want)
	}
}

// THE BRIDGE FOLLOWS THE SEAT. A node that takes a seat tells the bridge so
// once the seat's running jobs are recovered — not at the lease, when a missing
// session would say nothing about the run — and a node that hands the seat on
// ends its runs' sessions here, which the seat's next holder serves from then
// on: left open, they would pin the seat's surface in a process that no longer
// speaks for it, and every box would go on talking to it.
func TestTheBridgeFollowsTheSeat(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	bridge := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	})
	e, _ := launchRig(t, c, bridge)
	agent, ok := c.Org.AgentIDFor(seat)
	if !ok {
		t.Fatal("the seat has no agent id")
	}
	held := placement.Seat{ID: agent, Handle: seat.Handle()}
	if err := e.backends.Queue.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if bridge.Ready(agent) {
		t.Fatal("the bridge says the seat is taken before the node took it")
	}
	if err := e.prepareSeatSandbox(t.Context(), e.sandbox.Load(), held, 1, "node-a:1"); err != nil {
		t.Fatalf("prepareSeatSandbox: %v", err)
	}
	if !bridge.Ready(agent) {
		t.Fatal("the seat's runs were recovered and the bridge was not told the seat is taken")
	}

	l := launcherOn(e, c, seat, "t1")
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	if bridge.Live() != 1 {
		t.Fatalf("%d sessions open after the launch, want the run's", bridge.Live())
	}

	e.releaseSeat(t.Context(), held)
	if bridge.Live() != 0 {
		t.Errorf("%d sessions still open for a seat this node handed on", bridge.Live())
	}
	if bridge.Ready(agent) {
		t.Error("the bridge still says this node holds a seat it handed on")
	}
}

// bridgeFor is [mcpbridge.New] for a keyring a case expects to be usable.
func bridgeFor(t *testing.T, opts mcpbridge.Options) *mcpbridge.Bridge {
	t.Helper()
	bridge, err := mcpbridge.New(opts)
	if err != nil {
		t.Fatalf("mcpbridge.New: %v", err)
	}
	return bridge
}
