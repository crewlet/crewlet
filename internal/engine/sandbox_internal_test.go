package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
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

// llm_sandbox falls back to llm_execute before llm, because sandboxed work IS
// this seat's Execute phase running somewhere else.
func TestTheSandboxModelFallsBackThroughExecute(t *testing.T) {
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
    llm_execute: coder
`)
	got, _, _ := sandboxLLM(c, seatNamed(t, c, "Engineer"))
	if got == nil || got.Model != "gpt-4o-coder" {
		t.Fatalf("the sandbox model = %+v, want the seat's Execute model", got)
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
