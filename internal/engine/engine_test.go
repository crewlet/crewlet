package engine_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/toolsession"
)

const companyDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
    alpha:
      model: claude-haiku-4-5
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: CTO
    handle: cto
    llm: alpha
  - name: Founder
    kind: human
    contact:
      slack_user_id: U0FOUNDER
`

func company(t *testing.T, doc string) *engine.Company {
	t.Helper()
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.NewCompany(c)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	return e
}

func TestAnEpochBuildsWithoutReachingTheNetwork(t *testing.T) {
	t.Parallel()
	// Building an epoch must be something a `validate` command can do. A
	// constructor that dialled a provider would make config validation
	// depend on the vendor being up.
	c := company(t, companyDoc)
	if c.Org == nil || c.Models == nil || c.Tools == nil {
		t.Fatalf("epoch = %+v", c)
	}
	if got := c.Models.Keys(); !slices.Equal(got, []string{"zulu", "alpha"}) {
		t.Errorf("provider order = %v, want the declared order", got)
	}
}

func TestOnlyAgentSeatsArePlaced(t *testing.T) {
	t.Parallel()
	// A human seat is addressable and never spawned. Including one would
	// make the fleet try to claim a lease for something no node can run,
	// and then report the company permanently under capacity.
	c := company(t, companyDoc)
	var handles []string
	for _, s := range c.Seats() {
		handles = append(handles, s.Handle)
	}
	if !slices.Equal(handles, []string{"ceo", "cto"}) {
		t.Errorf("seats = %v, want the two agent seats, sorted", handles)
	}
}

func TestTheSeatListIsStable(t *testing.T) {
	t.Parallel()
	// It feeds the placement math, and the sweep compares its own answer
	// across ticks. A fleet that reshuffled its eligibility list every tick
	// would churn seats for no reason.
	c := company(t, companyDoc)
	first := c.Seats()
	for range 30 {
		if got := c.Seats(); len(got) != len(first) || got[0].Handle != first[0].Handle {
			t.Fatalf("seat order is unstable: %v then %v", first, got)
		}
	}
}

// noLaunch is an executor launcher no case here launches through: a runner is
// built only with one, and these cases are about building it.
type noLaunch struct{}

func (noLaunch) LaunchExecutor(context.Context, runner.AgentRunRequest) (runner.Launched, error) {
	return runner.Launched{}, nil
}

func TestARunnerIsBuiltPerSeat(t *testing.T) {
	t.Parallel()
	c := company(t, companyDoc)
	if _, err := c.RunnerFor("ceo", nil, engine.RunnerInput{
		Task: "post it", Reply: turn.NoReply(), Launcher: noLaunch{}, Sessions: toolSessions(t),
	}); err != nil {
		t.Errorf("RunnerFor(ceo): %v", err)
	}
	// AND NEVER WITHOUT A LAUNCHER: every executor is a launched run, so a
	// runner assembled with none is refused where it is assembled, naming
	// what is missing, rather than failing the seat's first turn.
	_, err := c.RunnerFor("ceo", nil, engine.RunnerInput{Task: "post it", Reply: turn.NoReply(),
		Sessions: toolSessions(t)})
	if err == nil || !strings.Contains(err.Error(), "Launcher") {
		t.Errorf("a runner with no executor launcher = %v, want it refused naming the launcher", err)
	}
	// A human seat has no runner, and the refusal must name the seat —
	// "not an agent seat" sent to the wrong place is a debugging session.
	_, err = c.RunnerFor("founder", nil, engine.RunnerInput{})
	if err == nil {
		t.Fatal("a human seat got a runner")
	}
	if !strings.Contains(err.Error(), "founder") {
		t.Errorf("the refusal does not name the seat: %v", err)
	}
	if _, err := c.RunnerFor("nobody", nil, engine.RunnerInput{}); err == nil {
		t.Error("an unknown handle got a runner")
	}
}

func TestTurnSettingsComeFromTheEpochNotALiveCell(t *testing.T) {
	t.Parallel()
	// An in-flight turn holds the epoch it started under until it ends,
	// which is what makes the mid-turn config swap unrepresentable rather
	// than merely guarded against.
	c := company(t, companyDoc+"\nturn_engine:\n  max_iterations: 7\n  delegation_depth_limit: 2\n")
	got := c.TurnSettings(time.Time{})
	if got.MaxIterations != 7 || got.DelegationDepthLimit != 2 {
		t.Errorf("settings = %+v", got)
	}
	if !slices.Contains(got.SkipNames, toolname.Activate) {
		t.Errorf("skip names = %v, want the meta-tools", got.SkipNames)
	}
	// The wall-clock cap comes off the TURN, not the epoch: a scheduled fire
	// carries one, fixed when the turn began, and every other trigger none.
	if !got.Deadline.IsZero() {
		t.Errorf("an uncapped turn produced a deadline of %s", got.Deadline)
	}
	deadline := time.Date(2026, 9, 1, 9, 3, 0, 0, time.UTC)
	if capped := c.TurnSettings(deadline); !capped.Deadline.Equal(deadline) {
		t.Errorf("Deadline = %s, want the turn's %s", capped.Deadline, deadline)
	}
}

func TestAnEmptyCompanyIsRefusedRatherThanHalfBuilt(t *testing.T) {
	t.Parallel()
	if _, err := engine.NewCompany(nil); err == nil {
		t.Error("a nil config built an epoch")
	}
}

func TestAnUnvalidatedConfigIsValidatedHere(t *testing.T) {
	t.Parallel()
	// A Company is an exported struct an embedder can build directly, so
	// the invariants everything below relies on are only guaranteed if this
	// checks them. An epoch assembled from an invalid config is a company
	// that boots and then fails at its first turn.
	// A company-level rule, deliberately: an empty name is ALSO caught
	// building the organization, so it proves nothing about this check. A
	// negative token budget is refused by Company.Validate alone — found by
	// mutation, where removing the validation still failed the empty-name
	// case and looked fine.
	handbuilt := config.DefaultCompany()
	handbuilt.Name = "Acme"
	negative := -1
	handbuilt.TokenBudget = config.TokenBudget{Day: &negative}
	handbuilt.Providers.LLM = map[string]config.LLMProvider{
		"a": {Model: "m"},
	}
	handbuilt.Providers.Sandbox = &config.SandboxProvider{Fake: true}
	handbuilt.Roles = []config.Role{{Name: "CEO", Handle: "ceo"}}
	if _, err := engine.NewCompany(&handbuilt); err == nil {
		t.Error("an invalid hand-built config produced an epoch")
	}

	// The counterfactual: the same config with a legal budget builds.
	// Without it this passes for a constructor that refuses everything
	// hand-built.
	handbuilt.TokenBudget = config.TokenBudget{}
	if _, err := engine.NewCompany(&handbuilt); err != nil {
		t.Errorf("a valid hand-built config was refused: %v", err)
	}
}

func TestTheExtensionJudgeIsOnUnlessTurnedOff(t *testing.T) {
	t.Parallel()
	// The config documents it as on by default, and an unset Toggle is
	// exactly the case that decides it: reading a zero Toggle as false
	// would disable round-cap extensions for every company that never
	// mentioned them.
	c := company(t, companyDoc)
	if got, err := c.RunnerFor("ceo", nil, engine.RunnerInput{Reply: turn.NoReply(),
		Launcher: noLaunch{}, Sessions: toolSessions(t)}); err != nil {
		t.Fatalf("RunnerFor: %v", err)
	} else if !got.Caps().ExtensionOn {
		t.Error("an unset extension_enabled disabled the round-cap judge")
	}
	off := company(t, companyDoc+"\nturn_engine:\n  extension_enabled: false\n")
	got, err := off.RunnerFor("ceo", nil, engine.RunnerInput{Reply: turn.NoReply(),
		Launcher: noLaunch{}, Sessions: toolSessions(t)})
	if err != nil {
		t.Fatalf("RunnerFor: %v", err)
	}
	if got.Caps().ExtensionOn {
		t.Error("an explicit false did not disable the round-cap judge")
	}
}

func TestAProviderBuildsWithoutALogin(t *testing.T) {
	// It must BUILD without credentials: the call then fails with the
	// vendor's own "not authenticated", which names the CLI, while a
	// constructor that refused to exist would take the whole company down
	// at boot over one provider's login.
	t.Setenv(config.CLIHomeEnv, t.TempDir())
	c, err := config.ParseCompany([]byte(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    local:
      model: sonnet
      agent: claude-code
roles:
  - name: CEO
    handle: ceo
    llm: local
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := engine.NewCompany(c); err != nil {
		t.Fatalf("a provider with no login refused to build: %v", err)
	}
}

// The state directory is derived PER PROVIDER KEY, which is what makes
// sharing a login an explicit act: two unrelated entries landing on one
// directory would prune each other's seat homes.
func TestStateDirsAreDerivedPerProviderKey(t *testing.T) {
	root := t.TempDir()
	t.Setenv(config.CLIHomeEnv, root)

	var entry config.LLMProvider
	opus, err := entry.ResolvedStateDir("opus-sub")
	if err != nil {
		t.Fatalf("ResolvedStateDir: %v", err)
	}
	sonnet, err := entry.ResolvedStateDir("sonnet-sub")
	if err != nil {
		t.Fatalf("ResolvedStateDir: %v", err)
	}
	if opus == sonnet {
		t.Fatalf("two provider keys derived one state dir %q", opus)
	}
	if !strings.HasPrefix(opus, root) {
		t.Errorf("state dir %q is not under %s=%q", opus, config.CLIHomeEnv, root)
	}

	// An explicit state_dir is honoured verbatim — that is how two entries
	// deliberately share one login.
	shared := config.LLMProvider{StateDir: "/var/lib/crewlet/llm-cli/claude"}
	for _, key := range []string{"opus-sub", "sonnet-sub"} {
		got, err := shared.ResolvedStateDir(key)
		if err != nil {
			t.Fatalf("ResolvedStateDir: %v", err)
		}
		if got != "/var/lib/crewlet/llm-cli/claude" {
			t.Errorf("ResolvedStateDir(%q) = %q, want the configured directory", key, got)
		}
	}
}

// The documented fallback: an entry that names no token still gets one from
// the profile's own variable, through the resolver — which reads the secret
// store BEFORE the environment. Without it, every operator would have to wire
// up a ${VAR} by hand for a value `crewlet llm login -capture-token` just
// wrote into the store itself.
func TestAnEntryReadsItsConventionalTokenWhenItNamesNone(t *testing.T) {
	t.Setenv(config.CLIHomeEnv, t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-from-the-convention")

	c, err := config.ParseCompany([]byte(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    subscription:
      model: sonnet
      agent: claude-code
roles:
  - name: CEO
    handle: ceo
    llm: subscription
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	built, err := engine.BuildCLIAgent("subscription", c.Providers.LLM["subscription"], config.EnvOnly(), nil)
	if err != nil {
		t.Fatalf("BuildCLIAgent: %v", err)
	}
	if got := built.SignInState(t.Context()); got != "token" {
		t.Errorf("SignInState = %q, want token — the conventional variable was not read", got)
	}
}

// toolSessions is the session directory a runner serves its phases' tools
// from, for one case, removed when it ends: what the engine hands every
// runner it builds ([engine.RunnerInput.Sessions]).
func toolSessions(t *testing.T) *toolsession.Host {
	t.Helper()
	host, err := toolsession.NewHost()
	if err != nil {
		t.Fatalf("toolsession.NewHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host
}
