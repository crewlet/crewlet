package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/org"
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
		t.Errorf("BaseURL = %q — a cli-agent entry talks to its vendor, so declaring "+
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

// EVERY BACKEND THE CONFIG ACCEPTS IS ONE THIS ENGINE CAN BUILD.
//
// `config.SandboxTypes` is the closed set an operator's `type:` is checked
// against, and `buildSandboxProvider` is what turns one into a running
// backend. Nothing connects them, and when they last disagreed the config's
// DEFAULT was the offender: `providers.sandbox: {}` validated, reported a
// configured sandbox on every operator surface, and failed at the first
// coding run with an error naming a backend nobody had written.
//
// It fails in the direction that is hardest to see — the config says yes and
// the runtime says no — and only for a company that actually runs code, so a
// boot proves nothing. Hence a test that walks the set.
func TestEveryConfiguredSandboxTypeCanBeBuilt(t *testing.T) {
	t.Parallel()
	for _, kind := range config.SandboxTypes {
		spec := &config.SandboxProvider{Type: kind}
		if kind == config.SandboxE2B {
			// The one backend with a required credential of its own: the
			// API authenticates every call, so a provider built without
			// a key would report a configured sandbox and 401 at the
			// first create.
			spec.APIKey = "e2b_test_key"
		}
		if kind == config.SandboxLocal {
			// The one backend with a required block of its own: type
			// local with none would silently take `direct` containment,
			// which runs the coding agent as the engine's user.
			spec.Local = &config.LocalSandbox{Containment: config.ContainmentDirect}
		}
		if kind == config.SandboxNone {
			// Not a backend — it is how an operator says "no code work",
			// and buildSandbox never reaches the switch for it.
			if spec.Enabled() {
				t.Errorf("%q reports itself enabled, so the engine would try "+
					"to build a backend for the value that means there is none", kind)
			}
			continue
		}
		// NO RESOLVER: this asks whether each backend can be CONSTRUCTED,
		// and a nil resolver hands the literal through, which is what an
		// in-process caller wrote.
		provider, err := buildSandboxProvider(spec, nil)
		if err != nil {
			t.Errorf("providers.sandbox.type %q is accepted by the config and "+
				"cannot be built: %v", kind, err)
			continue
		}
		if provider == nil {
			t.Errorf("providers.sandbox.type %q built no provider and no error, "+
				"so a sandbox-enabled seat plans around a box it never gets", kind)
		}
	}
}

// A suspension with nowhere to go leaves a job running in a box nobody is
// coming back for. Whichever of the three ways it happens — the runner never
// recorded the conversation, it would not serialize, or the row was no longer
// launching — the run has to be marked unresumable while the seat's owner is
// still this process, so recovery reaps the box instead of stranding it.
func TestAnUnrecordableSuspensionFailsTheRun(t *testing.T) {
	store := sandbox.NewCoordStore(memory.NewFleet())
	if err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", Role: "SWE",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	e := &Engine{sandboxPending: store}

	e.failSuspension(t.Context(), "t1", "sandbox_suspension_missing",
		"the turn suspended but recorded no conversation", nil)

	got, found, err := store.Get(t.Context(), "t1")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if got.Status != sandbox.StatusFailed {
		t.Fatalf("status = %q, want %q — a launching row holds a box nothing polls",
			got.Status, sandbox.StatusFailed)
	}
}

// The waiter duty must survive three of its OWN ticks and must never outlive
// the lease bucket it is written into.
//
// It was `3 * sandbox.DefaultPollInterval` — a constant that ignored the
// configured interval, so its own "three poll intervals" was true of exactly
// one deployment. That constant was 45s, which is also the default lease TTL,
// and the KV refuses a lease STRICTLY longer than its bucket's age: the two
// agreed by one comparison. Lower coordination.lease_ttl_seconds below 45 and
// every claim errors — and mayTick fails closed, so the waiter stops ticking
// altogether and every detached run hangs forever.
func TestTheWaiterDutyTTLFollowsItsCadenceAndItsBucket(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		lease    time.Duration
		want     time.Duration
	}{
		{"the default cadence", sandbox.DefaultPollInterval, 45 * time.Second, 45 * time.Second},
		{"a slower cadence scales with it", 60 * time.Second, 10 * time.Minute, 3 * time.Minute},
		{"a fast cadence takes the floor", 100 * time.Millisecond, 45 * time.Second, 30 * time.Second},
		{"an unset cadence takes the default", 0, 45 * time.Second, 45 * time.Second},
		{"a short bucket is the ceiling", sandbox.DefaultPollInterval, 20 * time.Second, 20 * time.Second},
		{"no bucket leaves the derived value", 60 * time.Second, 0, 3 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{leaseTTL: tc.lease}
			if got := e.waiterDutyTTL(tc.interval); got != tc.want {
				t.Fatalf("waiterDutyTTL(%s) with a %s bucket = %s, want %s",
					tc.interval, tc.lease, got, tc.want)
			}
			if tc.lease > 0 && e.waiterDutyTTL(tc.interval) > tc.lease {
				t.Fatal("the duty asks to outlive its bucket; the KV refuses that on every claim")
			}
		})
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
