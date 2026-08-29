package engine_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
)

const companyDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
    alpha:
      type: anthropic
      model: claude-haiku-4-5
      api_keys: ["${K}"]
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

func TestARunnerIsBuiltPerSeat(t *testing.T) {
	t.Parallel()
	c := company(t, companyDoc)
	if _, err := c.RunnerFor("ceo", engine.RunnerInput{Task: "post it"}); err != nil {
		t.Errorf("RunnerFor(ceo): %v", err)
	}
	// A human seat has no runner, and the refusal must name the seat —
	// "not an agent seat" sent to the wrong place is a debugging session.
	_, err := c.RunnerFor("founder", engine.RunnerInput{})
	if err == nil {
		t.Fatal("a human seat got a runner")
	}
	if !strings.Contains(err.Error(), "founder") {
		t.Errorf("the refusal does not name the seat: %v", err)
	}
	if _, err := c.RunnerFor("nobody", engine.RunnerInput{}); err == nil {
		t.Error("an unknown handle got a runner")
	}
}

func TestTurnSettingsComeFromTheEpochNotALiveCell(t *testing.T) {
	t.Parallel()
	// An in-flight turn holds the epoch it started under until it ends,
	// which is what makes the mid-turn config swap unrepresentable rather
	// than merely guarded against.
	c := company(t, companyDoc+"\nturn_engine:\n  max_iterations: 7\n  delegation_depth_limit: 2\n")
	got := c.TurnSettings()
	if got.MaxIterations != 7 || got.DelegationDepthLimit != 2 {
		t.Errorf("settings = %+v", got)
	}
	if !slices.Contains(got.SkipNames, "activate_tool") {
		t.Errorf("skip names = %v, want the meta-tools", got.SkipNames)
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
	handbuilt.TokenBudget = -1
	handbuilt.Providers.LLM = map[string]config.LLMProvider{
		"a": {Type: config.LLMAnthropic, Model: "m", APIKeys: []string{"k"}},
	}
	handbuilt.Roles = []config.Role{{Name: "CEO", Handle: "ceo"}}
	if _, err := engine.NewCompany(&handbuilt); err == nil {
		t.Error("an invalid hand-built config produced an epoch")
	}

	// The counterfactual: the same config with a legal budget builds.
	// Without it this passes for a constructor that refuses everything
	// hand-built.
	handbuilt.TokenBudget = 0
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
	if got, err := c.RunnerFor("ceo", engine.RunnerInput{}); err != nil {
		t.Fatalf("RunnerFor: %v", err)
	} else if !got.Caps().ExtensionOn {
		t.Error("an unset extension_enabled disabled the round-cap judge")
	}
	off := company(t, companyDoc+"\nturn_engine:\n  extension_enabled: false\n")
	got, err := off.RunnerFor("ceo", engine.RunnerInput{})
	if err != nil {
		t.Fatalf("RunnerFor: %v", err)
	}
	if got.Caps().ExtensionOn {
		t.Error("an explicit false did not disable the round-cap judge")
	}
}

func TestACLIAgentProviderBuildsWithoutALogin(t *testing.T) {
	// Like every other backend, it must BUILD without credentials: the
	// call then fails with the vendor's own "not authenticated", which
	// names the CLI, while a constructor that refused to exist would take
	// the whole company down at boot over one provider's login.
	t.Setenv(config.CLIHomeEnv, t.TempDir())
	c, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    local:
      type: cli-agent
      model: sonnet
      cli:
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
		t.Fatalf("a cli-agent provider with no login refused to build: %v", err)
	}
}

// The state directory is derived PER PROVIDER KEY, which is what makes
// sharing a login an explicit act: two unrelated entries landing on one
// directory would prune each other's seat homes.
func TestCLIAgentStateDirsAreDerivedPerProviderKey(t *testing.T) {
	root := t.TempDir()
	t.Setenv(config.CLIHomeEnv, root)

	var cli config.CLIAgent
	opus, err := cli.ResolvedStateDir("opus-sub")
	if err != nil {
		t.Fatalf("ResolvedStateDir: %v", err)
	}
	sonnet, err := cli.ResolvedStateDir("sonnet-sub")
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
	shared := config.CLIAgent{StateDir: "/var/lib/crewlet/llm-cli/claude"}
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

func TestACompanyWithNoModelsIsRefusedAtBuild(t *testing.T) {
	t.Parallel()
	// The org chart authored before its credentials PARSES — that is a
	// documented state — but it cannot run a turn, and discovering that
	// when a seat tries to think reports it as a nil provider deep in a
	// phase.
	c, err := config.ParseCompany([]byte("name: Acme\nroles:\n  - name: CEO\n    handle: ceo\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := engine.NewCompany(c); err == nil {
		t.Error("a company with no models built an epoch that cannot run a turn")
	}
}

// The documented fallback: an entry that names no token still gets one from
// the profile's own variable, through the resolver — which reads the secret
// store BEFORE the environment. Without it, every operator would have to wire
// up a ${VAR} by hand for a value `crewlet llm login -capture-token` just
// wrote into the store itself.
func TestACLIAgentReadsItsConventionalTokenWhenTheEntryNamesNone(t *testing.T) {
	t.Setenv(config.CLIHomeEnv, t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-from-the-convention")

	c, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    subscription:
      type: cli-agent
      model: sonnet
      cli:
        agent: claude-code
roles:
  - name: CEO
    handle: ceo
    llm: subscription
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	built, err := engine.BuildCLIAgent("subscription", c.Providers.LLM["subscription"], config.EnvOnly())
	if err != nil {
		t.Fatalf("BuildCLIAgent: %v", err)
	}
	if got := built.LoginState(); got != "token" {
		t.Errorf("LoginState = %q, want token — the conventional variable was not read", got)
	}
}
