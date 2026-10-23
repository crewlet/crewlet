package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// codeWorkRig is an engine whose run_sandbox launches reach a fake remote
// cell with BOTH runners registered — Claude Code the catalogue's default —
// so what a launch is refused or started with is the engine's decision and
// never a missing runner's.
func codeWorkRig(t *testing.T, c *Company) (*Engine, map[string]*sandbox.FakeRunner) {
	t.Helper()
	jobs := map[string]*sandbox.FakeRunner{
		codingagent.ClaudeCodeName: sandbox.NewFakeRunner(codingagent.ClaudeCodeName),
		codingagent.OpenCodeName:   sandbox.NewFakeRunner(codingagent.OpenCodeName),
	}
	runners := map[string]sandbox.Runner{}
	for name, runner := range jobs {
		runners[name] = runner
	}
	// THE COMPANY'S OWN PROVIDER-WIDE STEPS, as buildSandbox hands them to
	// the manager, so a launch here provisions what a node's would.
	var defaults []sandbox.SetupStep
	if spec := c.Config.Providers.Sandbox; spec != nil {
		defaults = setupSteps(spec.Setup)
	}
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:          map[sandbox.Placement]sandbox.Provider{sandbox.E2B: sandbox.NewFakeProvider()},
		Runners:            runners,
		DefaultCodingAgent: codingagent.ClaudeCodeName,
		DefaultSetup:       defaults,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	q := memory.New()
	pending := sandbox.NewCoordStore(coordmemory.NewFleet())
	e := &Engine{backends: &Backends{Queue: q}}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Audience: noAudience{},
		Queue:    q, Pending: pending, Manager: manager, Resume: &resumer{engine: e},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	e.useSandbox(pending, coordinator)
	e.epoch.current.Store(c)
	return e, jobs
}

// runnableCompany is the epoch a node builds from doc: held to the RUNNABLE
// rules alone, as an apply holds a revision another peer admitted, so a
// pairing the write path refuses still reaches the launch that must refuse it
// again.
func runnableCompany(t *testing.T, doc string) *Company {
	t.Helper()
	cfg, err := config.ParseCompanyDocument([]byte(doc))
	if err == nil {
		err = cfg.ValidateRunnable()
	}
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	built, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	return built
}

// THE run_sandbox LAUNCH REFUSES A RUNNER THAT CANNOT READ ITS ENTRY, BEFORE A
// BOX EXISTS.
//
// An OpenCode text seat that enables a sandbox and names no coding agent runs
// its code work under Claude Code, the catalogue's default, which would be
// handed `--model anthropic/claude-sonnet-5` and an OpenCode sign-in it never
// reads — a box minted, an agent installed, and a failure at its first model
// call. Asserted through the launcher rather than the resolver, so a launch
// that stopped asking, or asked after the box was minted, fails here; and the
// same seat naming its own CLI's runner launches, or a launcher refusing every
// entry would pass.
func TestARunSandboxLaunchRefusesARunnerThatCannotReadItsEntry(t *testing.T) {
	t.Setenv(config.CLIHomeEnv, t.TempDir())
	for _, tc := range []struct {
		name, codingAgent string
		refused           bool
	}{
		{"under the default runner", "", true},
		{"under its own CLI's runner", "coding_agent: opencode, ", false},
	} {
		c := runnableCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    oc:
      agent: opencode
      model: anthropic/claude-sonnet-5
      env: {ANTHROPIC_API_KEY: sk-ant-not-real}
roles:
  - name: Writer
    handle: writer
    llm: oc
    sandbox: {`+tc.codingAgent+`enabled: true}
`)
		e, jobs := codeWorkRig(t, c)
		seat := seatNamed(t, c, "Writer")
		_, err := (&launcher{engine: e}).Launch(t.Context(),
			&turnctx.Turn{RunID: "t1", Seat: seat}, "fix the failing test")
		started := len(jobs[codingagent.ClaudeCodeName].Started()) + len(jobs[codingagent.OpenCodeName].Started())
		var modelErr *SandboxModelError
		if !tc.refused {
			if err != nil {
				t.Fatalf("%s: the launch was refused: %v", tc.name, err)
			}
			if got := jobs[codingagent.OpenCodeName].Started(); len(got) != 1 ||
				got[0].Model != "anthropic/claude-sonnet-5" {
				t.Errorf("%s: OpenCode started %+v, want one run on the entry's model as written", tc.name, got)
			}
			continue
		}
		if !errors.As(err, &modelErr) {
			t.Fatalf("%s: err = %v (%T), want *SandboxModelError", tc.name, err, err)
		}
		// A PAIRING NO WAIT CHANGES, which the tool tells its model as one a
		// person's change clears.
		if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
			t.Errorf("%s: the refusal is classed %q (%v), want %q", tc.name, why, ok,
				sandbox.WhyConfiguration)
		}
		if started != 0 {
			t.Errorf("%s: %d jobs started for a refused launch", tc.name, started)
		}
		if _, found, getErr := e.sandbox.Load().pending.Get(t.Context(), "t1"); getErr != nil || found {
			t.Errorf("%s: a run row exists for a refused launch (found %v, err %v)", tc.name, found, getErr)
		}
	}
}

// A LAUNCH NO WAIT CAN START IS CLASSED AS ONE, so the run_sandbox tool tells
// its model what to do instead rather than to try again: a seat whose sandbox
// is off and a node that runs none are a person's configuration to change, and
// a seat that does its code work in its own session is told to use the shell it
// already has. Each is refused before a box exists.
//
// Mutation: drop the class from any of the three refusals, and its case goes
// red.
func TestARunSandboxLaunchClassesWhatNoWaitClears(t *testing.T) {
	t.Setenv(config.CLIHomeEnv, t.TempDir())
	c := runnableCompany(t, fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    main:
      model: claude-sonnet-5
    sub:
      agent: claude-code
      model: sonnet
      mode: box
      state_dir: %q
roles:
  - name: Self
    handle: self
    llm: sub
    sandbox: {enabled: true, run_in: self}
  - name: Idle
    handle: idle
    llm: main
    sandbox: {enabled: false}
`, t.TempDir()))
	e, jobs := codeWorkRig(t, c)
	for handle, want := range map[string]sandbox.Why{
		"self": sandbox.WhyOwnSession,
		"idle": sandbox.WhyConfiguration,
	} {
		_, err := (&launcher{engine: e}).Launch(t.Context(),
			&turnctx.Turn{RunID: "t-" + handle, Seat: c.Org.AgentSeatByHandle(handle)}, "fix it")
		if why, ok := sandbox.WhyOf(err); !ok || why != want {
			t.Errorf("%s: the refusal %v is classed %q (%v), want %q", handle, err, why, ok, want)
		}
	}
	if started := len(jobs[codingagent.ClaudeCodeName].Started()); started != 0 {
		t.Errorf("%d jobs started for refused launches", started)
	}

	// A NODE THAT RUNS NO SANDBOX AT ALL.
	bare := &Engine{}
	bare.epoch.current.Store(c)
	_, err := (&launcher{engine: bare}).Launch(t.Context(),
		&turnctx.Turn{RunID: "t-bare", Seat: c.Org.AgentSeatByHandle("idle")}, "fix it")
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
		t.Errorf("a node with no sandbox: the refusal %v is classed %q (%v), want %q", err, why, ok,
			sandbox.WhyConfiguration)
	}
}
