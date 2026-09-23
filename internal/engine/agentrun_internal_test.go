package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// modeCompany is a one-seat epoch whose executor is the named CLI in the named
// mode, over a REAL cliagent provider: the question is what the provider's own
// answer makes the engine do, and a fake would answer whatever the test wanted.
func modeCompany(t *testing.T, agent string, agentMode bool, runIn config.Placement) (*Company, *org.Role) {
	t.Helper()
	provider, err := cliagent.New(cliagent.Config{
		Key: "sub", Agent: agent, AgentMode: agentMode,
		StateDir: t.TempDir(), Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("cliagent.New: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "sub", Provider: provider}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	seat := &org.Role{Name: "SWE", LLM: org.ProviderKeys{"sub"}}
	mode := config.CLIModeText
	if agentMode {
		mode = config.CLIModeAgent
	}
	return &Company{
		Models: models,
		Org:    &org.Organization{Name: "Acme", Roles: []*org.Role{seat}},
		Config: &config.Company{Providers: config.Providers{
			LLM: map[string]config.LLMProvider{"sub": {
				Type: config.LLMCLIAgent,
				CLI:  &config.CLIAgent{Agent: config.CLIAgentName(agent), Mode: mode, RunIn: runIn},
			}},
		}},
	}, seat
}

// A TYPED NIL IS NOT A NIL INTERFACE, and this is the bug that class of
// mistake produces here: a nil *agentLauncher assigned into the runner's
// AgentLauncher field is a NON-nil interface, so every seat in the company —
// every API seat, every text-mode seat — takes the agent branch and
// dereferences nothing on its first turn.
func TestATextModeSeatGetsNoAgentLauncher(t *testing.T) {
	t.Parallel()
	c, _ := modeCompany(t, "claude-code", false, "")
	e := &Engine{}
	if got := e.agentRunFor(c, "swe", &turnctx.Turn{RunID: "t1"}); got != nil {
		t.Fatalf("a text-mode seat got %T, want a nil interface", got)
	}
}

// AN AGENT-MODE SEAT GETS ONE, carrying the CLI and the cell resolved from its
// own entry — resolved ONCE, before the turn, so an apply landing mid-turn
// cannot move a run that is already going.
//
// Its PRESENCE is also the whole signal the runner reads to decide that this
// executor already holds a shell and must not also be offered run_sandbox
// (see runner.offersSandbox), so these two tests pin that too: there is no
// second derivation of "is this seat in agent mode" to keep in step.
func TestAnAgentModeSeatGetsALauncherForItsOwnCLIAndCell(t *testing.T) {
	t.Parallel()
	c, _ := modeCompany(t, "opencode", true, config.PlacementE2B)
	e := &Engine{}
	got := e.agentRunFor(c, "swe", &turnctx.Turn{RunID: "t1"})
	if got == nil {
		t.Fatal("an agent-mode seat got no launcher, so its executor ran natively")
	}
	launcher, ok := got.(*agentLauncher)
	if !ok {
		t.Fatalf("launcher is %T", got)
	}
	if launcher.codingAgent != "opencode" {
		t.Errorf("coding agent = %q, want the CLI the entry names", launcher.codingAgent)
	}
	if launcher.placement != sandbox.E2B {
		t.Errorf("placement = %q, want the entry's own run_in", launcher.placement)
	}
}

// AN ENTRY THAT NAMES NO CELL DEFERS TO THE CATALOGUE, spelled once: the empty
// placement reaches the manager, which resolves it to the company default. A
// second fallback here would be a second answer to that question.
func TestAnAgentEntryWithNoCellDefersToTheCatalogue(t *testing.T) {
	t.Parallel()
	c, _ := modeCompany(t, "claude-code", true, "")
	e := &Engine{}
	launcher, _ := e.agentRunFor(c, "swe", &turnctx.Turn{RunID: "t1"}).(*agentLauncher)
	if launcher == nil {
		t.Fatal("no launcher")
	}
	if launcher.placement != "" {
		t.Errorf("placement = %q, want empty so the manager resolves the default", launcher.placement)
	}
}

// A RUN WITH NO REACHABLE BRIDGE IS REFUSED, not started.
//
// A coding agent with none of the seat's tools cannot answer anybody, cannot
// touch a ticket and cannot submit its work: it would burn a subscription
// producing prose nothing collects, and the turn would be rescued as
// incomplete with no sign of why. The refusal names the variable that fixes it.
func TestAgentModeIsRefusedWithNoBridge(t *testing.T) {
	t.Parallel()
	c, seat := modeCompany(t, "claude-code", true, config.PlacementDirect)
	e := &Engine{}
	e.epoch.current.Store(c)
	launcher := &agentLauncher{
		engine: e, turn: &turnctx.Turn{RunID: "t1", Seat: seat}, seat: seat,
		codingAgent: "claude-code", placement: sandbox.Direct,
	}
	err := launcher.LaunchExecutor(t.Context(), runnerAgentRequest())
	if err == nil {
		t.Fatal("a run was launched with no sandbox and no bridge")
	}
	// With no sandbox at all the refusal names THAT, because it is the
	// nearer of the two missing things and fixing the further one first
	// would leave the operator exactly where they started.
	if !strings.Contains(err.Error(), "providers.sandbox") {
		t.Errorf("the refusal does not name what to configure: %v", err)
	}
}

// THE BRIDGE ENTRY IS ADDED TO A COPY, never to the seat's own server map.
//
// The rendered map's values come from config the epoch owns, and an endpoint
// written into it would follow the seat into its NEXT run — where the token is
// dead and every tool call fails for a reason nothing in the config explains.
func TestTheBridgeEntryDoesNotFollowTheSeatToItsNextRun(t *testing.T) {
	t.Parallel()
	seatServers := map[string]sandbox.MCPServer{
		"jira": {Name: "jira", Transport: sandbox.TransportStdio, Command: "jira-mcp"},
	}
	first := withBridge(seatServers, "https://engine.example.com/mcp/tok-1")
	if _, leaked := seatServers["crewlet"]; leaked {
		t.Fatal("the bridge entry was written into the seat's own map")
	}
	if first["crewlet"].URL != "https://engine.example.com/mcp/tok-1" {
		t.Errorf("the copy does not carry the endpoint: %v", first["crewlet"])
	}
	second := withBridge(seatServers, "https://engine.example.com/mcp/tok-2")
	if second["crewlet"].URL == first["crewlet"].URL {
		t.Error("a second run reused the first run's dead endpoint")
	}
	// The seat's own servers survive into both, because a bridged run
	// still needs the credentials only its own MCP children hold.
	for _, out := range []map[string]sandbox.MCPServer{first, second} {
		if out["jira"].Command != "jira-mcp" {
			t.Errorf("the seat's own servers were dropped: %v", out)
		}
	}
}

// THE DURABLE LOG IS THE RESUME'S WHOLE RECORD, so the conversion has to keep
// every field the delivery check, the citations and the ledger read.
func TestABridgedCallSurvivesTheRoundTripToTheLedger(t *testing.T) {
	t.Parallel()
	got := bridgedCalls([]sandbox.BridgeCall{
		{Name: "slack_post", Args: `{"channel":"C1"}`, Output: "posted"},
		{Name: "jira_create", Args: "not json", Failed: true},
	})
	if len(got) != 2 {
		t.Fatalf("converted %d calls, want 2", len(got))
	}
	if got[0].Name != "slack_post" || got[0].Result != "posted" || got[0].Failed {
		t.Errorf("call = %+v", got[0])
	}
	if got[0].Args["channel"] != "C1" {
		t.Errorf("arguments were lost: %v", got[0].Args)
	}
	// UNDECODABLE ARGUMENTS KEEP THE CALL. One ledger line renders worse;
	// failing the resume over it would lose the whole turn.
	if got[1].Name != "jira_create" || !got[1].Failed {
		t.Errorf("a call with bad arguments was dropped or reshaped: %+v", got[1])
	}
	if got[1].Args != nil {
		t.Errorf("undecodable arguments became %v, want nil", got[1].Args)
	}
}

// runnerAgentRequest is the shape the runner hands the launcher.
func runnerAgentRequest() runner.AgentRunRequest {
	return runner.AgentRunRequest{Brief: "fix the failing test", Round: 1}
}

// AN AGENT-MODE RUN IS THE EXECUTOR, so it runs on the executor's own entry.
//
// A seat legitimately points llm_sandbox at a cheaper, more code-shaped model
// than the one it thinks with — that is what the field is for. Sending an
// agent-mode run there would run the seat's whole turn on the model it chose
// for a subordinate job, silently, on any seat that set both.
func TestAnAgentModeRunUsesTheExecutorsModelNotTheSandboxOne(t *testing.T) {
	t.Parallel()
	thinker, err := cliagent.New(cliagent.Config{
		Key: "thinker", Agent: "claude-code", AgentMode: true, Model: "opus",
		StateDir: t.TempDir(), Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("cliagent.New: %v", err)
	}
	coder, err := cliagent.New(cliagent.Config{
		Key: "coder", Agent: "claude-code", Model: "haiku",
		StateDir: t.TempDir(), Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("cliagent.New: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{
		{Key: "thinker", Provider: thinker}, {Key: "coder", Provider: coder},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	seat := &org.Role{
		Name: "SWE",
		LLM:  org.ProviderKeys{"thinker"},
		// The seat delegates CODE WORK to a cheaper model, which is what
		// this field is for and must not capture the executor.
		LLMSandbox: org.ProviderKeys{"coder"},
	}
	c := &Company{
		Models: models,
		Config: &config.Company{Providers: config.Providers{
			LLM: map[string]config.LLMProvider{
				"thinker": {Type: config.LLMCLIAgent, Model: "opus"},
				"coder":   {Type: config.LLMCLIAgent, Model: "haiku"},
			},
		}},
	}

	// ASKED THROUGH THE LAUNCHER, not through runLLM directly: what is
	// under test is which phase the agent-mode path chooses, and a test
	// that named the phase itself would pass whatever the launcher did.
	launcher := &agentLauncher{seat: seat}
	executor, _, _ := launcher.executorLLM(c)
	if executor == nil || executor.Model != "opus" {
		t.Fatalf("an agent-mode run resolved to %+v, want the executor's own model", executor)
	}
	// And a run_sandbox call still goes to the model the seat chose for it.
	delegated, _, _ := sandboxLLM(c, seat)
	if delegated == nil || delegated.Model != "haiku" {
		t.Fatalf("a delegated coding run resolved to %+v, want llm_sandbox's model", delegated)
	}
}

// A SEAT WITH NO role.sandbox BLOCK IS A SUPPORTED AGENT-MODE CONFIGURATION.
//
// An agent-mode executor is placed by its own providers.llm entry's `run_in`,
// so it runs in a box whether or not that block was ever written — and
// assembling the run environment used to range over the nil block's Env and
// panic on the seat's very first launch.
func TestAnAgentModeSeatNeedsNoSandboxBlock(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	seat := &org.Role{Name: "SWE"}
	env := e.sandboxEnv(seat, nil, nil)
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
// most of a real company's seats — always answered nil. run_sandbox then
// refused it with "this seat's sandbox is not enabled" on a seat whose block
// says exactly the opposite, and every other per-seat sandbox setting (its
// setup steps, its MCP scope, its env, its pause TTL, its round cap) was
// silently dropped.
func TestASeatsSandboxBlockIsFoundInsideAUnit(t *testing.T) {
	t.Parallel()
	never := 0.0
	c := &Company{Config: &config.Company{
		Roles: []config.Role{{Name: "CEO"}},
		Units: []config.Unit{{
			Name:  "Platform",
			Roles: []config.Role{{Name: "Nested", Sandbox: &config.RoleSandbox{Enabled: true}}},
			Children: []config.Unit{{
				Name: "Infra",
				Roles: []config.Role{{Name: "Deep", Sandbox: &config.RoleSandbox{
					Enabled: true, PauseTTLSeconds: &never,
				}}},
			}},
		}},
	}}
	for _, name := range []string{"Nested", "Deep"} {
		gate := seatSandbox(c, name)
		if gate == nil {
			t.Fatalf("seat %q inside a unit found no sandbox block", name)
		}
		if !gate.Enabled {
			t.Errorf("seat %q found a block that is not the one it wrote", name)
		}
	}
	// The seat at depth keeps its own settings, not the shallower one's.
	if deep := seatSandbox(c, "Deep"); deep.PauseTTLSeconds == nil || *deep.PauseTTLSeconds != 0 {
		t.Errorf("the nested seat's own settings were lost: %+v", deep)
	}
	// A seat with no block still answers nil, and a name nobody holds too.
	if seatSandbox(c, "CEO") != nil || seatSandbox(c, "Nobody") != nil {
		t.Error("a seat with no block, or no such seat, answered a block")
	}

	// THE FIRST MATCH IS THE ANSWER, even when it wrote no block. Using the
	// nil block as the "still looking" sentinel cannot tell a seat that
	// declared none from a name nobody holds, so the walk ran on past its
	// own answer and handed back a LATER seat's block for this one — a
	// seat that turned code work off getting somebody else's.
	shadowed := &Company{Config: &config.Company{
		Roles: []config.Role{{Name: "Twin"}},
		Units: []config.Unit{{
			Name:  "Platform",
			Roles: []config.Role{{Name: "Twin", Sandbox: &config.RoleSandbox{Enabled: true}}},
		}},
	}}
	if got := seatSandbox(shadowed, "Twin"); got != nil {
		t.Errorf("the first seat named Twin wrote no block and was handed %+v", got)
	}
}

// launchReadyEngine is an engine that can take an agent-mode launch all the
// way to a box: a remote cell served by the in-process double, a durable run
// store, a queue for the start event and a bridge with somewhere to dial.
func launchReadyEngine(t *testing.T, c *Company) *Engine {
	t.Helper()
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:          map[sandbox.Placement]sandbox.Provider{sandbox.E2B: sandbox.NewFakeProvider()},
		Runners:            map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
		DefaultCodingAgent: "claude-code",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	q := memory.New()
	pending := sandbox.NewCoordStore(coordmemory.NewFleet())
	bridge := mcpbridge.New(mcpbridge.Options{
		Key: []byte("test-key"), BaseURL: "https://engine.example.com",
	})
	// Mounted, as serveAPI mounts it on a node with a listener: a bridge no
	// listener took opens no session.
	_ = bridge.Handler()
	e := &Engine{
		backends:       &Backends{Queue: q},
		sandboxPending: pending,
		bridge:         bridge,
	}
	// The engine's own resumer, as buildSandboxRuntime hands it over: the
	// coordinator refuses to be built without one.
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Queue: q, Pending: pending, Manager: manager, Resume: &resumer{engine: e},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	e.sandboxCoordinator = coordinator
	e.epoch.current.Store(c)
	return e
}

// A NODE THAT SERVES NO BRIDGE REFUSES AGENT MODE, AND SAYS WHICH SETTING.
//
// A base URL on a node that binds no listener (api.port 0) used to mint an
// endpoint naming this node: the box launched, every tool call it made found
// nothing listening, and the turn was rescued as incomplete with no sign of
// why. The refusal names api.port, because the message for an unset base URL
// would send the operator to a variable that is already set.
func TestAnAgentModeRunIsRefusedOnANodeThatServesNoBridge(t *testing.T) {
	t.Parallel()
	c, seat := splitLoginCompany(t, "api", "codex")
	e := launchReadyEngine(t, c)
	e.bridge = mcpbridge.New(mcpbridge.Options{
		Key: []byte("test-key"), BaseURL: "https://engine.example.com",
	})
	launcher := &agentLauncher{
		engine: e, turn: &turnctx.Turn{RunID: "t1", Seat: seat}, seat: seat,
		codingAgent: "claude-code", placement: sandbox.E2B,
	}
	err := launcher.LaunchExecutor(t.Context(), runner.AgentRunRequest{
		Brief: "fix the failing test", Round: 1,
		Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil),
	})
	if err == nil {
		t.Fatal("an agent-mode run launched on a node whose bridge no listener serves")
	}
	if !strings.Contains(err.Error(), "api.port") {
		t.Errorf("the refusal does not name the setting that fixes it: %v", err)
	}
	if e.bridge.Live() != 0 {
		t.Errorf("%d bridge sessions live for a run that was refused", e.bridge.Live())
	}
	if _, found, getErr := e.sandboxPending.Get(t.Context(), "t1"); getErr != nil || found {
		t.Errorf("a run row exists for a launch that was refused (found %v, err %v)", found, getErr)
	}
}

// splitLoginCompany is a one-seat epoch whose executor and whose code work
// run on DIFFERENT entries: a codex subscription — whose login cannot follow
// a run into a remote box — and an API-key entry, whose key can. Which one
// the seat's `llm` names and which its `llm_sandbox` names is the argument.
func splitLoginCompany(t *testing.T, executor, coder string) (*Company, *org.Role) {
	t.Helper()
	codex, err := cliagent.New(cliagent.Config{
		Key: "codex", Agent: "codex", AgentMode: true,
		StateDir: t.TempDir(), Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("cliagent.New: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{
		{Key: "codex", Provider: codex},
		{Key: "api", Provider: &answeringProvider{}},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	seat := &org.Role{
		Name: "SWE", LLM: org.ProviderKeys{executor}, LLMSandbox: org.ProviderKeys{coder},
	}
	return &Company{
		Models: models,
		Org:    &org.Organization{Name: "Acme", Roles: []*org.Role{seat}},
		Config: &config.Company{Providers: config.Providers{
			LLM: map[string]config.LLMProvider{
				"codex": {Type: config.LLMCLIAgent, CLI: &config.CLIAgent{
					Agent: "codex", Mode: config.CLIModeAgent, RunIn: config.PlacementE2B,
				}},
				"api": {Type: config.LLMAnthropic, Model: "claude-golden"},
			},
		}},
	}, seat
}

// THE LAUNCHER GUARDS THE EXECUTOR'S OWN LOGIN, because an agent-mode run IS
// the executor. Asked about llm_sandbox instead, the guard inspected a model
// the run never touches: a seat whose executor was a codex subscription and
// whose code work was an API key launched a remote box that failed at its
// first model call, minutes in, with the vendor's "not authenticated", and
// the mirror image, an API-key executor with a codex llm_sandbox, was refused
// a run that would have worked.
//
// Through LaunchExecutor rather than the guard directly: the guard honours
// whatever phase it is handed, and what is under test is which phase the
// launcher hands it.
func TestTheLauncherGuardsTheExecutorsOwnLogin(t *testing.T) {
	t.Parallel()
	request := func() runner.AgentRunRequest {
		return runner.AgentRunRequest{
			Brief: "fix the failing test", Round: 1,
			Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil),
		}
	}
	launcherFor := func(e *Engine, seat *org.Role) *agentLauncher {
		return &agentLauncher{
			engine: e, turn: &turnctx.Turn{RunID: "t1", Seat: seat}, seat: seat,
			codingAgent: "claude-code", placement: sandbox.E2B,
		}
	}

	// Executor on the subscription, code work on the key: refused, and the
	// session opened for the run is closed with the refusal.
	c, seat := splitLoginCompany(t, "codex", "api")
	e := launchReadyEngine(t, c)
	err := launcherFor(e, seat).LaunchExecutor(t.Context(), request())
	var credErr *SandboxCredentialError
	if !errors.As(err, &credErr) {
		t.Fatalf("an agent-mode run on a login that cannot follow it was launched "+
			"(err = %v): the launcher asked the guard about llm_sandbox", err)
	}
	if e.bridge.Live() != 0 {
		t.Errorf("%d bridge sessions live after a refused launch", e.bridge.Live())
	}

	// And the mirror image, or a launcher that always refused would pass.
	c, seat = splitLoginCompany(t, "api", "codex")
	e = launchReadyEngine(t, c)
	if err := launcherFor(e, seat).LaunchExecutor(t.Context(), request()); err != nil {
		t.Fatalf("an agent-mode run on an API-key executor was refused: %v", err)
	}
}

// A RUN RECORDS THE AGENT ID OF THE TURN THAT LAUNCHED IT. The id is derived
// from the company's name and the seat's handle, so an apply that renames the
// company mid-turn changes it; the run's durable row must carry the id of the
// organization the turn is pinned to, which is the id every other event of
// that turn carries, not the one the engine's current company would derive.
func TestARunRecordsTheAgentIDOfItsPinnedTurn(t *testing.T) {
	t.Parallel()
	pinned, seat := modeCompany(t, "claude-code", true, config.PlacementDirect)
	e := &Engine{}
	e.epoch.current.Store(&Company{
		Config: pinned.Config, Models: pinned.Models,
		Org: &org.Organization{Name: "Acme Renamed", Roles: []*org.Role{seat}},
	})
	launcher := &agentLauncher{
		engine: e, turn: &turnctx.Turn{RunID: "t1", Seat: seat, Org: pinned.Org}, seat: seat,
		codingAgent: "claude-code", placement: sandbox.Direct,
	}
	want, ok := org.DeriveAgentID("Acme", seat.Handle())
	if !ok {
		t.Fatal("the fixture derives no agent id")
	}
	if got := launcher.runTurnRef(t.Context()).AgentID; got != want.String() {
		t.Errorf("the run records agent id %q, want %q: the id of the turn's own organization", got, want)
	}
}

// A DELIVERY IN THE MIDDLE OF A LONG RUN IS ONE THE RESUME SEES.
//
// The run's row keeps a bounded list of a bridged run's calls, for older
// builds, that drops its middle once a run makes more than
// [sandbox.MaxBridgeCalls]. A resume that read that list could not see a Slack
// post made in the middle of a long run: the delivery check counted nobody
// reached, and a turn that had answered somebody could be sent round to answer
// them again. What the resume reads is every call.
func TestTheResumeSeesADeliveryInTheMiddleOfALongRun(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(coordmemory.NewFleet())
	ctx := t.Context()
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "t-long", AgentHandle: "swe"},
		sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	total := sandbox.MaxBridgeCalls + 50
	delivery := total / 2
	for i := range total {
		call := sandbox.BridgeCall{Name: "read_page", Output: "a page"}
		if i == delivery {
			call = sandbox.BridgeCall{Name: "slack_post", Args: `{"channel":"C1"}`, Output: "posted"}
		}
		if ok, err := store.AppendBridgeCall(ctx, "t-long", call); err != nil || !ok {
			t.Fatalf("append %d = %v, %v", i, ok, err)
		}
	}
	run, _, err := store.Get(ctx, "t-long")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// THE PREMISE: the row's own list lost the delivery. Were it still
	// there, this test could not tell the two sources apart.
	for _, kept := range run.BridgeCalls {
		if kept.Name == "slack_post" {
			t.Fatal("the row's bounded list kept the middle call, so this case proves nothing")
		}
	}

	e := &Engine{sandboxPending: store}
	calls, dropped, err := e.resumeBridged(ctx, resumeInput{
		Run: run, State: execstate.State{Version: execstate.Version, AgentRun: true, Round: 1},
	})
	if err != nil {
		t.Fatalf("resumeBridged: %v", err)
	}
	if len(calls) != total || dropped != (runner.DroppedCalls{}) {
		t.Fatalf("the resume read %d calls and %+v dropped, want all %d the run made and none dropped",
			len(calls), dropped, total)
	}
	surface := turn.Surface{Deliveries: map[string]string{"slack_post": "slack"}}
	if !turn.DeliveredTo(calls, surface, turn.ToolReply("slack")) {
		t.Error("the delivery check does not count the post the run made, so the turn would " +
			"be sent round to post it again")
	}
}

// failingCalls is a run store whose bridged-call log cannot be read.
type failingCalls struct{ sandbox.PendingStore }

func (failingCalls) BridgeCalls(context.Context, sandbox.PendingRun) (sandbox.BridgeLog, error) {
	return sandbox.BridgeLog{}, errors.New("coordination store unreachable")
}

// A LOG THAT CANNOT BE READ FAILS THE RESUME, which the coordinator hands back
// for a retry. Resumed on nothing instead, the turn would be judged to have
// reached nobody. A NATIVE resume reads no log at all, so it is not stopped by
// one it does not need.
func TestAResumeWhoseLogCannotBeReadIsHandedBack(t *testing.T) {
	t.Parallel()
	e := &Engine{sandboxPending: failingCalls{sandbox.NewCoordStore(coordmemory.NewFleet())}}
	in := resumeInput{Run: sandbox.PendingRun{TurnID: "t-1", LaunchID: "l-1"}}

	in.State = execstate.State{Version: execstate.Version, AgentRun: true, Round: 1}
	if _, _, err := e.resumeBridged(t.Context(), in); err == nil {
		t.Error("an agent-mode resume went ahead on a log it could not read")
	}
	in.State = execstate.State{Version: execstate.Version, Round: 1}
	if calls, _, err := e.resumeBridged(t.Context(), in); err != nil || calls != nil {
		t.Errorf("a native resume read the bridged log: %v, %v", calls, err)
	}
}

// resumeAgentRunTurn drives [Engine.resumeTurn] — the path a completion takes —
// over one agent-mode run on store, and returns the record the resumed
// executor pass published.
//
// The run's log ends in a no_action submission and nobody is waiting on the
// run, so the engine's own check ends the turn after that pass: nothing here
// reaches a model, and what is observed is only what the resume handed the
// pass.
func resumeAgentRunTurn(t *testing.T, store *sandbox.CoordStore, turnID string) *types.AgentPhaseCompleted {
	t.Helper()
	ctx := t.Context()
	admitted, seat := modeCompany(t, "claude-code", true, config.PlacementDirect)
	admitted.Tools = tools.NewRegistry()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	e, _ := resumingEngine(t)
	e.backends = &Backends{Queue: q}
	e.sandboxPending = store
	e.epoch.current.Store(admitted)

	run, found, err := store.Get(ctx, turnID)
	if err != nil || !found {
		t.Fatalf("Get(%s) = %v, %v", turnID, found, err)
	}
	err = e.resumeTurn(ctx, resumeInput{
		Company: admitted,
		Run:     run,
		State:   execstate.State{Version: execstate.Version, AgentRun: true, Round: 1},
		Turn:    &turnctx.Turn{RunID: run.TurnID, Seat: seat, Org: admitted.Org},
		Answer:  "nothing left to do",
	})
	if err != nil {
		t.Fatalf("resumeTurn: %v", err)
	}
	for _, ev := range q.History() {
		if rec, ok := ev.Data.(*types.AgentPhaseCompleted); ok && rec.Phase == types.PhaseExecute {
			return rec
		}
	}
	t.Fatal("the resumed executor pass published no record")
	return nil
}

// noActionSubmission is the call a run that found nothing to do ends with.
var noActionSubmission = sandbox.BridgeCall{
	Name: runner.SubmitWorkTool, Args: `{"outcome":"no_action","summary":"nothing left to do"}`,
}

// THE RESUME A COMPLETION RUNS READS EVERY CALL FROM THE LOG — not the bounded
// list on the run's row, which drops the middle of a long run.
//
// [Engine.resumeBridged] reading the log is not enough: resumeTurn is what
// hands its answer to the resumed pass, and a resumeTurn that took the row's
// list instead would pass every test of the helper while the pass it builds
// missed a post made in the middle of the run.
func TestAResumedTurnIsHandedEveryCallTheRunMade(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmemory.NewFleet())
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "t-long", AgentHandle: "swe"},
		sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	total := sandbox.MaxBridgeCalls + 50
	delivery := total / 2
	for i := range total {
		call := sandbox.BridgeCall{Name: "read_page", Output: "a page"}
		switch i {
		case delivery:
			call = sandbox.BridgeCall{Name: "slack_post", Args: `{"channel":"C1"}`, Output: "posted"}
		case total - 1:
			call = noActionSubmission
		}
		if ok, err := store.AppendBridgeCall(ctx, "t-long", call); err != nil || !ok {
			t.Fatalf("append %d = %v, %v", i, ok, err)
		}
	}
	run, _, err := store.Get(ctx, "t-long")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, kept := range run.BridgeCalls {
		if kept.Name == "slack_post" {
			t.Fatal("the row's bounded list kept the middle call, so this case proves nothing")
		}
	}

	rec := resumeAgentRunTurn(t, store, "t-long")
	if len(rec.ToolExecutions) != total {
		t.Fatalf("the resumed pass records %d calls, want all %d the run made", len(rec.ToolExecutions), total)
	}
	if got := rec.ToolExecutions[delivery]["name"]; got != "slack_post" {
		t.Errorf("call %d of the resumed pass is %v, want the post the run made there", delivery, got)
	}
}

// A RUN AN OLDER BUILD RECORDED REACHES ITS RESUME WITH ITS GAP. The calls
// that build dropped are kept nowhere, and the resumed pass's record is where
// a reader of the turn is told so — which it can only be if resumeTurn hands
// the count on rather than only the calls that survived.
func TestAResumedTurnIsToldWhatAnOlderBuildDropped(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmemory.NewFleet()
	kept := make([]sandbox.BridgeCall, 0, sandbox.MaxBridgeCalls)
	for range sandbox.MaxBridgeCalls - 1 {
		kept = append(kept, sandbox.BridgeCall{Name: "read_page"})
	}
	kept = append(kept, noActionSubmission)
	raw, err := json.Marshal(sandbox.PendingRun{
		TurnID: "t-older", AgentHandle: "swe", Status: sandbox.StatusResumed, LaunchID: "launch-older",
		BridgeCalls: kept, BridgeCallsElided: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.CreateSandboxRun(ctx, "t-older", raw); err != nil {
		t.Fatalf("CreateSandboxRun: %v", err)
	}

	rec := resumeAgentRunTurn(t, sandbox.NewCoordStore(fleet), "t-older")
	if len(rec.ToolExecutions) != sandbox.MaxBridgeCalls {
		t.Errorf("the resumed pass records %d calls, want the %d the row kept",
			len(rec.ToolExecutions), sandbox.MaxBridgeCalls)
	}
	want := fmt.Sprintf("41 call(s) this run made after its first %d", sandbox.MaxBridgeCalls/2)
	if !strings.Contains(rec.Notes, want) {
		t.Errorf("the resumed pass's record does not say where the dropped calls fell: notes = %q", rec.Notes)
	}
}

// ARGUMENTS THE STORE COULD NOT KEEP REACH THE RESUME AS THEIR MARKER.
//
// A call recorded with no arguments reads, to the resumed phase, the reviewer
// and the iteration ledger, exactly like a call made with none. The marker the
// store writes in their place decodes, so every one of those readers shows it
// where the arguments would be, beside the call's output.
func TestArgumentsTheStoreDidNotKeepReachTheResumeAsTheirMarker(t *testing.T) {
	t.Parallel()
	calls := bridgedCalls([]sandbox.BridgeCall{{
		Name: "create_page", Args: sandbox.ArgsNotKept(9 << 20), Output: "created 123",
	}})
	if len(calls) != 1 || calls[0].Result != "created 123" {
		t.Fatalf("calls = %+v, want the one call with its output", calls)
	}
	marker, _ := calls[0].Args["…"].(string)
	if !strings.Contains(marker, fmt.Sprint(9<<20)) || !strings.Contains(marker, "not kept") {
		t.Fatalf("the call's arguments = %v, want the marker saying %d bytes were not kept", calls[0].Args, 9<<20)
	}
	if log := ledger.FormatCalls(calls, ledger.FormatOptions{}); !strings.Contains(log, "not kept") {
		t.Errorf("the reviewer's tool log does not show the marker: %s", log)
	}
}
