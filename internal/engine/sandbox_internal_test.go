package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/queue"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/version"
)

// The knob has three states and the manager's input carries a pointer for
// exactly that reason. Getting this wrong is not visible as a config error: a
// seat silently loses its checkout the moment a harness asks a question,
// which is the case a paused box exists for.
func TestTheSeatsPauseOverrideDistinguishesInheritFromNever(t *testing.T) {
	t.Parallel()
	never := 0.0
	held := 600.0

	cases := []struct {
		name string
		gate org.RoleSandbox
		want *time.Duration
	}{
		{"unset inherits", org.RoleSandbox{}, nil},
		{"an explicit zero never pauses", org.RoleSandbox{PauseTTLSeconds: &never}, dur(0)},
		{"a set value is used", org.RoleSandbox{PauseTTLSeconds: &held}, dur(600 * time.Second)},
	}
	for _, c := range cases {
		got := pauseTTL(&c.gate)
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%s: got %v, want inherit", c.name, *got)
		case c.want != nil && got == nil:
			t.Errorf("%s: got inherit, want %v", c.name, *c.want)
		case c.want != nil && got != nil && *got != *c.want:
			t.Errorf("%s: got %v, want %v", c.name, *got, *c.want)
		}
	}
}

func dur(d time.Duration) *time.Duration { return &d }

// companyFor parses a company document and builds its epoch, so a test can
// ask what a real seat resolves to rather than hand-assembling one.
func companyFor(t *testing.T, doc string) *Company {
	t.Helper()
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	built, err := NewCompany(c)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	return built
}

func seatNamed(t *testing.T, c *Company, name string) *org.Role {
	t.Helper()
	for _, role := range c.Org.Roles {
		if role.Name == name {
			return role
		}
	}
	t.Fatalf("no role %q", name)
	return nil
}

// runOn is what seat's executor launches with on the head of its
// chain, over the given seat environment.
func runOn(t *testing.T, c *Company, seat *org.Role, seatEnv map[string]string) executorRun {
	t.Helper()
	member, err := c.Models.Head(seat, phase.Execute)
	if err != nil {
		t.Fatalf("%s's executor chain: %v", seat.Name, err)
	}
	return newExecutorRun(member.Provider.(*harness.Provider), runEnv{env: seatEnv})
}

// AN API-KEY ENTRY'S BOX IS HANDED THE KEY ITS OWN CALLS USE, RESOLVED.
//
// Under auth.mode api-key an entry signs its harness in with one metered key, and
// a launch on that entry has to carry it: Claude Code in a box reads only
// its environment, so a run that left the key behind started signed in to
// nothing. Model and key are the epoch's resolution, the one its seats' own
// calls are made with, so a `${LLM_MODEL}` reaches the box as its value rather
// than that literal. And the mode decides which credential travels: a plan
// token the engine's own environment happens to hold stays out of an api-key
// entry's box, or the run would be signed in two ways at once.
func TestAnAPIKeyEntrysBoxCarriesItsResolvedKey(t *testing.T) {
	t.Setenv(config.HarnessHomeEnv, t.TempDir())
	t.Setenv("LLM_MODEL", "claude-sonnet-5")
	t.Setenv("ENTRY_KEY", "sk-ant-entry")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-not-this-entrys")
	c := companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    claude:
      model: ${LLM_MODEL}
      auth: {mode: api-key, api_key: "${ENTRY_KEY}"}
roles:
  - name: Engineer
    handle: eng
    llm: claude
`)
	run := runOn(t, c, seatNamed(t, c, "Engineer"), nil)
	if run.model != "claude-sonnet-5" {
		t.Errorf("the box's model = %q, want the resolved model", run.model)
	}
	// TZ rides with it: the zone the engine reads the run's limit reset in.
	want := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-entry", "TZ": run.env["TZ"]}
	if run.env["TZ"] == "" || !maps.Equal(run.env, want) {
		t.Errorf("the run environment = %v, want exactly %v", run.env, want)
	}
}

// THE ENTRY'S SIGN-IN IS ONE UNIT WHERE THE SEAT BROUGHT ITS OWN.
//
// Underlaid by name, an api-key entry's key is added beside whatever the seat
// named in role.sandbox.env: Claude Code ranks ANTHROPIC_API_KEY above a
// plan's CLAUDE_CODE_OAUTH_TOKEN, so a seat billing its own Pro/Max plan is
// moved onto the company's metered key, and a seat on a cloud provider's own
// credentials carries a company key into a box that never reads it. When the
// seat's environment signs Claude Code in, the entry adds nothing.
func TestASeatsOwnClaudeCodeSignInKeepsTheEntrysOut(t *testing.T) {
	t.Setenv(config.HarnessHomeEnv, t.TempDir())
	t.Setenv("ENTRY_KEY", "sk-ant-company")
	c := companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    claude:
      model: claude-sonnet-5
      auth: {mode: api-key, api_key: "${ENTRY_KEY}"}
roles:
  - name: Engineer
    handle: eng
    llm: claude
`)
	seat := seatNamed(t, c, "Engineer")
	e := &Engine{}
	for _, own := range []map[string]string{
		{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-plan"},
		{"ANTHROPIC_API_KEY": "sk-ant-seat"},
		{"CLAUDE_CODE_USE_BEDROCK": "1"},
	} {
		run := runOn(t, c, seat,
			e.runEnvironment(t.Context(), nil, seat, &org.RoleSandbox{Env: own}, nil, nil).env)
		for name, value := range own {
			if run.env[name] != value {
				t.Errorf("%v: %s = %q, want the seat's own", own, name, run.env[name])
			}
		}
		if run.env["ANTHROPIC_API_KEY"] == "sk-ant-company" {
			t.Errorf("%v: the company key was added beside the seat's own sign-in", own)
		}
	}

	// A seat that brings no sign-in still gets the entry's, or this would
	// just be an entry whose key never travels.
	run := runOn(t, c, seat,
		e.runEnvironment(t.Context(), nil, seat, &org.RoleSandbox{Env: map[string]string{"GITHUB_TOKEN": "ghp_x"}}, nil, nil).env)
	if run.env["ANTHROPIC_API_KEY"] != "sk-ant-company" || run.env["GITHUB_TOKEN"] != "ghp_x" {
		t.Errorf("a seat with no sign-in of its own: run environment %v", run.env)
	}
}

// AN ENTRY'S RUN NAMES THE MODEL AS ITS TEXT CALLS DO, FOR ITS OWN HARNESS.
//
// The run used to be handed a FAMILY instead — the profile's vendor — from
// which OpenCode rebuilt `anthropic/<model>`. So an OpenCode executor ran its
// box on a model its text calls never named, and no value of `model`
// worked for both. The model is resolved (a `${VAR}` reaches the harness as its
// value) and passed as written.
func TestAnEntrysRunNamesItsModelAsItsTextCallsDo(t *testing.T) {
	state := t.TempDir()
	t.Setenv(config.HarnessHomeEnv, state)
	t.Setenv("OC_MODEL", "openrouter/anthropic/claude-sonnet-5")
	c := companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    oc:
      harness: opencode
      model: ${OC_MODEL}
roles:
  - name: Engineer
    handle: eng
    llm: oc
`)
	seat := seatNamed(t, c, "Engineer")
	run := runOn(t, c, seat, nil)
	member, err := c.Models.Head(seat, phase.Execute)
	if err != nil {
		t.Fatal(err)
	}
	text := member.Provider.(*harness.Provider).ModelArgument()
	if text != "openrouter/anthropic/claude-sonnet-5" {
		t.Fatalf("the text calls' model = %q, want the resolved model as written", text)
	}
	if run.model != text {
		t.Errorf("the run's model = %q, want %q, the value every text call passes", run.model, text)
	}
	// The run is a script quoted for `sh -c`; its quoting undone, the model
	// is an argument of its own.
	req := sandbox.RunRequest{Brief: "x", Model: run.model}
	cmd := executor.OpenCode{}.Command(req, executor.OpenCode{}.Session(req), executor.Paths{}, "")
	if script := strings.ReplaceAll(cmd, `'\''`, "'"); !strings.Contains(script, "--model '"+text+"'") {
		t.Errorf("the agent run gave:\n%s\nwant --model %q, the value every text call passes", cmd, text)
	}
}

// A LOGIN OPENCODE KEEPS IN ITS OWN DATABASE FOLLOWS A RUN INTO A LOCAL BOX: the
// entry's exported login, mapped where the runner imports it from — mapped
// whether or not anybody has signed the entry in yet, because the box reads
// the login as it stands when it is seeded and is given only what it holds
// then, and with a store's rule, without which its refresh is never written
// back. A map of what was there when the run was resolved named a login the
// shared directory may have moved past by the time the box was seeded.
//
// Mutation: hand the document without its rule.
func TestAnOpenCodeLoginTravelsAsItsExportedDocument(t *testing.T) {
	t.Setenv(config.HarnessHomeEnv, t.TempDir())
	c := companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    oc:
      harness: opencode
      model: anthropic/claude-sonnet-5
roles:
  - name: Engineer
    handle: eng
    llm: oc
`)
	seat := seatNamed(t, c, "Engineer")
	member, err := c.Models.Head(seat, phase.Execute)
	if err != nil {
		t.Fatal(err)
	}
	shared := member.Provider.(*harness.Provider).Workspace().CredentialsDir()
	doc := filepath.Join(shared, harnessprofile.CredentialStoreDocument)
	for _, signedIn := range []bool{false, true} {
		if signedIn {
			if err := os.MkdirAll(shared, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(doc, []byte(`[{"id":"cred_1"}]`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		run := runOn(t, c, seat, nil)
		files := run.files()
		if got := files[executor.CredentialDocument]; got != doc || len(files) != 1 {
			t.Errorf("signed in %v: the box is seeded from %q at %s, want the shared login %q "+
				"and nothing else (files %v)", signedIn, got, executor.CredentialDocument, doc, files)
		}
		if rule, ruled := run.fileRules()[executor.CredentialDocument]; !ruled ||
			rule.Name != harnessprofile.CredentialStoreDocument {
			t.Errorf("signed in %v: the document is handed no store's rule (%+v, %v), so its refresh "+
				"would never be written back", signedIn, rule, ruled)
		}
	}
}

// The login travels as a host-path MAP for the local backend to seed, each
// file with its rule, and as a token in the run environment. The files are
// offered rather than exported because they carry a refresh token whose
// rotation is shared fleet state.
//
// Mutation: hand a login file without its rule, and its refresh stays in the box.
func TestASubscriptionSeatCarriesItsLoginIntoTheBox(t *testing.T) {
	state := t.TempDir()
	t.Setenv(config.HarnessHomeEnv, state)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-travelling")

	credentials := filepath.Join(state, "subscription", "claude-code", "credentials")
	if err := os.MkdirAll(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentials, ".credentials.json"),
		[]byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c := companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    subscription:
      harness: claude-code
      model: sonnet
roles:
  - name: Engineer
    handle: eng
    llm: subscription
`)
	run := runOn(t, c, seatNamed(t, c, "Engineer"), nil)
	files, env := run.files(), run.env
	host, mapped := files[".claude/.credentials.json"]
	if !mapped {
		t.Fatalf("the login was not offered to the box: %v", files)
	}
	if host != filepath.Join(credentials, ".credentials.json") {
		t.Errorf("mapped to %q, want the shared login on the engine host", host)
	}
	// EVERY LOGIN FILE IS HANDED ITS RULE: a file with none is never written
	// back, so a refreshed login would stay in the box.
	rules := run.fileRules()
	for rel, source := range files {
		if rules[rel].Name != filepath.Base(source) || len(rules[rel].RefreshKeys) == 0 {
			t.Errorf("%s is handed the rule %+v, want its own, naming the token fields it carries back", rel, rules[rel])
		}
	}
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-travelling" {
		t.Errorf("the headless token did not reach the run env: %v", env)
	}
}

// A BOX IS OFFERED EVERY FILE OF THE LOGIN, signed in or not: the map names
// where the login is kept, and the box is seeded with whatever the login holds
// when it is seeded — nothing at all for an entry nobody signed in, so the harness
// inside says its own plain "not authenticated" rather than failing on a file
// that is not there. A map of only the files present when the run was
// resolved named a login the shared directory may have moved past by the
// time the box was seeded, and a box given half of a newer sign-in.
func TestEveryLoginFileIsOfferedWhetherOrNotItIsHeld(t *testing.T) {
	t.Setenv(config.HarnessHomeEnv, t.TempDir())
	c := companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    subscription:
      harness: claude-code
      model: sonnet
roles:
  - name: Engineer
    handle: eng
    llm: subscription
`)
	seat := seatNamed(t, c, "Engineer")
	files := runOn(t, c, seat, nil).files()
	member, err := c.Models.Head(seat, phase.Execute)
	if err != nil {
		t.Fatal(err)
	}
	shared := member.Provider.(*harness.Provider).Workspace().CredentialsDir()
	if want := filepath.Join(shared, ".credentials.json"); files[".claude/.credentials.json"] != want || len(files) != 1 {
		t.Errorf("an entry nobody signed in offered %v, want its one login file at %s", files, want)
	}
}

// The direction of the merge is a decision, not a detail: an operator who
// named a variable in role.sandbox.env meant that value — including the
// deliberate choice to point one seat's executor at a different account.
func TestTheOperatorsSandboxEnvWinsOverTheResolvedCredential(t *testing.T) {
	t.Parallel()
	got := underlay(
		map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "the operator's own"},
		map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "resolved", "OTHER": "added"},
	)
	if got["CLAUDE_CODE_OAUTH_TOKEN"] != "the operator's own" {
		t.Errorf("the engine overrode a declared variable: %q", got["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	if got["OTHER"] != "added" {
		t.Errorf("an undeclared default was not added: %v", got)
	}
	if underlay(nil, map[string]string{"A": "1"})["A"] != "1" {
		t.Error("underlay dropped defaults onto a nil environment")
	}
}

// EVERY PLACEMENT THE CONFIG ACCEPTS IS ONE THIS ENGINE CAN BUILD.
//
// `config.Placements` is the closed set a `run_in:` is checked against, and
// `buildSandboxProvider` turns one into a running backend. Nothing connects
// them, so a placement the config accepts (its default included) could name a
// backend nobody wrote: the config says yes and the runtime says no, only for
// a company that actually runs code, so a boot proves nothing. Hence a test
// that walks the set.
//
// NOT PARALLEL: it puts a stub container runtime first on PATH, and t.Setenv
// refuses a parallel test.
func TestEveryConfiguredPlacementCanBeBuilt(t *testing.T) {
	// A STUB `docker`, FIRST ON PATH. Building the `container` cell looks
	// its runtime up there ([sandbox.ResolveContainerRuntime]) — on
	// purpose, so a host that cannot run the cell is told when the config
	// is applied rather than at the first executor. Left to the host's
	// PATH, this case failed on every machine with neither docker nor
	// podman (a contributor's workstation, a runner with no container CLI)
	// and passed wherever one was installed, so its verdict was about the
	// machine; a skip there would leave the property unchecked on exactly
	// those machines. The stub is only found, never run: building a
	// provider starts no container.
	stubVendorHarness(t, "docker")
	spec := &config.SandboxProvider{
		// An image, because one of the two cells needs it and a backend
		// that cannot be built for `container` is exactly what this test
		// exists to catch. That cell needs a runtime on PATH as well, which
		// the stub above stands in for, so a failure here means the engine
		// cannot build the cell, never that this machine has no docker.
		Local: &config.LocalSandbox{Image: "example.invalid/box:1"},
	}
	if !spec.Enabled() {
		t.Fatal("a catalogue with local: configures no box")
	}
	for _, placement := range config.Placements {
		// NO RESOLVER: this asks whether each backend can be CONSTRUCTED,
		// and the catalogue holds no reference to resolve.
		provider, err := buildSandboxProvider(spec, nil, placement)
		if err != nil {
			t.Errorf("run_in %q is accepted by the config and cannot be "+
				"built: %v", placement, err)
			continue
		}
		if provider == nil {
			t.Errorf("run_in %q built no provider and no error, so an "+
				"executor placed there plans around a box it never gets", placement)
		}
	}
}

// THE RUNTIME'S DECLARED ENVIRONMENT REACHES IT RESOLVED. Tier B stores a
// `${VAR}` verbatim, so a runtime_env passed through as written would put the
// literal "${REGISTRY_ROLE}" into every docker call's environment: a credential
// helper assuming a role called "${…}" and an agent socket at that path. It
// resolves the way an MCP server's `env:` does — the store first, the
// process environment behind it — and a literal stays a literal.
func TestTheRuntimesDeclaredEnvironmentIsResolvedAtTheEdge(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/run/user/1000/ssh-agent.sock")
	env := config.WithStore(config.MapSource{"REGISTRY_ROLE": "arn:aws:iam::123456789012:role/pull"})
	got := localOptions(&config.SandboxProvider{Local: &config.LocalSandbox{
		Image: "example.invalid/box:1",
		RuntimeEnv: map[string]string{
			"SSH_AUTH_SOCK": "${SSH_AUTH_SOCK}",
			"AWS_ROLE_ARN":  "${REGISTRY_ROLE}",
			"AWS_PROFILE":   "registry-pull",
		},
	}}, env, config.PlacementContainer).RuntimeEnv
	want := map[string]string{
		"SSH_AUTH_SOCK": "/run/user/1000/ssh-agent.sock",
		"AWS_ROLE_ARN":  "arn:aws:iam::123456789012:role/pull",
		"AWS_PROFILE":   "registry-pull",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("RuntimeEnv = %v, want %v", got, want)
	}
}

// THE ORPHAN REAPER'S CUTOFF IS THE MANAGER'S HOST GRACE: one value judges how
// long a launch node may be away and which stamped box on its root is an
// orphan, so a box the grace still keeps is never reaped under it. Unset, both
// take sandbox.DefaultHostGrace, applied in that package alone.
//
// Mutation: leave HostGrace off the local options, or default it in config or
// here, and a check below goes red.
func TestTheReapersCutoffIsTheManagersHostGrace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line    string
		manager time.Duration
		local   time.Duration
	}{
		{line: "host_grace_seconds: 600", manager: 10 * time.Minute, local: 10 * time.Minute},
		{manager: sandbox.DefaultHostGrace},
	} {
		c, err := config.ParseCompany([]byte(fmt.Sprintf(`
name: Acme
providers:
  sandbox:
    local: {state_dir: %q}
    default_run_in: direct
    %s
  llm:
    main: {model: sonnet}
roles:
  - name: SWE
    llm: main
`, t.TempDir(), tc.line)))
		if err != nil {
			t.Fatalf("the fixture does not parse: %v", err)
		}
		spec := c.Providers.Sandbox
		if got := localOptions(spec, nil, config.PlacementDirect).HostGrace; got != tc.local {
			t.Errorf("%q hands the reaper %v, want %v", tc.line, got, tc.local)
		}
		manager, err := buildSandbox(c, nil, nil, nil)
		if err != nil || manager == nil {
			t.Fatalf("buildSandbox = %v, %v", manager, err)
		}
		if got := manager.HostGrace(); got != tc.manager {
			t.Errorf("%q gives the manager %v, want %v", tc.line, got, tc.manager)
		}
	}
}

// A CATALOGUE WITH NOTHING IN IT IS NOT A SANDBOX, and the distinction is the
// one an operator's half-finished edit lands on: `providers.sandbox: {}`
// parses, and if it reported itself enabled the engine would build a manager
// with no backends and launch every seat's executor into nothing.
func TestAnEmptyCatalogueIsNotEnabled(t *testing.T) {
	t.Parallel()
	if (&config.SandboxProvider{}).Enabled() {
		t.Error("an empty catalogue reports itself enabled")
	}
	if (&config.SandboxProvider{Fake: true}).Enabled() != true {
		t.Error("the double does not report itself enabled")
	}
}

// THE DOUBLE ANSWERS EVERY CELL, so a demonstration config differs from a
// real one in exactly one line rather than in the placement every seat names.
func TestTheDoubleAnswersEveryPlacement(t *testing.T) {
	t.Parallel()
	spec := &config.SandboxProvider{Fake: true}
	for _, placement := range config.Placements {
		provider, err := buildSandboxProvider(spec, nil, placement)
		if err != nil {
			t.Fatalf("the double cannot serve %q: %v", placement, err)
		}
		if _, double := provider.(*sandbox.FakeProvider); !double {
			t.Errorf("placement %q built %T, not the double", placement, provider)
		}
	}
}

// A suspension with nowhere to go leaves a job running in a box nobody is
// coming back for. Whichever way it happens (the runner never recorded the
// conversation, it would not serialize, the record could not be written, or the
// run was no longer launching), the run is settled while the seat's owner is
// still this process: its box reclaimed and its record deleted.
//
// Marking the record failed is what this used to do, and it stranded the box:
// a record that is not active is read by no recovery pass and polled by no
// waiter, so the job ran on in its box, holding its node's disk and memory,
// with nothing left to reclaim it.
func TestAnUnrecordableSuspensionReclaimsTheRunsBox(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(memory.NewFleet())
	provider := sandbox.NewFakeProvider()
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
		Runners:   map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	queue := &publishRecorder{}
	// The engine's own resumer, as buildSandboxRuntime hands it over: the
	// coordinator refuses to be built without one.
	e := &Engine{}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Pushes:   noPushes{},
		Answers:  testAnswers(),
		Audience: noAudience{},
		Queue:    queue, Pending: store, Manager: manager, Resume: &resumer{engine: e},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	e.useSandbox(store, coordinator)
	box, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", Role: "SWE",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := store.AttachSandbox(t.Context(), "t1",
		sandbox.BoxRef{SandboxID: box.ID(), CommandID: "1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}

	e.failSuspension(t.Context(), e.sandbox.Load(), "t1", "sandbox_suspension_missing",
		"the turn suspended but recorded no conversation", nil)

	if got, found, err := store.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("Get = %+v, found %v, %v; want the run ended and its record gone", got, found, err)
	}
	if killed := provider.KilledIDs(); len(killed) != 1 || killed[0] != box.ID() {
		t.Fatalf("killed %v, want the running job's box %q reclaimed", killed, box.ID())
	}
	if !queue.published(types.ExecutorFailed{}.EventType()) {
		t.Fatal("the lost run was not announced")
	}
}

// publishRecorder is the slice of the queue a coordinator publishes through.
type publishRecorder struct {
	mu     sync.Mutex
	events []*events.Event
}

func (r *publishRecorder) Publish(_ context.Context, _ string, ev *events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *publishRecorder) published(eventType string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.Type == eventType {
			return true
		}
	}
	return false
}

// The waiter duty must survive three of its OWN ticks, whatever the seat lease
// TTL is.
//
// It was `3 * sandbox.DefaultPollInterval`, a constant that ignored the
// configured interval, and then it was capped at the seat lease TTL because
// duties shared the seat lease bucket. That cap handed a 60 s poll a 45 s duty,
// which lapsed between two of its own ticks. Duties have their own bucket now,
// so the only ceiling is coord.MaxDutyTTL, and an interval that would need more
// is refused when the waiter starts rather than on every tick.
func TestTheWaiterDutyTTLFollowsItsCadence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"the default cadence", sandbox.DefaultPollInterval, 45 * time.Second},
		{"a cadence slower than the seat lease TTL still gets three ticks", 60 * time.Second, 3 * time.Minute},
		{"a fast cadence takes the floor", 100 * time.Millisecond, 30 * time.Second},
		{"an unset cadence takes the default", 0, 45 * time.Second},
		{"the slowest cadence the duty ceiling admits", coord.MaxDutyTTL / dutyTTLTicks, coord.MaxDutyTTL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := waiterDutyTTL(tc.interval); got != tc.want {
				t.Fatalf("waiterDutyTTL(%s) = %s, want %s", tc.interval, got, tc.want)
			}
			if got := waiterDutyTTL(tc.interval); got < dutyTTLTicks*tc.interval {
				t.Fatalf("waiterDutyTTL(%s) = %s lapses inside three of its own ticks", tc.interval, got)
			}
			if _, err := (&Engine{}).waiterDuty(tc.interval); err != nil {
				t.Fatalf("waiterDuty(%s) refused an interval the duty ceiling admits: %v", tc.interval, err)
			}
		})
	}
}

// A poll interval whose duty no backend grants is refused at start, naming the
// option, rather than failing every claim for the life of the process.
func TestAWaiterIntervalBeyondTheDutyCeilingIsRefusedAtStart(t *testing.T) {
	t.Parallel()
	interval := coord.MaxDutyTTL/dutyTTLTicks + time.Second
	duty, err := (&Engine{}).waiterDuty(interval)
	if !errors.Is(err, coord.ErrTTLTooLong) {
		t.Fatalf("waiterDuty(%s) = (%v, %v), want an error wrapping coord.ErrTTLTooLong", interval, duty != nil, err)
	}
	if !strings.Contains(err.Error(), "SandboxPollInterval") {
		t.Fatalf("the refusal %q does not name the option to change", err)
	}
}

// chargeRig is a coordinator whose accountant is the engine's own, over the
// fleet's real counters, so every charge is asserted where a budget reads it.
type chargeRig struct {
	fleet       *memory.Fleet
	coordinator *sandbox.Coordinator
	completion  types.ExecutorFinished
}

// chargeAgent is the seat the rig's run belongs to, and the id its counter is
// keyed on.
const chargeAgent = "11111111-1111-1111-1111-111111111111"

// discardQueue takes the coordinator's announcements and keeps none of them.
type discardQueue struct{}

func (discardQueue) Publish(context.Context, string, *events.Event) error { return nil }

// movedSeat fails the first `fails` resumes the way a node that lost the seat
// does, then resumes.
type movedSeat struct {
	mu    sync.Mutex
	fails int
}

func (m *movedSeat) Resume(context.Context, sandbox.ResumeRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fails > 0 {
		m.fails--
		return fmt.Errorf("%w: the seat moved between the publish and the receive",
			sandbox.ErrResumeUnavailable)
	}
	return nil
}

// newChargeRig seeds one running launch whose job has finished having
// spent `tokens`, charged against the given caps.
func newChargeRig(t *testing.T, tokens, orgCap, seatCap int, resume sandbox.Resumer) *chargeRig {
	t.Helper()
	ctx := t.Context()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	provider, runner := sandbox.NewFakeProvider(), sandbox.NewFakeRunner("claude-code")
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
		Runners:   map[string]sandbox.Runner{"claude-code": runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Pushes:   noPushes{},
		Answers:  testAnswers(),
		Audience: noAudience{},
		Queue:    discardQueue{}, Pending: store, Manager: manager, Resume: resume,
		Account: accountant(fleet, dayCap(orgCap), dayCap(seatCap)),
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	box, err := provider.Create(ctx, sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", AgentID: chargeAgent, Role: "SWE",
		Harness: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := store.AttachSandbox(ctx, "t1", sandbox.BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", Harness: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	if suspended, err := store.MarkSuspended(ctx, "t1", sandbox.Suspension{State: json.RawMessage(
		`{"version":4,"iteration":1}`)}); err != nil || !suspended {
		t.Fatalf("MarkSuspended = %v, %v", suspended, err)
	}
	runner.Finish(sandbox.Result{Success: true, Text: "done", InputTokens: tokens})
	run, found, err := store.Get(ctx, "t1")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}

	return &chargeRig{
		fleet: fleet, coordinator: coordinator,
		completion: types.ExecutorFinished{
			TurnID: "t1", LaunchID: run.LaunchID, AgentHandle: "swe", Agent: chargeAgent,
			RoleName: "SWE", SandboxID: box.ID(), Harness: "claude-code",
		},
	}
}

func (r *chargeRig) deliver(t *testing.T) error {
	t.Helper()
	return r.coordinator.OnCompleted(t.Context(), r.completion, events.New(r.completion, events.TraceContext{}))
}

func (r *chargeRig) used(t *testing.T, scope string) int {
	t.Helper()
	return dayUsed(t.Context(), t, r.fleet, scope)
}

// dayCap is a day ceiling of n, or no ceiling at all for 0 — the rig's shorthand.
func dayCap(n int) coord.Caps {
	if n == 0 {
		return nil
	}
	return coord.Caps{period.Day: n}
}

// A LAUNCH IS CHARGED TO THE FLEET ONCE, however often its completion
// comes back. A node that lost the seat answers ErrResumeUnavailable, the
// claim reverts and the completion is delivered again, and each of those
// passes collected the same finished job and charged it to both counters: the
// seat's allowance and the company's shrank by one run per retry.
func TestARetriedExecutorIsChargedToTheFleetOnce(t *testing.T) {
	t.Parallel()
	rig := newChargeRig(t, 1000, 0, 0, &movedSeat{fails: 2})
	for attempt := range 2 {
		if err := rig.deliver(t); !errors.Is(err, sandbox.ErrResumeUnavailable) {
			t.Fatalf("attempt %d = %v, want the completion sent back", attempt+1, err)
		}
	}
	if err := rig.deliver(t); err != nil {
		t.Fatalf("the retry that resumes: %v", err)
	}

	if got := rig.used(t, coord.OrgScope); got != 1000 {
		t.Errorf("the company was charged %d for one run of 1000 delivered three times", got)
	}
	if got := rig.used(t, coord.AgentScope(chargeAgent)); got != 1000 {
		t.Errorf("the seat was charged %d for one run of 1000 delivered three times", got)
	}
}

// A RUN THAT OVERRUNS A CAP IS STILL RECORDED ON BOTH COUNTERS, and the turn
// goes on: the tokens are spent either way, and a cap cannot un-spend them.
//
// The charge is a POST-charge, not the gate a round passes. Put through the
// gate it was recorded not at all whenever it did not fit — which is exactly
// when a cap binds — so the counter under-stated the company by the whole run
// at that moment, and the seat's next round was admitted against room the run
// had already used. Recorded, the next round is refused against the figure
// that includes it.
func TestAnOverCapExecutorIsRecordedOnBothCounters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		orgCap, seatCap int
	}{
		{"the company's cap", 500, 0},
		{"the seat's cap", 0, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newChargeRig(t, 1000, tc.orgCap, tc.seatCap, &movedSeat{})
			if err := rig.deliver(t); err != nil {
				t.Fatalf("an over-cap charge stopped the turn: %v", err)
			}
			org, seat := rig.used(t, coord.OrgScope), rig.used(t, coord.AgentScope(chargeAgent))
			if org != 1000 || seat != 1000 {
				t.Errorf("counters at org=%d seat=%d after a run of 1000 past a cap "+
					"of 500; the spend happened and both counters have to hold it",
					org, seat)
			}
		})
	}
}

// AND ONCE, however often its completion comes back.
//
// The two halves meet here: the charge is recorded past the cap, and the
// coordinator reads the answer as "recorded" rather than as "refused, offer it
// again". Read the other way, the one company a cap is binding on is charged
// for its over-cap run once per completion retry.
func TestAnOverCapExecutorIsChargedToTheFleetOnce(t *testing.T) {
	t.Parallel()
	rig := newChargeRig(t, 1000, 500, 0, &movedSeat{fails: 2})
	for attempt := range 2 {
		if err := rig.deliver(t); !errors.Is(err, sandbox.ErrResumeUnavailable) {
			t.Fatalf("attempt %d = %v, want the completion sent back", attempt+1, err)
		}
	}
	if err := rig.deliver(t); err != nil {
		t.Fatalf("the retry that resumes: %v", err)
	}

	if got := rig.used(t, coord.OrgScope); got != 1000 {
		t.Errorf("the company was charged %d for one over-cap run of 1000 "+
			"delivered three times", got)
	}
	if got := rig.used(t, coord.AgentScope(chargeAgent)); got != 1000 {
		t.Errorf("the seat was charged %d for one over-cap run of 1000 "+
			"delivered three times", got)
	}
}

// A NODE WITH NO COORDINATOR RETIRES BESIDE A RUN RECORD NOTHING CAN READ. The
// listing it refuses on skips the record rather than failing, so a seat with
// no readable run is retired and one with a readable run is still refused, and
// the unreadable record is left exactly as it was for the documented repair:
// nothing can deliver to it, and nothing deletes or rewrites what it could not
// read.
func TestABareRetirementSkipsARecordNothingCanRead(t *testing.T) {
	t.Parallel()
	swe, pm := uuid.New(), uuid.New()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	if _, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", AgentID: swe.String(), Role: "SWE",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	garbled := []byte("{not json")
	if _, err := fleet.CreateSandboxRun(t.Context(), "t-garbled", garbled, nil); err != nil {
		t.Fatalf("CreateSandboxRun: %v", err)
	}
	bare := &Engine{backends: &Backends{Fleet: fleet}}
	// Refused over the READABLE run, and over it alone: a listing that failed
	// on the unreadable record would refuse too, for the wrong reason.
	if _, err := bare.retireSeatRuns(t.Context(), swe, "swe", "retirement:1", 3); err == nil ||
		!strings.Contains(err.Error(), "still has 1 executor records") {
		t.Fatalf("the retirement of a seat with one readable run = %v, want it refused over "+
			"that run", err)
	}
	if _, err := bare.retireSeatRuns(t.Context(), pm, "pm", "retirement:1", 3); err != nil {
		t.Fatalf("a seat with no readable run was refused beside an unreadable record: %v", err)
	}
	if record, found, err := fleet.SandboxRun(t.Context(), "t-garbled"); err != nil || !found ||
		string(record.Value) != string(garbled) {
		t.Fatalf("the unreadable record is %q (found %v, err %v), want it left as written",
			record.Value, found, err)
	}
}

// A RETIRED SEAT'S RUNS ARE ENDED BY THE NODE THAT RETIRES IT. Through the
// coordinator where this node has one, which reclaims each box; and where it
// has none, the retirement is refused while the fleet still records a run for
// the seat, because a node that cannot reach a box must not delete the
// subscriptions that run's completion and answer travel on.
//
// BY THE SEAT'S ID, on both paths: the retirement of a mailbox the broker's
// listing alone found holds no handle, and selected by the empty one it held
// it matched no run, refused nothing and ended nothing.
func TestRetiringASeatEndsItsRunsOrRefusesWithoutACoordinator(t *testing.T) {
	t.Parallel()
	swe, pm := uuid.New(), uuid.New()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	if _, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", AgentID: swe.String(), Role: "SWE",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}

	bare := &Engine{backends: &Backends{Fleet: fleet}}
	if _, err := bare.retireSeatRuns(t.Context(), swe, "swe", "retirement:1", 3); err == nil {
		t.Fatal("a node with no coordinator let the retirement proceed over a recorded run")
	}
	if _, err := bare.retireSeatRuns(t.Context(), swe, "", "retirement:1", 3); err == nil {
		t.Fatal("a node with no coordinator let the retirement of a mailbox with no handle " +
			"proceed over its seat's recorded run")
	}
	if _, err := bare.retireSeatRuns(t.Context(), pm, "pm", "retirement:1", 3); err != nil {
		t.Fatalf("a seat with no runs was refused: %v", err)
	}

	provider := sandbox.NewFakeProvider()
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
		Runners:   map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	equipped := &Engine{backends: &Backends{Fleet: fleet}}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Pushes:   noPushes{},
		Answers:  testAnswers(),
		Audience: noAudience{},
		Queue:    &publishRecorder{}, Pending: store, Manager: manager,
		Resume: &resumer{engine: equipped},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	equipped.useSandbox(store, coordinator)
	if _, err := equipped.retireSeatRuns(t.Context(), swe, "", "retirement:1", 3); err != nil {
		t.Fatalf("retireSeatRuns: %v", err)
	}
	if _, found, err := store.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("the retired seat's run survived (found %v, %v)", found, err)
	}
}

// THE MAPPING THE RESUMED LOOP DEPENDS ON, and three of its five fields come
// off the parked row rather than off anything this process can see — so a
// field dropped from the literal this replaced had no symptom here at all. The
// round is the one that did have one: the loop counted from one again, so a
// turn that suspended at round three of three came back with three more.
func TestAResumedRunCarriesEveryFieldTheLoopGatesOn(t *testing.T) {
	t.Parallel()
	prior := []ledger.Iteration{{Iteration: 1, Intent: "before the box"}}
	in := resumeInput{
		Run: sandbox.PendingRun{
			TurnID: "run-1", DelegationDepth: 3, AgentHandle: "swe",
		},
		State: execstate.State{Round: 4, Iterations: prior},
	}
	got := resumeInputFor(in, turn.ToolReply("slack"))

	// THE SAME TURN CONTINUES. A resume that minted its own id would file
	// the second half of one turn as a second execution of it.
	if got.RunID != "run-1" {
		t.Errorf("RunID = %q, want the parked run's", got.RunID)
	}
	if !got.Resume {
		t.Error("the loop would start a fresh executor rather than re-entering one")
	}
	// The budget, not a fresh one.
	if got.Round != 4 {
		t.Errorf("Round = %d, want the round the executor parked in", got.Round)
	}
	if got.Depth != 3 {
		t.Errorf("Depth = %d, want 3 — the cap would be checked against zero", got.Depth)
	}
	if got.Reply != turn.ToolReply("slack") {
		t.Errorf("Reply = %q — the resumed turn would run with no delivery gate",
			got.Reply)
	}
	// The rounds that closed before the box, so their deliveries are not
	// fired a second time.
	if len(got.History) != 1 || got.History[0].Intent != "before the box" {
		t.Errorf("History = %+v, want the ledger the suspend handed out", got.History)
	}
}

// A PARK PAST ITS SHARE OF THE RUN'S ROW IS REFUSED BY NAME, before the store is
// asked. What gives way in a parked state is its closed rounds
// (execstate.EncodeWithin), down to one count of calls per tool for the lot,
// and what is left past [sandbox.ExecuteStateBytes] is the turn's own context —
// here an ask of three MiB, a trigger's own words — which no resume can do
// without and no row can carry. So the run is ended as
// a suspension nothing recorded, and the park answers that nothing can resume
// it, with no error: nothing was sent that may have landed.
//
// Mutation: encode the state whole (execstate.Encode), and the store's own
// refusal reads as a write that may have landed; skip both refusals, and three
// MiB of state land on a row whose share of it is two.
func TestAParkPastItsShareOfTheRowIsRefusedByName(t *testing.T) {
	t.Parallel()
	e, _ := starting(t, refusingModels(t))
	rigRuns(t, e, nil)
	company := e.Company()
	seat := company.Org.AgentSeatByHandle("swe")
	r, err := company.RunnerFor(t.Context(), "swe", nil, RunnerInput{
		Reply: turn.NoReply(), Launcher: launcherStub{}, Sessions: e.sessions,
		Turn: runner.Turn{RunID: "run-code", Context: &turnctx.Turn{RunID: "run-code", Seat: seat,
			Org: company.Org}},
	})
	if err != nil {
		t.Fatalf("RunnerFor: %v", err)
	}
	res, err := turn.Run(t.Context(), r, company.TurnSettings(time.Time{}),
		turn.Input{RunID: "run-code", Reply: turn.NoReply()})
	if err != nil || !res.Suspended {
		t.Fatalf("turn.Run = %+v, %v; the turn did not park", res, err)
	}
	pending := e.sandbox.Load().pending
	// THE ROW THE LAUNCH WOULD HAVE OPENED, which the stub opened none of.
	if _, err := pending.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "run-code", AgentHandle: "swe", Role: "SWE",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	resumable, err := e.persistSuspension(t.Context(), r, "run-code", turnTelemetry{
		written: &turnctx.Written{}, ask: strings.Repeat("a", 3<<20),
	}, segmentCharge{})
	if err != nil || resumable {
		t.Fatalf("persistSuspension = %v, %v; want the park refused by name, nothing sent", resumable, err)
	}
	row, found, err := pending.Get(t.Context(), "run-code")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found && (row.Status == sandbox.StatusRunning || len(row.ExecuteState) > 0) {
		t.Errorf("the refused park left the run %s with %d bytes of state", row.Status, len(row.ExecuteState))
	}
}

// useSandbox installs a runtime a case assembled itself, as [Engine.startSandbox]
// publishes the one it builds.
func (e *Engine) useSandbox(pending sandbox.PendingStore, coordinator *sandbox.Coordinator) {
	if e.sandboxSeats == nil {
		e.sandboxSeats = map[string]bool{}
	}
	e.sandbox.Store(&sandboxRuntime{pending: pending, coordinator: coordinator})
}

// THE COORDINATOR COUNTS A FAILED RESUME TOWARD ITS GIVE-UP BY THE ANSWER THE
// SEAT'S DELIVERIES ARE NAKED BY (nodesFailure): a resume the coordination
// store, the queue, a stop or this node's sandbox configuration failed is the
// node's, and ends no run, while one the run itself failed, a deadline among
// them, is counted. Unwired, every failure is the run's, and half an hour of an
// outage ends every run whose job finished during it.
//
// Mutation: leave NodesFailure off the coordinator's options, or hand it any
// answer but nodesFailure's, and a row goes red.
func TestTheEnginesCoordinatorCountsOnlyTheRunsOwnFailures(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := &Engine{backends: &Backends{Queue: queuememory.New()}, answers: testAnswers()}
	opts := e.coordinatorOptions(nil, nil)
	if opts.NodesFailure == nil {
		t.Fatal("the coordinator is built with no answer to whose a failed resume is")
	}
	for name, tc := range map[string]struct {
		err   error
		nodes bool
	}{
		"a coordination store that did not answer": {err: fmt.Errorf("engine: read the rebase: %w: "+
			"nats: timeout", coord.ErrUnavailable), nodes: true},
		"a step the coordination store was silent on": {err: fmt.Errorf("%w: sandbox: read run t1",
			sandbox.ErrStoreSilent), nodes: true},
		"a relaunch this node's configuration cannot start": {err: fmt.Errorf("sandbox: resuming t1: %w",
			sandbox.Because(sandbox.WhyConfiguration, errors.New("docker: the runtime refused the image"))),
			nodes: true},
		"a resume that ran past its deadline": {err: fmt.Errorf("runner: %w", context.DeadlineExceeded)},
		"the run's own":                       {err: errors.New("the turn could not be re-entered")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := opts.NodesFailure(ctx, tc.err); got != tc.nodes || got != nodesFailure(ctx, tc.err) {
				t.Errorf("the coordinator reads %v as the node's = %v, want %v, as nodesFailure does",
					tc.err, got, tc.nodes)
			}
		})
	}
}

// THE ENGINE'S COORDINATOR RECORDS ITS SPOOL ROOM INTO THE NODE'S RECORDER, the
// one every other subsystem records into: each final take's wait under its
// outcome, in milliseconds, and the room held. Unwired, a final take refused
// for room is a log line alone, and no panel shows the refusals rising.
//
// Mutation: leave Spools off the coordinator's options, or record a wait
// without its outcome or the room held under another instrument.
func TestTheEnginesCoordinatorRecordsItsSpoolRoom(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	e := &Engine{backends: &Backends{Queue: queuememory.New()}, answers: testAnswers(), metrics: rec}
	meter := e.coordinatorOptions(nil, nil).Spools
	if meter == nil {
		t.Fatal("the coordinator is built with nothing to tell its spool room's measures")
	}
	for _, outcome := range sandbox.SpoolOutcomes {
		meter.SpoolWaited(outcome, time.Minute)
	}
	meter.SpoolHeld(1 << 20)
	seen := map[string]bool{}
	for _, s := range rec.Read() {
		switch {
		case s.Name == metrics.ExecutorSpoolWait:
			if s.Count != 1 || s.Sum != 60_000 {
				t.Errorf("%s%v holds %d waits of %v ms, want one of 60000", s.Name, s.Attrs, s.Count, s.Sum)
			}
			seen[s.Attrs["outcome"]] = true
		case s.Name == metrics.ExecutorSpoolHeldBytes && s.Value != 1<<20:
			t.Errorf("%s reads %v, want 1 MiB", s.Name, s.Value)
		case s.Name == metrics.ExecutorSpoolHeldBytes:
			seen["held"] = true
		}
	}
	for _, outcome := range sandbox.SpoolOutcomes {
		if !outcome.Valid() || !seen[string(outcome)] {
			t.Errorf("no %s series for outcome %q", metrics.ExecutorSpoolWait, outcome)
		}
	}
	if !seen["held"] {
		t.Errorf("no %s series", metrics.ExecutorSpoolHeldBytes)
	}
}

// THE ENGINE'S COORDINATOR IS TOLD WHICH RESUME FAILURES ONLY AN OPERATOR
// CLEARS, by the one classification of conditions every failure is told by: a
// disk full, read-only or failing and a process at its limit of open files are
// raised as executor_resume_blocked while the run waits, under the kind its
// failure records; a coordination store that did not answer, which clears by
// itself, and the run's own failure are not. Unwired, a run the give-up never
// ends waited for the operator with nothing raised but a line per attempt.
//
// Mutation: leave OperatorsCondition off the coordinator's options, or answer
// every condition of the node's as the operator's, and a row goes red.
func TestTheEnginesCoordinatorRaisesAResumeOnlyAnOperatorClears(t *testing.T) {
	t.Parallel()
	e := &Engine{backends: &Backends{Queue: queuememory.New()}, answers: testAnswers()}
	opts := e.coordinatorOptions(nil, nil)
	if opts.OperatorsCondition == nil {
		t.Fatal("the coordinator is built with no answer to which failures only an operator clears")
	}
	for name, tc := range map[string]struct {
		err  error
		kind string
	}{
		"a full disk":             {err: fmt.Errorf("resume: write wal: %w", syscall.ENOSPC), kind: "disk_full"},
		"a read-only disk":        {err: fmt.Errorf("resume: open: %w", syscall.EROFS), kind: "disk_read_only"},
		"a failing disk":          {err: fmt.Errorf("resume: read: %w", syscall.EIO), kind: "disk_failed"},
		"open files at the limit": {err: fmt.Errorf("resume: open: %w", syscall.EMFILE), kind: "open_files_exhausted"},
		"a coordination store that did not answer": {err: fmt.Errorf("engine: read the rebase: %w: "+
			"nats: timeout", coord.ErrUnavailable)},
		"the run's own": {err: errors.New("the turn could not be re-entered")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kind, ok := opts.OperatorsCondition(tc.err)
			if ok != (tc.kind != "") || (ok && kind != tc.kind) {
				t.Errorf("the coordinator reads %v as the operator's = %v (%q), want %q", tc.err, ok, kind,
					tc.kind)
			}
		})
	}
}

// THE ENGINE'S COORDINATOR BOUNDS A RESUME ANOTHER BUILD CAN MAKE BY THE LIVE
// NODES' BUILDS: a parked state a newer build wrote is resumable elsewhere while
// a node that reads its version is live, and not once none is; a state of this
// build's version only where another node reads it; a run with no parked state,
// or bytes that are no state, nowhere. ONLY THE VERSION IS READ
// (execstate.VersionOf), never the whole state (the package doc's "A resume
// another node or build can make").
//
// Mutation: leave ResumableElsewhere off the coordinator's options, read a
// state this build does not decode as this build's version, decode the whole
// state for its version, or mark the newer state's resume as the plain
// sandbox.ErrResumeUnavailable, and a row goes red.
func TestTheEnginesCoordinatorCountsAResumeAnotherBuildCanMakeOnlyOnceNoneIsLive(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	newer := execstate.Version + 1
	err := (&resumer{engine: &Engine{backends: &Backends{Coord: memory.New()}}}).resume(ctx,
		sandbox.ResumeRequest{Run: runParkedAtVersion(t, newer)})
	if !errors.Is(err, sandbox.ErrResumeElsewhere) {
		t.Fatalf("the resume of a newer state = %v, want it marked as one another build can make", err)
	}
	for name, tc := range map[string]struct {
		run       sandbox.PendingRun
		live      []int
		elsewhere bool
	}{
		"a newer state, a node of its build live": {run: runParkedAtVersion(t, newer),
			live: []int{execstate.Version, newer}, elsewhere: true},
		"a newer state, none of its build live": {run: runParkedAtVersion(t, newer),
			live: []int{execstate.Version}},
		"a newer state, no other node live": {run: runParkedAtVersion(t, newer)},
		"this build's state, another of it live": {run: runParkedAtVersion(t, execstate.Version),
			live: []int{execstate.Version}, elsewhere: true},
		"this build's state, alone": {run: runParkedAtVersion(t, execstate.Version)},
		"no parked state": {run: sandbox.PendingRun{TurnID: "t1"},
			live: []int{execstate.Version, newer}},
		"bytes that are no state": {run: sandbox.PendingRun{TurnID: "t1", ExecuteState: json.RawMessage(`[1]`)},
			live: []int{execstate.Version, newer}},
		"this build's version, a body it refuses, another of it live": {run: sandbox.PendingRun{
			TurnID: "t1", ExecuteState: json.RawMessage(`{"version":` + strconv.Itoa(execstate.Version) +
				`,"active_tool_names":"not a list"}`)}, live: []int{execstate.Version}, elsewhere: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			backend := memory.New()
			present(ctx, t, backend, "node-self", execstate.Version)
			for i, v := range tc.live {
				present(ctx, t, backend, "node-"+strconv.Itoa(i), v)
			}
			e := &Engine{backends: &Backends{Queue: queuememory.New(), Coord: backend},
				answers: testAnswers(), testNode: "node-self"}
			opts := e.coordinatorOptions(nil, nil)
			if opts.ResumableElsewhere == nil {
				t.Fatal("the coordinator is built with no bound on a resume another build can make")
			}
			got, err := opts.ResumableElsewhere(ctx, tc.run)
			if err != nil {
				t.Fatalf("the fleet's answer: %v", err)
			}
			if got != tc.elsewhere {
				t.Errorf("resumable elsewhere = %v, want %v", got, tc.elsewhere)
			}
		})
	}
}

// THE ENGINE'S COORDINATOR BOUNDS A RESUME A REPLY KIND KEEPS FROM THIS BUILD
// BY THE LIVE NODES' BUILDS, never by the parked state's version: resumable
// elsewhere while a live node runs another build (or does not say which, its
// heartbeat naming no build or carrying no status), not once every live node
// runs this build. A fleet that cannot be read is an error (the package doc's
// "A resume another node or build can make").
//
// Mutation: bound the reply's failure by the parked state's version again, or
// read a node that does not say its build, or says nothing at all, as this
// build's, or answer an unread fleet as nobody, and a row goes red.
func TestTheEnginesCoordinatorCountsAReplyNoBuildLiveReads(t *testing.T) {
	t.Parallel()
	live := t.Context()
	unread := sandbox.PendingRun{TurnID: "t1", AgentHandle: "app", Reply: "later",
		ExecuteState: runParkedAtVersion(t, execstate.Version).ExecuteState}
	if _, err := resumeReply(unread); !errors.Is(err, sandbox.ErrResumeElsewhere) {
		t.Fatalf("the premise: reply %q is one another build may read, got %v", unread.Reply, err)
	}
	for name, tc := range map[string]struct {
		builds    []string
		silent    bool
		down      bool
		elsewhere bool
	}{
		"two builds live":                         {builds: []string{version.String(), "v9.9.9"}, elsewhere: true},
		"a node that does not say its build":      {builds: []string{version.String(), ""}, elsewhere: true},
		"a node that says nothing at all":         {builds: []string{version.String()}, silent: true, elsewhere: true},
		"one build fleet-wide":                    {builds: []string{version.String(), version.String()}},
		"no other node live":                      {},
		"a fleet that cannot be read, two builds": {builds: []string{"v9.9.9"}, down: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			backend := coordtest.NewFaulty(memory.New())
			presentOnBuild(live, t, backend, "node-self", version.String())
			for i, build := range tc.builds {
				presentOnBuild(live, t, backend, "node-"+strconv.Itoa(i), build)
			}
			if tc.silent {
				// A HEARTBEAT THAT CARRIED NO STATUS, its hook having
				// overrun its share of the beat.
				present(live, t, backend, "node-silent", -1)
			}
			if tc.down {
				backend.Break(nil)
			}
			e := &Engine{backends: &Backends{Queue: queuememory.New(), Coord: backend},
				answers: testAnswers(), testNode: "node-self"}
			got, err := e.coordinatorOptions(nil, nil).ResumableElsewhere(live, unread)
			if tc.down {
				if err == nil {
					t.Fatalf("an unread fleet answered %v with no error; want the read's failure, "+
						"which counts nothing", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("the fleet's answer: %v", err)
			}
			if got != tc.elsewhere {
				t.Errorf("resumable elsewhere = %v, want %v", got, tc.elsewhere)
			}
		})
	}
}

// presentOnBuild publishes a live node's presence saying it runs build, or no
// build at all for "", and that it reads this build's parked state version, so
// only its build tells it apart.
func presentOnBuild(ctx context.Context, t *testing.T, backend coord.Backend, node, build string) {
	t.Helper()
	status := coord.NodeStatus{ParkedStateVersion: execstate.Version, Build: build}
	if _, _, err := backend.TryAcquire(ctx, coord.NodeResource(node), coord.AcquireOptions{
		Owner: node + ":1", TTL: time.Hour, Preferred: node, Ungated: true,
		Meta: map[string]any{coord.StatusKey: status.Meta()},
	}); err != nil {
		t.Fatal(err)
	}
}

// THE ENGINE'S COORDINATOR HOLDS THIS PROCESS'S TAKER LEASE IN THE NODE'S LEASE
// STORE, AND GIVES IT BACK ONLY AS THE SANDBOX STOPS. Every capture names the
// lease the coordinator holds, and a wait for another process's capture reads
// that lease in the same store, so the engine hands its coordinator its
// incarnation, its lease store and the seat lease TTL in force. The lease is
// claimed as the coordinator is built and released after its last keep, which
// the engine's stop reaches after the drain. No lease store: no taker at all.
// The store's clock is held still, so the deadline is exactly the TTL past the
// claim.
//
// Mutation: leave the incarnation, store or TTL off the coordinator's options,
// build its lease on another TTL, or keep its stop off the engine's: red.
func TestTheEnginesCoordinatorHoldsThisProcesssTakerLease(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	leases := memory.New()
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	leases.Clock = func() time.Time { return at }
	e := &Engine{backends: &Backends{Queue: queuememory.New(), Coord: leases}, id: "node-a",
		incarnation: "node-a:1", leaseTTL: 30 * time.Second, answers: testAnswers()}
	opts := e.coordinatorOptions(nil, nil)
	if opts.Incarnation != "node-a:1" || opts.TakerTTL != 30*time.Second {
		t.Errorf("the coordinator holds its taker lease as %q for %v, want this process's node-a:1 for the "+
			"seat lease TTL in force, 30s", opts.Incarnation, opts.TakerTTL)
	}
	if lease, _, err := leases.TryAcquire(ctx, coord.TakerResource("node-x:2"),
		coord.AcquireOptions{Owner: "node-x:2", TTL: time.Minute, Ungated: true}); err != nil || lease == nil {
		t.Fatalf("claim node-x:2's taker lease: %v", err)
	}
	if opts.Takers == nil {
		t.Fatal("the coordinator is handed no lease store to hold its taker lease in, beside the node's")
	}
	if lease, err := opts.Takers.Get(ctx, coord.TakerResource("node-x:2")); err != nil || lease == nil ||
		lease.Owner != "node-x:2" {
		t.Errorf("the coordinator reads another process's taker lease as %+v, %v; want node-x:2's from the "+
			"node's own lease store", lease, err)
	}

	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:        map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
		DefaultPlacement: sandbox.Direct,
		Runners:          map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rt, err := e.sandboxRuntimeOn(sandbox.NewCoordStore(memory.NewFleet()), manager)
	if err != nil {
		t.Fatalf("sandboxRuntimeOn: %v", err)
	}
	e.useSandbox(rt.pending, rt.coordinator)
	held := func() *coord.Lease {
		t.Helper()
		lease, err := leases.Get(ctx, coord.TakerResource("node-a:1"))
		if err != nil {
			t.Fatalf("read this process's taker lease: %v", err)
		}
		return lease
	}
	// CLAIMED AS THE COORDINATOR IS BUILT, for the TTL the engine handed it.
	lease := held()
	if lease == nil || lease.Owner != "node-a:1" {
		t.Fatalf("the engine's coordinator holds %+v as its taker lease, want one under this process's "+
			"incarnation", lease)
	}
	if want := at.Add(30 * time.Second); !lease.ExpiresAt.Equal(want) {
		t.Errorf("this process's taker lease runs to %v on the store's clock, want %v: the 30s TTL the "+
			"engine handed its coordinator", lease.ExpiresAt, want)
	}
	e.stopSandbox()
	if lease := held(); lease != nil {
		t.Errorf("the taker lease is still held by %s at %d once the sandbox stopped, want it given back",
			lease.Owner, lease.Epoch)
	}

	e.backends.Coord = nil
	if opts := e.coordinatorOptions(nil, nil); opts.Takers != nil || opts.Incarnation != "" {
		t.Errorf("an engine with no lease store hands its coordinator %#v as its taker lease's store, under %q; "+
			"want neither", opts.Takers, opts.Incarnation)
	}
}

// A DELIVERY MADE AS A SEAT'S CONTROL TOPIC IS ATTACHED WAITS FOR THE SEAT'S
// RECOVERY. The broker delivers what it held the moment the topic is attached,
// which on a seat already held the admission does not refuse, so the
// preparation hands the subscription to the recovery pass, which attaches it
// once the seat's recovery gate is closed: the delivery is refused its claim
// (sandbox.ErrSeatRecovering) and handed back as this node's, and the pass takes
// the run over (the package doc's "Everything else on the control topic NAKs").
//
// Mutation: attach the control topic before the pass and hand the pass no
// attach, as the preparation did, and the delivery claims the run ahead of it.
func TestADeliveryAsTheSeatsControlTopicIsAttachedWaitsForItsRecovery(t *testing.T) {
	t.Parallel()
	live := t.Context()
	c, seat := executorCompany(t)
	bridge := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	})
	e, _ := launchRig(t, c, bridge)
	agent, ok := c.Org.AgentIDFor(seat)
	if !ok {
		t.Fatal("the seat has no agent id")
	}
	rt := e.sandbox.Load()
	// A JOB OF THE SEAT'S RUNNING, whose completion was published while
	// nothing here consumed the seat's control topic.
	opened, openErr := rt.pending.BeginLaunch(live, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: seat.Handle(), AgentID: agent.String(), Role: seat.Name,
		Harness: "claude-code",
	}, sandbox.Fence{})
	if openErr != nil {
		t.Fatalf("BeginLaunch: %v", openErr)
	}
	if suspended, err := rt.pending.MarkSuspended(live, "t1", sandbox.Suspension{
		State: json.RawMessage(`{"version":4,"iteration":1}`), Iteration: 1}); err != nil || !suspended {
		t.Fatalf("MarkSuspended = %v, %v", suspended, err)
	}
	finished := types.ExecutorFinished{Agent: agent.String(), AgentHandle: seat.Handle(), RoleName: seat.Name,
		TurnID: "t1", LaunchID: opened.LaunchID, Harness: "claude-code"}
	attached := &deliveredOnAttach{EventQueue: e.backends.Queue, topic: topics.AgentControl(agent),
		ev: events.New(finished, events.TraceContext{})}
	e.backends.Queue = attached
	if err := attached.Start(live); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := e.prepareSeatSandbox(live, rt, placement.Seat{ID: agent, Handle: seat.Handle()}, 1,
		"node-a:1"); err != nil {
		t.Fatalf("prepareSeatSandbox: %v", err)
	}
	got, delivered := attached.outcome()
	if !delivered {
		t.Fatal("the premise: the seat's control topic was attached and nothing was delivered on it")
	}
	if !errors.Is(got.Err, sandbox.ErrSeatRecovering) || got.Outcome != queue.OutcomeNak || got.FailsMessage() {
		t.Fatalf("the delivery made as the seat's control topic was attached = %s (%v, fails the message "+
			"%v); want it refused by the seat's recovery gate and handed back as this node's", got.Outcome,
			got.Err, got.FailsMessage())
	}
	if row, found, err := rt.pending.Get(live, "t1"); err != nil || !found || row.Status != sandbox.StatusRunning {
		t.Fatalf("the run is %q (found %v, %v) once the seat was prepared; want it running, taken over as "+
			"its last holder left it", row.Status, found, err)
	}
}

// deliveredOnAttach is an event queue that, the moment a subscription to topic
// is attached, hands it ev as the broker hands a message it held for the topic,
// and keeps what the subscription made of it. It answers every call as the
// queue beneath it does.
type deliveredOnAttach struct {
	queue.EventQueue
	topic     string
	ev        *events.Event
	mu        sync.Mutex
	got       queue.Result
	delivered bool
}

func (q *deliveredOnAttach) Subscribe(ctx context.Context, topic, group string, h queue.Handler) error {
	if err := q.EventQueue.Subscribe(ctx, topic, group, h); err != nil {
		return err
	}
	if topic != q.topic {
		return nil
	}
	got := h(ctx, q.ev)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.got, q.delivered = got, true
	return nil
}

// outcome is what the subscription made of the delivery, and whether one was
// made.
func (q *deliveredOnAttach) outcome() (queue.Result, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.got, q.delivered
}

// A FAKE RUN STATES WHAT ITS HARNESS'S RUNNER STATES: the double stands in for
// the box, never for the harness, so its reading of the main thread, the
// window it pins and what it says can switch its notes off are the real
// runner's, and a fake run is collected and warned about as its harness's is.
//
// Mutation: drop any one copy in fakeRunners, and a harness goes red.
func TestAFakeRunStatesWhatItsHarnesssRunnerStates(t *testing.T) {
	t.Parallel()
	real, stated := sandboxRunners(), false
	for name, runner := range fakeRunners(nil) {
		fake, want := runner.(*sandbox.FakeRunner), real[name].(*executor.Runner)
		if fake.Thread != sandbox.ThreadTraitsOf(want) {
			t.Errorf("%s: the fake reads its thread as %+v, its runner as %+v", name, fake.Thread,
				sandbox.ThreadTraitsOf(want))
		}
		if fake.NotesOff != want.NotesOffWhy() {
			t.Errorf("%s: the fake says %q switches its notes off, its runner %q", name, fake.NotesOff,
				want.NotesOffWhy())
		}
		stated = stated || want.NotesOffWhy() != ""
		for _, model := range []string{"sonnet", "opus", "anthropic/claude-sonnet-4-5"} {
			if got, pinned := fake.PinnedWindow(model), want.PinnedWindow(model); got != pinned {
				t.Errorf("%s: the fake pins %s to %d, its runner to %d", name, model, got, pinned)
			}
		}
	}
	if !stated {
		t.Error("no harness's runner says what switches its notes off, so the copy above proves nothing")
	}
}
