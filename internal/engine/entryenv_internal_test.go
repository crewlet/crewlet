package engine

import (
	"maps"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// gatewayEnv is a claude-code entry's env that routes every call through a
// company gateway: its address, the bearer token it takes, and a region a
// cloud sign-in behind it reads.
func gatewayEnv() map[string]string {
	return map[string]string{
		"ANTHROPIC_BASE_URL":   "https://llm-gateway.example.com",
		"ANTHROPIC_AUTH_TOKEN": "gateway-bearer",
		"AWS_REGION":           "eu-west-1",
	}
}

// launchedEnv launches c's seat's executor and answers the run environment its
// job was started with, and the cell its run was placed in.
func launchedEnv(t *testing.T, c *Company, seat *org.Role) (map[string]string, sandbox.Placement) {
	t.Helper()
	run, placement := launchedRequest(t, c, seat)
	return run.Env, placement
}

// launchedRequest launches c's seat's executor and answers the request its runner
// was started with and the cell the launch picked.
func launchedRequest(t *testing.T, c *Company, seat *org.Role) (sandbox.RunRequest, sandbox.Placement) {
	t.Helper()
	e, runners := launchRigRunners(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	// EVERY HARNESS'S RUNNER, so a seat on any harness is read off the job its
	// own runner started, and a launch on two is caught.
	var started []sandbox.RunRequest
	for _, runner := range runners {
		started = append(started, runner.Started()...)
	}
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want the executor's one", len(started))
	}
	row, _, err := e.sandbox.Load().pending.Get(t.Context(), "t1")
	if err != nil {
		t.Fatalf("reading the run's row: %v", err)
	}
	return started[0], sandbox.Placement(row.Placement)
}

// AN EXECUTOR'S BOX IS HANDED ITS ENTRY'S WHOLE ENV, IN EVERY CELL.
//
// An entry's env is extra environment for every invocation of its harness,
// the executor's run included: an entry reaching its model through a company
// gateway needs the gateway's address and bearer token in the box, and that
// token is its Claude Code sign-in.
//
// Mutation: hand the box the entry's credential names alone, and the gateway's
// address goes missing in every cell.
func TestAnExecutorsBoxIsHandedItsEntrysEnvInEveryCell(t *testing.T) {
	t.Parallel()
	for _, cell := range config.Placements {
		t.Run(string(cell), func(t *testing.T) {
			t.Parallel()
			c, seat := chainCompany(t, chainMember{key: "gw", agent: "claude-code", model: "sonnet",
				runIn: cell, subscription: true, env: gatewayEnv()})
			env, placed := launchedEnv(t, c, seat)
			if placed != sandbox.Placement(cell) {
				t.Fatalf("the run was placed in %q, want %q", placed, cell)
			}
			for name, value := range gatewayEnv() {
				if env[name] != value {
					t.Errorf("%s = %q in the %s box, want the entry's %q", name, env[name], cell, value)
				}
			}
		})
	}
}

// THE SEAT'S OWN VARIABLE WINS ITS NAME, and only its name.
//
// The entry's env lies beneath the seat's role.sandbox.env: a seat naming
// another gateway or region means it, and the rest of the entry's env is
// still its runs'.
//
// Mutation: lay the entry's env over the seat's, and the seat's address is
// replaced by the entry's.
func TestASeatsOwnVariableWinsOverItsEntrysByName(t *testing.T) {
	t.Parallel()
	c, seat := chainCompany(t, chainMember{key: "gw", agent: "claude-code", model: "sonnet",
		runIn: config.PlacementContainer, subscription: true, env: gatewayEnv()})
	seat.Sandbox = &org.RoleSandbox{Env: map[string]string{
		"ANTHROPIC_BASE_URL": "https://team-gateway.example.com",
	}}
	env, _ := launchedEnv(t, c, seat)
	if got := env["ANTHROPIC_BASE_URL"]; got != "https://team-gateway.example.com" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the seat's own", got)
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "gateway-bearer" || env["AWS_REGION"] != "eu-west-1" {
		t.Errorf("the entry's other variables did not reach the box: %v", env)
	}
}

// A SEAT SIGNED IN BY ITS OWN TOKEN REPLACES THE ENTRY'S SIGN-IN WHOLE, AND
// KEEPS THE ENTRY'S ROUTE.
//
// Claude Code ranks its sign-ins by name (a cloud toggle above a key, a key
// above a plan token), so any part of the entry's sign-in beside a seat's own
// token would move the seat onto the company's account: the metered key, the
// gateway's bearer token, the cloud toggle and the cloud's key all stay out.
// The base URL and region still lie beneath: they say where requests go.
//
// Mutation: drop the entry's whole env for such a seat, and the gateway's
// address goes missing; drop nothing, and the company's key rides along.
func TestASeatsOwnTokenReplacesTheEntrysSignInAndKeepsItsRoute(t *testing.T) {
	t.Parallel()
	env := gatewayEnv()
	env["CLAUDE_CODE_USE_BEDROCK"] = "1"
	env["AWS_BEARER_TOKEN_BEDROCK"] = "bedrock-company-key"
	c, seat := chainCompany(t, chainMember{key: "api", agent: "claude-code", model: "sonnet",
		runIn: config.PlacementContainer, env: env})
	seat.Sandbox = &org.RoleSandbox{Env: map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-the-seats-plan",
	}}
	got, _ := launchedEnv(t, c, seat)
	if got["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-the-seats-plan" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q, want the seat's own", got["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "AWS_BEARER_TOKEN_BEDROCK",
	} {
		if value, present := got[name]; present {
			t.Errorf("the entry's %s = %q rode in beside the seat's own sign-in", name, value)
		}
	}
	for _, name := range []string{"ANTHROPIC_BASE_URL", "AWS_REGION"} {
		if got[name] != env[name] {
			t.Errorf("%s = %q, want the entry's %q beneath the seat's sign-in", name, got[name], env[name])
		}
	}
}

// THE PROFILE'S OWN TOKEN VARIABLE IS PART OF THE SIGN-IN, WHATEVER IT IS
// CALLED. An override names the variable a build of the harness reads its token
// from, and a name of no credential's shape is still the entry's sign-in: a
// seat that signs in itself must not be handed it beside its own.
//
// Mutation: judge the sign-in by Claude Code's names and the credential shape
// alone, and the renamed token rides in beside the seat's key.
func TestAnOverriddenTokenVariableIsPartOfTheEntrysSignIn(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompanyOverrides(t, "claude-code",
		harness.Auth{Mode: harness.AuthSubscription, Token: "sk-ant-oat-entry"},
		map[string]any{"token_env": "CLAUDE_SESSION_STRING"})
	if env := runOn(t, c, seat, nil).env; env["CLAUDE_SESSION_STRING"] != "sk-ant-oat-entry" {
		t.Fatalf("the entry's token under its renamed variable did not reach the box: %v", env)
	}
	env := runOn(t, c, seat, map[string]string{"ANTHROPIC_API_KEY": "sk-ant-the-seats"}).env
	if value, present := env["CLAUDE_SESSION_STRING"]; present {
		t.Errorf("the entry's token %q rode in beside the seat's own key", value)
	}
}

// THE NAMES THAT SIGN A harness IN ARE ITS PROFILE'S LIST, read where a box's
// sign-in is judged: whether a seat already signs the harness in, so the
// entry's own sign-in stays out ([entryEnv]). The list is the one
// `auth.mode: env` reads, so a switch an override adds to it counts, and one
// it drops does not.
//
// Mutation: judge by a fixed list of Claude Code's names, and the added switch
// does not keep the entry's key out, while the dropped Bedrock toggle still
// does.
func TestTheNamesThatSignAHarnessInAreItsProfilesList(t *testing.T) {
	t.Parallel()
	overrides := map[string]any{"sign_in_env": []any{"CLAUDE_CODE_USE_MYCLOUD"}}
	c, seat := creditedCompanyOverrides(t, "claude-code",
		harness.Auth{Mode: harness.AuthAPIKey, APIKey: "sk-ant-entry"}, overrides)

	env := runOn(t, c, seat, map[string]string{"CLAUDE_CODE_USE_MYCLOUD": "1"}).env
	if value, present := env["ANTHROPIC_API_KEY"]; present {
		t.Errorf("the entry's key %q rode in beside the seat's own sign-in", value)
	}
	env = runOn(t, c, seat, map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}).env
	if env["ANTHROPIC_API_KEY"] != "sk-ant-entry" {
		t.Errorf("a name the profile no longer lists kept the entry's key out: %v", env)
	}
}

// creditedCompanyOverrides is a one-seat company whose executor is agent's
// harness signed in under auth with the entry's profile overrides: a real
// provider, since the question is which credentials its profile puts inside a
// box, which a fake cannot answer.
func creditedCompanyOverrides(t *testing.T, agent string, auth harness.Auth,
	overrides map[string]any) (*Company, *org.Role) {
	t.Helper()
	provider, err := harness.New(harness.Config{
		Key: "sub", Harness: agent, StateDir: t.TempDir(), Timeout: time.Minute, Auth: auth,
		Overrides: overrides,
	})
	if err != nil {
		t.Fatalf("harness.New(%s): %v", agent, err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "sub", Provider: provider}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	seat := &org.Role{Name: "SWE", LLM: org.ProviderKeys{"sub"}}
	return &Company{Models: models, Config: &config.Company{}}, seat
}

// A HARNESS THAT SIGNS IN THROUGH env TAKES THAT SIGN-IN INTO ITS BOX:
// OpenCode reads its provider's key from its own environment, so an entry's
// whole sign-in can be one variable in env, which the doctor counts and the
// run environment has to carry, with the rest of env beside it.
//
// Mutation: hand the box the profile's credential names alone, and the key
// or the proxy goes missing.
func TestAHarnessEnvSignInReachesTheRun(t *testing.T) {
	t.Parallel()
	env := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-not-real", "HTTPS_PROXY": "http://proxy.example.com"}
	provider, err := harness.New(harness.Config{
		Key: "sub", Harness: "opencode", Model: "anthropic/claude-sonnet-4",
		StateDir: t.TempDir(), Timeout: time.Minute, Env: env,
	})
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "sub", Provider: provider}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	cfg := &config.Company{Providers: config.Providers{LLM: map[string]config.LLMProvider{
		"sub": {Harness: config.HarnessOpenCode, Model: "anthropic/claude-sonnet-4", Env: env},
	}}}
	run := runOn(t, &Company{Models: models, Config: cfg}, &org.Role{Name: "SWE", LLM: org.ProviderKeys{"sub"}}, nil)
	if run.env["ANTHROPIC_API_KEY"] != "sk-ant-not-real" {
		t.Errorf("the run environment does not carry the env sign-in: %v", run.env)
	}
	if run.env["HTTPS_PROXY"] != "http://proxy.example.com" {
		t.Errorf("the entry's proxy did not reach the run environment: %v", run.env)
	}
}

// AN INTEGRATION'S BOX DEFAULTS LIE BENEATH THE ENTRY AND NEVER TOUCH ITS
// SIGN-IN: the entry's own env wins a name, its sign-in is neither stripped nor
// replaced, a default under one of the harness's sign-in names never reaches
// the box, and the rest do. A variable naming a box file is kept apart for the
// launch, under the same rule, and not in the env.
// Mutation: lay the defaults with the seat's env, and the entry's region is
// overridden and Claude Code is switched to Vertex.
func TestBoxDefaultsLieBeneathTheEntryAndNeverTouchItsSignIn(t *testing.T) {
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
      env: {CLOUD_REGION: eu-west-1}
roles:
  - name: Engineer
    handle: eng
    llm: claude
`)
	seat := seatNamed(t, c, "Engineer")
	member, err := c.Models.Head(seat, phase.Execute)
	if err != nil {
		t.Fatalf("executor chain: %v", err)
	}
	run := newExecutorRun(member.Provider.(*harness.Provider), runEnv{env: map[string]string{}, defaults: map[string]string{
		"CLOUD_REGION": "fr-par", "CLAUDE_CODE_USE_VERTEX": "1",
		"ANTHROPIC_API_KEY": "sk-ant-integration", "CLI_ZONE": "fr-par-1",
	}, homePaths: map[string]string{"CLI_TOKEN_FILE": ".config/crewlet/token", "CLAUDE_CODE_USE_VERTEX": ".config/crewlet/x"}})
	if want := map[string]string{"CLI_TOKEN_FILE": ".config/crewlet/token"}; !maps.Equal(run.homePaths, want) {
		t.Errorf("the box file variables = %v, want %v: none under a sign-in name", run.homePaths, want)
	}
	if value, present := run.env["CLI_TOKEN_FILE"]; present {
		t.Errorf("a box file's variable was laid before the box is known, as %q", value)
	}
	for name, want := range map[string]string{
		"CLOUD_REGION": "eu-west-1", "ANTHROPIC_API_KEY": "sk-ant-company", "CLI_ZONE": "fr-par-1",
	} {
		if got := run.env[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if value, present := run.env["CLAUDE_CODE_USE_VERTEX"]; present {
		t.Errorf("a default under a sign-in name reached the box as %q", value)
	}
}
