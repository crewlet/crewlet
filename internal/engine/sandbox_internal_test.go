package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The knob has three states and the manager's input carries a pointer for
// exactly that reason. Getting this wrong is not visible as a config error: a
// seat silently loses its checkout the moment a coding agent asks a question,
// which is the case a paused box exists for.
func TestTheSeatsPauseOverrideDistinguishesInheritFromNever(t *testing.T) {
	never := 0.0
	held := 600.0
	legacy := -1.0
	longhand := -30.0

	cases := []struct {
		name string
		gate config.RoleSandbox
		want *time.Duration
	}{
		{"unset inherits", config.RoleSandbox{}, nil},
		{"an explicit zero never pauses", config.RoleSandbox{PauseTTLSeconds: &never}, dur(0)},
		{"a set value is used", config.RoleSandbox{PauseTTLSeconds: &held}, dur(600 * time.Second)},
		// -1 is the field's earlier spelling of "inherit"; any negative
		// value reads the same way, because none of them can mean a
		// duration and "no expiry" is the leak the knob exists to prevent.
		{"the legacy -1 inherits", config.RoleSandbox{PauseTTLSeconds: &legacy}, nil},
		{"any negative inherits", config.RoleSandbox{PauseTTLSeconds: &longhand}, nil},
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

// A coding run needs the model its seat was pointed at. Nothing filled this
// before, so OpenCode — which must declare its own provider rather than read
// a credential from the environment — resolved a bare model against its own
// catalogue and the vendor's default endpoint.
func TestTheSandboxGetsTheSeatsResolvedModel(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	c := companyFor(t, `
name: Acme
providers:
  llm:
    gateway:
      type: openai-compatible
      model: gpt-4o
      base_url: https://llm.example.com/v1
      api_keys: ["${OPENAI_API_KEY}"]
roles:
  - name: Engineer
    handle: eng
    llm: gateway
`)
	got, credentials, env := sandboxLLM(c, seatNamed(t, c, "Engineer"))
	if got == nil {
		t.Fatal("the sandbox got no model at all")
	}
	if got.Model != "gpt-4o" {
		t.Errorf("Model = %q", got.Model)
	}
	if got.BaseURL != "https://llm.example.com/v1" {
		t.Errorf("BaseURL = %q — a custom gateway must not be resolved away", got.BaseURL)
	}
	if credentials != nil || env != nil {
		t.Errorf("an API entry contributed credential files or env: %v %v", credentials, env)
	}
}

// llm_sandbox falls back to `llm`, which IS the seat's own model: the turn's
// work happens in one conversation, so there is no separate executor key to
// inherit — sandboxed work is that same work, done somewhere else.
func TestTheSandboxModelFallsBackToTheSeatsOwn(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	c := companyFor(t, `
name: Acme
providers:
  llm:
    big:
      type: openai
      model: gpt-4o
      api_keys: ["${OPENAI_API_KEY}"]
    coder:
      type: openai
      model: gpt-4o-coder
      api_keys: ["${OPENAI_API_KEY}"]
roles:
  - name: Engineer
    handle: eng
    llm: big
`)
	got, _, _ := sandboxLLM(c, seatNamed(t, c, "Engineer"))
	if got == nil || got.Model != "gpt-4o" {
		t.Fatalf("the sandbox model = %+v, want the seat's own model", got)
	}
	// And its own key still wins, or the fallback would be the only path.
	c = companyFor(t, `
name: Acme
providers:
  llm:
    big:
      type: openai
      model: gpt-4o
      api_keys: ["${OPENAI_API_KEY}"]
    coder:
      type: openai
      model: gpt-4o-coder
      api_keys: ["${OPENAI_API_KEY}"]
roles:
  - name: Engineer
    handle: eng
    llm: big
    llm_sandbox: coder
`)
	got, _, _ = sandboxLLM(c, seatNamed(t, c, "Engineer"))
	if got == nil || got.Model != "gpt-4o-coder" {
		t.Fatalf("the sandbox model = %+v, want llm_sandbox's own", got)
	}
}

// A subscription entry's providers.llm type is "cli-agent" for every vendor,
// so a coding agent resolving "<family>/<model>" would address a Claude
// subscription's "sonnet" as an OpenAI model. The profile's vendor is what
// names the family.
func TestASubscriptionSeatAddressesItsRealVendor(t *testing.T) {
	state := t.TempDir()
	t.Setenv(config.CLIHomeEnv, state)
	c := companyFor(t, `
name: Acme
providers:
  llm:
    subscription:
      type: cli-agent
      model: sonnet
      cli:
        agent: claude-code
roles:
  - name: Engineer
    handle: eng
    llm: subscription
`)
	got, _, _ := sandboxLLM(c, seatNamed(t, c, "Engineer"))
	if got == nil {
		t.Fatal("the sandbox got no model at all")
	}
	if got.ProviderType != "anthropic" {
		t.Errorf("ProviderType = %q, want the CLI's own vendor family", got.ProviderType)
	}
	if got.BaseURL != "" {
		t.Errorf("BaseURL = %q. A cli-agent entry talks to its vendor, so declaring "+
			"a custom endpoint points the coding agent at nothing", got.BaseURL)
	}
}

// The login travels as a host-path MAP for the local backend to seed, and as
// a token in the run environment. The files are offered rather than exported
// because they carry a refresh token whose rotation is shared fleet state.
func TestASubscriptionSeatCarriesItsLoginIntoTheBox(t *testing.T) {
	state := t.TempDir()
	t.Setenv(config.CLIHomeEnv, state)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-travelling")

	credentials := filepath.Join(state, "subscription", "credentials")
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
  llm:
    subscription:
      type: cli-agent
      model: sonnet
      cli:
        agent: claude-code
roles:
  - name: Engineer
    handle: eng
    llm: subscription
`)
	_, files, env := sandboxLLM(c, seatNamed(t, c, "Engineer"))
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

// Seeding files that do not exist would fail inside the run with a puzzling
// error instead of the CLI's plain "not authenticated".
func TestNoLoginOffersNoCredentialFiles(t *testing.T) {
	t.Setenv(config.CLIHomeEnv, t.TempDir())
	c := companyFor(t, `
name: Acme
providers:
  llm:
    subscription:
      type: cli-agent
      model: sonnet
      cli:
        agent: claude-code
roles:
  - name: Engineer
    handle: eng
    llm: subscription
`)
	_, files, _ := sandboxLLM(c, seatNamed(t, c, "Engineer"))
	if len(files) != 0 {
		t.Errorf("files that do not exist were offered to the box: %v", files)
	}
}

// The direction of the merge is a decision, not a detail: an operator who
// named a variable in role.sandbox.env meant that value — including the
// deliberate choice to point one seat's coding runs at a different account.
func TestTheOperatorsSandboxEnvWinsOverTheResolvedCredential(t *testing.T) {
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
// on every operator surface, and failed at the first coding run with an error
// naming a backend nobody had written.
//
// It fails in the direction that is hardest to see — the config says yes and
// the runtime says no — and only for a company that actually runs code, so a
// boot proves nothing. Hence a test that walks the set.
func TestEveryConfiguredPlacementCanBeBuilt(t *testing.T) {
	t.Parallel()
	// EXACTLY ONE CELL IS EXEMPT, and it is exempt because it is not a
	// backend: `self` is the executor's own agent-mode run, so nothing
	// builds a provider for it. Asserted rather than assumed, so a fifth
	// cell added tomorrow cannot quietly join the exemption.
	for _, p := range config.Placements {
		if p.NeedsBackend() != (p != config.PlacementSelf) {
			t.Fatalf("%q disagrees with itself about needing a backend", p)
		}
	}
	for _, placement := range config.BackendPlacements() {
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
			t.Errorf("run_in %q built no provider and no error, so a "+
				"sandbox-enabled seat plans around a box it never gets", placement)
		}
	}
}

// A CATALOGUE WITH NOTHING IN IT IS NOT A SANDBOX, and the distinction is the
// one an operator's half-finished edit lands on: `providers.sandbox: {}`
// parses, and if it reported itself enabled the engine would build a manager
// with no backends and offer run_sandbox to every gated seat.
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
	for _, placement := range config.BackendPlacements() {
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
	e := &Engine{sandboxPending: store}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Queue: queue, Pending: store, Manager: manager, Resume: &resumer{engine: e},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	e.sandboxCoordinator = coordinator
	box, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", Role: "SWE",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := store.AttachSandbox(t.Context(), "t1",
		sandbox.BoxRef{SandboxID: box.ID(), CommandID: "1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}

	e.failSuspension(t.Context(), "t1", "sandbox_suspension_missing",
		"the turn suspended but recorded no conversation", nil)

	if got, found, err := store.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("Get = %+v, found %v, %v; want the run ended and its record gone", got, found, err)
	}
	if killed := provider.KilledIDs(); len(killed) != 1 || killed[0] != box.ID() {
		t.Fatalf("killed %v, want the running job's box %q reclaimed", killed, box.ID())
	}
	if !queue.published(types.SandboxRunFailed{}.EventType()) {
		t.Fatal("the lost run was not announced")
	}
}

// belowCeilingRuns is the fleet's run records behind a NATS server whose
// max_payload is below the contract's ceiling: it refuses, as too large, every
// update of a run's record and every part of a suspension longer than limit.
type belowCeilingRuns struct {
	*memory.Fleet
	limit int
}

func (f belowCeilingRuns) refuses(value []byte) error {
	if len(value) > f.limit {
		return fmt.Errorf("the server announces %d bytes: %w", f.limit, coord.ErrTooLarge)
	}
	return nil
}

func (f belowCeilingRuns) UpdateSandboxRun(ctx context.Context, turnID string, value []byte, version uint64) (bool, error) {
	if err := f.refuses(value); err != nil {
		return false, err
	}
	return f.Fleet.UpdateSandboxRun(ctx, turnID, value, version)
}

func (f belowCeilingRuns) CreateSuspensionPart(ctx context.Context, turnID, launchID string, part int, value []byte) (bool, error) {
	if err := f.refuses(value); err != nil {
		return false, err
	}
	return f.Fleet.CreateSuspensionPart(ctx, turnID, launchID, part, value)
}

// unreachableRuns is the fleet's run records with a store that stops
// answering writes to them once down is set.
type unreachableRuns struct {
	*memory.Fleet
	down *atomic.Bool
}

func (u unreachableRuns) UpdateSandboxRun(ctx context.Context, turnID string, value []byte, version uint64) (bool, error) {
	if u.down.Load() {
		return false, fmt.Errorf("broker restarting: %w", coord.ErrUnavailable)
	}
	return u.Fleet.UpdateSandboxRun(ctx, turnID, value, version)
}

// suspendedState is a suspended executor conversation whose one message carries
// content bytes of text, ending on the run_sandbox call it is waiting on.
func suspendedState(content int) execstate.State {
	return execstate.State{
		Messages: []llm.Message{{
			Role: "assistant", Content: strings.Repeat("w", content),
			ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "run_sandbox"}},
		}},
		PendingCallID: "call-1", PendingCallName: "run_sandbox",
		Task: "fix the flaking api test",
	}
}

// launchedRun opens a launch on a fresh box and attaches it, as the run_sandbox
// tool does before the turn suspends: the run is launching, with a job running
// in its box and no conversation on its record yet.
func launchedRun(t *testing.T, store sandbox.PendingStore, provider *sandbox.FakeProvider) sandbox.Sandbox {
	t.Helper()
	box, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", AgentID: chargeAgent, Role: "SWE", CodingAgent: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := store.AttachSandbox(t.Context(), "t1", sandbox.BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", CodingAgent: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	return box
}

// A SUSPENSION NO RECORD WOULD TAKE FAILS ITS RUN SAYING WHAT TO DO. The run's
// failure is its only lasting account — its record is deleted and its box
// reclaimed — so its detail is the sentence an operator acts on. A conversation
// a server refused as too large, down to the smallest part the store splits
// to, is a server whose max_payload is below the contract's ceiling, and the
// detail names that setting and the value to raise it to. Any other failure is
// the store's, and the detail does not claim a remedy it does not have.
func TestASuspensionNoRecordTakesFailsItsRunNamingTheRemedy(t *testing.T) {
	down := &atomic.Bool{}
	for _, tc := range []struct {
		name    string
		records sandbox.RunRecords
		// fail is what makes the store fail once the run has launched.
		fail func()
		// names is what the detail must say; not, what it must not.
		names, not []string
	}{
		{
			name: "a server below the ceiling",
			// The run's own record is well under the limit, so the
			// launch lands; the conversation is not.
			records: belowCeilingRuns{Fleet: memory.NewFleet(), limit: 32 << 10},
			fail:    func() {},
			names:   []string{"max_payload", fmt.Sprint(queue.MaxPayloadBytes), "every server"},
		},
		{
			name:    "a store that stopped answering",
			records: unreachableRuns{Fleet: memory.NewFleet(), down: down},
			fail:    func() { down.Store(true) },
			names:   []string{"sandbox_suspension_unwritable"},
			not:     []string{"max_payload"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := sandbox.NewCoordStore(tc.records)
			provider := sandbox.NewFakeProvider()
			manager, err := sandbox.NewManager(sandbox.ManagerOptions{
				Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
				Runners:   map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
			})
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			published := &publishRecorder{}
			e := &Engine{sandboxPending: store}
			coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
				Queue: published, Pending: store, Manager: manager, Resume: &resumer{engine: e},
			})
			if err != nil {
				t.Fatalf("NewCoordinator: %v", err)
			}
			e.sandboxCoordinator = coordinator
			box := launchedRun(t, store, provider)
			tc.fail()

			e.keepSuspension(t.Context(), "t1", suspendedState(100<<10))

			if got, found, err := store.Get(t.Context(), "t1"); err != nil || found {
				t.Fatalf("Get = %+v, found %v, %v; want the run ended and its record gone", got, found, err)
			}
			if killed := provider.KilledIDs(); len(killed) != 1 || killed[0] != box.ID() {
				t.Fatalf("killed %v, want the running job's box %q reclaimed", killed, box.ID())
			}
			failed := published.failures()
			if len(failed) != 1 || failed[0].Reason != types.SandboxFailureSuspensionUnrecorded {
				t.Fatalf("announced %+v, want the run failed as %q", failed,
					types.SandboxFailureSuspensionUnrecorded)
			}
			for _, want := range tc.names {
				if !strings.Contains(failed[0].Detail, want) {
					t.Errorf("the detail %q does not name %q", failed[0].Detail, want)
				}
			}
			for _, unwanted := range tc.not {
				if strings.Contains(failed[0].Detail, unwanted) {
					t.Errorf("the detail %q names %q, a remedy this failure does not have",
						failed[0].Detail, unwanted)
				}
			}
		})
	}
}

// THE ENGINE'S OWN RESUMER RE-ENTERS A CONVERSATION KEPT IN PARTS, WHOLE.
//
// A conversation past what the run's record keeps one within is held in parts,
// with a reference to them on the record in its place — a map the resumer
// cannot decode, since it names no version this build writes. So the resume has
// to be handed the whole the parts hold, and this drives the real path: the
// coordinator's claim and read, then the engine's resumer decoding the state.
// A node with no applied company answers ErrResumeUnavailable only AFTER the
// state decoded, so that answer is the proof the whole arrived; the reference
// arriving instead is refused as an unknown version first.
func TestTheEnginesResumerReentersAConversationKeptInPartsWhole(t *testing.T) {
	store := sandbox.NewCoordStore(memory.NewFleet())
	provider, runner := sandbox.NewFakeProvider(), sandbox.NewFakeRunner("claude-code")
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
		Runners:   map[string]sandbox.Runner{"claude-code": runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	e := &Engine{sandboxPending: store}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Queue: &publishRecorder{}, Pending: store, Manager: manager, Resume: &resumer{engine: e},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	e.sandboxCoordinator = coordinator
	box := launchedRun(t, store, provider)

	blob, err := execstate.Encode(suspendedState(5 << 20))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if suspended, err := store.MarkSuspended(t.Context(), "t1", blob); err != nil || !suspended {
		t.Fatalf("MarkSuspended = %v, %v", suspended, err)
	}
	run, found, err := store.Get(t.Context(), "t1")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if _, whole := run.ExecuteState["messages"]; whole || len(run.ExecuteState) != 1 {
		t.Fatalf("the record holds %d keys of the conversation, want the reference to its "+
			"parts alone: the premise", len(run.ExecuteState))
	}
	runner.Finish(sandbox.Result{Success: true, Text: "fixed"})

	completion := types.SandboxRunCompleted{
		TurnID: "t1", LaunchID: run.LaunchID, AgentHandle: "swe", Agent: chargeAgent,
		RoleName: "SWE", SandboxID: box.ID(), CodingAgent: "claude-code",
	}
	err = coordinator.OnCompleted(t.Context(), completion, events.New(completion, events.TraceContext{}))
	if errors.Is(err, execstate.ErrUnknownVersion) {
		t.Fatalf("the resumer was handed the reference rather than the conversation: %v", err)
	}
	if !errors.Is(err, sandbox.ErrResumeUnavailable) || !strings.Contains(err.Error(), "no applied company") {
		t.Fatalf("OnCompleted = %v, want the conversation decoded and the resume sent on for "+
			"want of a company", err)
	}
}

// publishRecorder is the slice of the queue a coordinator publishes through.
type publishRecorder struct {
	mu     sync.Mutex
	events []*events.Event
}

// failures is every run failure announced, once each: a failure goes to the
// board's topic and to the seat's control topic, as one event.
func (r *publishRecorder) failures() []types.SandboxRunFailed {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []types.SandboxRunFailed
	seen := map[string]bool{}
	for _, ev := range r.events {
		failed, ok := ev.Data.(*types.SandboxRunFailed)
		if !ok || seen[ev.ID.String()] {
			continue
		}
		seen[ev.ID.String()] = true
		out = append(out, *failed)
	}
	return out
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
	completion  types.SandboxRunCompleted
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

// newChargeRig seeds one running coding run whose job has finished having
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
		Queue: discardQueue{}, Pending: store, Manager: manager, Resume: resume,
		Account: sandboxAccountant{
			budgets: fleet,
			caps:    func(string) (int, int) { return orgCap, seatCap },
		},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	box, err := provider.Create(ctx, sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", AgentID: chargeAgent, Role: "SWE",
		CodingAgent: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := store.AttachSandbox(ctx, "t1", sandbox.BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", CodingAgent: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	if suspended, err := store.MarkSuspended(ctx, "t1", map[string]any{
		"pending_tool_name": "run_sandbox",
	}); err != nil || !suspended {
		t.Fatalf("MarkSuspended = %v, %v", suspended, err)
	}
	runner.Finish(sandbox.Result{Success: true, Text: "done", InputTokens: tokens})
	run, found, err := store.Get(ctx, "t1")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}

	return &chargeRig{
		fleet: fleet, coordinator: coordinator,
		completion: types.SandboxRunCompleted{
			TurnID: "t1", LaunchID: run.LaunchID, AgentHandle: "swe", Agent: chargeAgent,
			RoleName: "SWE", SandboxID: box.ID(), CodingAgent: "claude-code",
		},
	}
}

func (r *chargeRig) deliver(t *testing.T) error {
	t.Helper()
	return r.coordinator.OnCompleted(t.Context(), r.completion, events.New(r.completion, events.TraceContext{}))
}

func (r *chargeRig) used(t *testing.T, scope string) int {
	t.Helper()
	got, err := r.fleet.Used(t.Context(), scope)
	if err != nil {
		t.Fatalf("Used(%s): %v", scope, err)
	}
	return got
}

// A CODING RUN IS CHARGED TO THE FLEET ONCE, however often its completion
// comes back. A node that lost the seat answers ErrResumeUnavailable, the
// claim reverts and the completion is delivered again, and each of those
// passes collected the same finished job and charged it to both counters: the
// seat's allowance and the company's shrank by one run per retry.
func TestARetriedCodingRunIsChargedToTheFleetOnce(t *testing.T) {
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

// leftover is a seat's remaining allowance, or a counter that cannot be read.
type leftover struct {
	left int
	err  error
}

func (l leftover) Remaining(context.Context) (int, error) { return l.left, l.err }

// THE PRE-FLIGHT FLOOR, three-valued like every other budget read in this
// engine. turn_engine.sandbox_min_budget_tokens was validated, schema'd and
// documented and read by nothing, so a company that set it got a new revision
// and no behaviour.
func TestACodingRunIsRefusedBelowTheBudgetFloor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Below the floor: refused, and the message says what to do instead —
	// a coding run costs a box, a clone and a toolchain install before it
	// produces a token.
	err := sandboxHeadroom(ctx, leftover{left: 500}, 2000)
	if err == nil {
		t.Fatal("a seat with 500 tokens launched a run needing 2000")
	}
	for _, want := range []string{"500", "2000", "your own tools"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is missing %q: %v", want, err)
		}
	}

	// A READ THAT FAILED is refused too: launching a box on an unknown
	// budget is how a company discovers its ceiling by spending past it.
	if err := sandboxHeadroom(ctx,
		leftover{err: errors.New("the coordination store is unreachable")}, 2000); err == nil {
		t.Error("a run launched on a budget nobody could read")
	}

	// And the two states that legitimately pass, or the assertions above
	// hold for a floor that refuses everything.
	if err := sandboxHeadroom(ctx, leftover{left: 50_000}, 2000); err != nil {
		t.Errorf("a seat with headroom was refused: %v", err)
	}
	if err := sandboxHeadroom(ctx, nil, 2000); err != nil {
		t.Errorf("a company with no token budget was refused: %v", err)
	}
	if err := sandboxHeadroom(ctx, leftover{left: 0}, 0); err != nil {
		t.Errorf("an unset floor refused a run: %v", err)
	}
}

// A RETIRED SEAT'S RUNS ARE ENDED BY THE NODE THAT RETIRES IT. Through the
// coordinator where this node has one, which reclaims each box; and where it
// has none, the retirement is refused while the fleet still records a run for
// the seat, because a node that cannot reach a box must not delete the
// subscriptions that run's completion and answer travel on.
func TestRetiringASeatEndsItsRunsOrRefusesWithoutACoordinator(t *testing.T) {
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	if err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", Role: "SWE",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}

	bare := &Engine{backends: &Backends{Fleet: fleet}}
	if err := bare.retireSeatRuns(t.Context(), "swe", "retirement:1", 3); err == nil {
		t.Fatal("a node with no coordinator let the retirement proceed over a recorded run")
	}
	if err := bare.retireSeatRuns(t.Context(), "pm", "retirement:1", 3); err != nil {
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
		Queue: &publishRecorder{}, Pending: store, Manager: manager,
		Resume: &resumer{engine: equipped},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	equipped.sandboxCoordinator = coordinator
	if err := equipped.retireSeatRuns(t.Context(), "swe", "retirement:1", 3); err != nil {
		t.Fatalf("retireSeatRuns: %v", err)
	}
	if _, found, err := store.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("the retired seat's run survived (found %v, %v)", found, err)
	}
}

// A TURN'S CODING RUNS STOP AT THE EXECUTOR'S ROUND CAP, and the refusal names
// the setting. Each run suspends the executor and its resume re-enters with a
// fresh tool loop, so a round that relaunched on every resume would be bounded
// by nothing the loop counts: the run's row counts the launches, and the one
// that would pass turn_engine.max_tool_rounds is refused before any box is
// provisioned. The launch also carries the turn's own instant onto the row, for
// the resume that re-enters it.
func TestATurnsCodingRunsStopAtTheExecutorsRoundCap(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	seat := &org.Role{Name: "SWE", DeclaredHandle: "swe"}
	c := &Company{
		Org: &org.Organization{Name: "Acme", Roles: []*org.Role{seat}},
		Config: &config.Company{
			Roles: []config.Role{{Name: "SWE", Sandbox: &config.RoleSandbox{
				Enabled: true, RunIn: config.PlacementE2B,
			}}},
			TurnEngine: config.TurnEngine{MaxToolRounds: 2},
		},
	}
	e := launchReadyEngine(t, c)
	if err := e.backends.Queue.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	triggered := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	turn := &turnctx.Turn{RunID: "t1", Seat: seat, Org: c.Org, TriggeredAt: triggered}
	l := &launcher{engine: e}
	for launch := range 2 {
		if _, err := l.Launch(ctx, turn, "fix the flaky test"); err != nil {
			t.Fatalf("launch %d of 2: %v", launch+1, err)
		}
	}
	if run, _, err := e.sandboxPending.Get(ctx, "t1"); err != nil || !run.TriggeredAt.Equal(triggered) {
		t.Errorf("the row carries the turn's instant %v (%v), want %v", run.TriggeredAt, err, triggered)
	}

	_, err := l.Launch(ctx, turn, "fix the flaky test again")
	if !errors.Is(err, sandbox.ErrLaunchCap) || !strings.Contains(err.Error(), "turn_engine.max_tool_rounds (2)") {
		t.Fatalf("the third launch = %v, want a refusal naming turn_engine.max_tool_rounds", err)
	}
}
