package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
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
func runOn(t *testing.T, c *Company, seat *org.Role, seatEnv map[string]string) codingRun {
	t.Helper()
	member, err := c.Models.Head(seat, phase.Execute)
	if err != nil {
		t.Fatalf("%s's executor chain: %v", seat.Name, err)
	}
	return newCodingRun(member.Provider.(*harness.Provider), seatEnv)
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
			e.runEnvironment(t.Context(), nil, seat, &org.RoleSandbox{Env: own}, nil).env)
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
		e.runEnvironment(t.Context(), nil, seat, &org.RoleSandbox{Env: map[string]string{"GITHUB_TOKEN": "ghp_x"}}, nil).env)
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
	cmd := executor.OpenCode{}.Command(sandbox.RunRequest{Brief: "x", Model: run.model}, executor.Paths{}, "")
	if script := strings.ReplaceAll(cmd, `'\''`, "'"); !strings.Contains(script, "--model '"+text+"'") {
		t.Errorf("the agent run gave:\n%s\nwant --model %q, the value every text call passes", cmd, text)
	}
}

// A LOGIN OPENCODE KEEPS IN ITS OWN DATABASE FOLLOWS A RUN INTO A LOCAL BOX: the
// entry's exported login, mapped where the runner imports it from — mapped
// whether or not anybody has signed the entry in yet, because the box reads
// the login as it stands when it is seeded and is given only what it holds
// then. A map of what was there when the run was resolved named a login the
// shared directory may have moved past by the time the box was seeded.
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
		if got := run.files[executor.CredentialDocument]; got != doc || len(run.files) != 1 {
			t.Errorf("signed in %v: the box is seeded from %q at %s, want the shared login %q "+
				"and nothing else (files %v)", signedIn, got, executor.CredentialDocument, doc, run.files)
		}
	}
}

// The login travels as a host-path MAP for the local backend to seed, and as
// a token in the run environment. The files are offered rather than exported
// because they carry a refresh token whose rotation is shared fleet state.
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
	files, env := run.files, run.env
	host, mapped := files[".claude/.credentials.json"]
	if !mapped {
		t.Fatalf("the login was not offered to the box: %v", files)
	}
	if host != filepath.Join(credentials, ".credentials.json") {
		t.Errorf("mapped to %q, want the shared login on the engine host", host)
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
	files := runOn(t, c, seat, nil).files
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
// `buildSandboxProvider` is what turns one into a running backend. Nothing
// connects them, and when they last disagreed the config's DEFAULT was the
// offender: `providers.sandbox: {}` validated, reported a configured sandbox
// on every operator surface, and failed at the first launch with an error
// naming a backend nobody had written.
//
// It fails in the direction that is hardest to see — the config says yes and
// the runtime says no — and only for a company that actually runs code, so a
// boot proves nothing. Hence a test that walks the set.
func TestEveryConfiguredPlacementCanBeBuilt(t *testing.T) {
	t.Parallel()
	for _, placement := range config.Placements {
		spec := &config.SandboxProvider{
			// The one backend with a required credential of its own: the
			// API authenticates every call, so a provider built without
			// a key would report a configured sandbox and 401 at the
			// first create.
			E2B: &config.E2BSandbox{APIKey: "e2b_test_key"},
			// An image, because one of the two local cells needs it and
			// a backend that cannot be built for `container` is exactly
			// what this test exists to catch.
			Local: &config.LocalSandbox{Image: "example.invalid/box:1"},
		}
		if !spec.Configured(placement) {
			t.Fatalf("a catalogue with every backend does not configure %q, so "+
				"the closed set names a cell providers.sandbox cannot hold", placement)
		}
		// NO RESOLVER: this asks whether each backend can be CONSTRUCTED,
		// and a nil resolver hands the literal through, which is what an
		// in-process caller wrote.
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
	got := localOptions(&config.LocalSandbox{
		Image: "example.invalid/box:1",
		RuntimeEnv: map[string]string{
			"SSH_AUTH_SOCK": "${SSH_AUTH_SOCK}",
			"AWS_ROLE_ARN":  "${REGISTRY_ROLE}",
			"AWS_PROFILE":   "registry-pull",
		},
	}, env, config.PlacementContainer).RuntimeEnv
	want := map[string]string{
		"SSH_AUTH_SOCK": "/run/user/1000/ssh-agent.sock",
		"AWS_ROLE_ARN":  "arn:aws:iam::123456789012:role/pull",
		"AWS_PROFILE":   "registry-pull",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("RuntimeEnv = %v, want %v", got, want)
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
		if provider.Kind() != "fake" {
			t.Errorf("placement %q built %q, not the double", placement, provider.Kind())
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
// waiter, so the job ran on in a box billed to its provider's TTL.
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
func TestARetriedCodingRunIsChargedToTheFleetOnce(t *testing.T) {
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
func TestAnOverCapCodingRunIsRecordedOnBothCounters(t *testing.T) {
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
func TestAnOverCapCodingRunIsChargedToTheFleetOnce(t *testing.T) {
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
	if _, err := fleet.CreateSandboxRun(t.Context(), "t-garbled", garbled); err != nil {
		t.Fatalf("CreateSandboxRun: %v", err)
	}
	bare := &Engine{backends: &Backends{Fleet: fleet}}
	// Refused over the READABLE run, and over it alone: a listing that failed
	// on the unreadable record would refuse too, for the wrong reason.
	if err := bare.retireSeatRuns(t.Context(), swe, "swe", "retirement:1", 3); err == nil ||
		!strings.Contains(err.Error(), "still has 1 executor records") {
		t.Fatalf("the retirement of a seat with one readable run = %v, want it refused over "+
			"that run", err)
	}
	if err := bare.retireSeatRuns(t.Context(), pm, "pm", "retirement:1", 3); err != nil {
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
	if err := bare.retireSeatRuns(t.Context(), swe, "swe", "retirement:1", 3); err == nil {
		t.Fatal("a node with no coordinator let the retirement proceed over a recorded run")
	}
	if err := bare.retireSeatRuns(t.Context(), swe, "", "retirement:1", 3); err == nil {
		t.Fatal("a node with no coordinator let the retirement of a mailbox with no handle " +
			"proceed over its seat's recorded run")
	}
	if err := bare.retireSeatRuns(t.Context(), pm, "pm", "retirement:1", 3); err != nil {
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
		Answers:  testAnswers(),
		Audience: noAudience{},
		Queue:    &publishRecorder{}, Pending: store, Manager: manager,
		Resume: &resumer{engine: equipped},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	equipped.useSandbox(store, coordinator)
	if err := equipped.retireSeatRuns(t.Context(), swe, "", "retirement:1", 3); err != nil {
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

// useSandbox installs a runtime a case assembled itself, as [Engine.startSandbox]
// publishes the one it builds.
func (e *Engine) useSandbox(pending sandbox.PendingStore, coordinator *sandbox.Coordinator) {
	if e.sandboxSeats == nil {
		e.sandboxSeats = map[string]bool{}
	}
	e.sandbox.Store(&sandboxRuntime{pending: pending, coordinator: coordinator})
}
