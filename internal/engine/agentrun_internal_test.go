package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
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
				CLI:  &config.CLIAgent{Agent: agent, Mode: mode, RunIn: runIn},
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
	if got := e.agentRunFor(c, "swe", &turnctx.Turn{ID: "t1"}); got != nil {
		t.Fatalf("a text-mode seat got %T, want a nil interface", got)
	}
	if c.AgentModeSeat(c.Org.Roles[0]) {
		t.Error("a text-mode seat reports itself in agent mode")
	}
}

// AN AGENT-MODE SEAT GETS ONE, carrying the CLI and the cell resolved from its
// own entry — resolved ONCE, before the turn, so an apply landing mid-turn
// cannot move a run that is already going.
func TestAnAgentModeSeatGetsALauncherForItsOwnCLIAndCell(t *testing.T) {
	t.Parallel()
	c, seat := modeCompany(t, "opencode", true, config.PlacementE2B)
	e := &Engine{}
	got := e.agentRunFor(c, "swe", &turnctx.Turn{ID: "t1"})
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
	if !c.AgentModeSeat(seat) {
		t.Error("an agent-mode seat does not report itself in agent mode")
	}
}

// AN ENTRY THAT NAMES NO CELL DEFERS TO THE CATALOGUE, spelled once: the empty
// placement reaches the manager, which resolves it to the company default. A
// second fallback here would be a second answer to that question.
func TestAnAgentEntryWithNoCellDefersToTheCatalogue(t *testing.T) {
	t.Parallel()
	c, _ := modeCompany(t, "claude-code", true, "")
	e := &Engine{}
	launcher, _ := e.agentRunFor(c, "swe", &turnctx.Turn{ID: "t1"}).(*agentLauncher)
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
		engine: e, turn: &turnctx.Turn{ID: "t1", Seat: seat}, seat: seat,
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
	seatServers := map[string]map[string]any{
		"jira": {"type": "stdio", "command": "jira-mcp"},
	}
	first := withBridge(seatServers, "https://engine.example.com/mcp/tok-1")
	if _, leaked := seatServers["crewlet"]; leaked {
		t.Fatal("the bridge entry was written into the seat's own map")
	}
	if first["crewlet"]["url"] != "https://engine.example.com/mcp/tok-1" {
		t.Errorf("the copy does not carry the endpoint: %v", first["crewlet"])
	}
	second := withBridge(seatServers, "https://engine.example.com/mcp/tok-2")
	if second["crewlet"]["url"] == first["crewlet"]["url"] {
		t.Error("a second run reused the first run's dead endpoint")
	}
	// The seat's own servers survive into both, because a bridged run
	// still needs the credentials only its own MCP children hold.
	for _, out := range []map[string]map[string]any{first, second} {
		if out["jira"]["command"] != "jira-mcp" {
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
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Queue: q, Pending: pending, Manager: manager,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	e := &Engine{
		backends:           &Backends{Queue: q},
		sandboxCoordinator: coordinator,
		sandboxPending:     pending,
		bridge: mcpbridge.New(mcpbridge.Options{
			Key: []byte("test-key"), BaseURL: "https://engine.example.com",
		}),
	}
	e.epoch.current.Store(c)
	return e
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
// first model call, minutes in, with the vendor's "not authenticated" — and
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
			engine: e, turn: &turnctx.Turn{ID: "t1", Seat: seat}, seat: seat,
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
