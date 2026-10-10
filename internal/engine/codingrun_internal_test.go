package engine

import (
	"errors"
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
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:          map[sandbox.Placement]sandbox.Provider{sandbox.E2B: sandbox.NewFakeProvider()},
		Runners:            runners,
		DefaultCodingAgent: codingagent.ClaudeCodeName,
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
// cli-agent entry would pass.
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
      type: cli-agent
      model: anthropic/claude-sonnet-5
      cli: {agent: opencode, env: {ANTHROPIC_API_KEY: sk-ant-not-real}}
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
				got[0].LLM == nil || got[0].LLM.Model != "anthropic/claude-sonnet-5" {
				t.Errorf("%s: OpenCode started %+v, want one run on the entry's model as written", tc.name, got)
			}
			continue
		}
		if !errors.As(err, &modelErr) {
			t.Fatalf("%s: err = %v (%T), want *SandboxModelError", tc.name, err, err)
		}
		if started != 0 {
			t.Errorf("%s: %d jobs started for a refused launch", tc.name, started)
		}
		if _, found, getErr := e.sandbox.Load().pending.Get(t.Context(), "t1"); getErr != nil || found {
			t.Errorf("%s: a run row exists for a refused launch (found %v, err %v)", tc.name, found, getErr)
		}
	}
}
