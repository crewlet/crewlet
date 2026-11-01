package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// creditedCompany is a one-seat company whose executor is the named
// subscription CLI.
//
// A REAL cliagent provider rather than a stand-in, because the whole question
// is which credentials that provider's profile can actually put inside a box —
// a fake would answer whatever the test wanted and prove nothing about the CLI.
func creditedCompany(t *testing.T, agent string) (*Company, *org.Role) {
	t.Helper()
	return creditedCompanyAuth(t, agent, cliagent.Auth{})
}

// creditedCompanyAuth is [creditedCompany] signed in under auth.
func creditedCompanyAuth(t *testing.T, agent string, auth cliagent.Auth) (*Company, *org.Role) {
	t.Helper()
	return creditedCompanyOverrides(t, agent, auth, nil)
}

// creditedCompanyOverrides is [creditedCompanyAuth] with the entry's profile
// overrides.
func creditedCompanyOverrides(t *testing.T, agent string, auth cliagent.Auth,
	overrides map[string]any) (*Company, *org.Role) {
	t.Helper()
	provider, err := cliagent.New(cliagent.Config{
		Key: "sub", Agent: agent, StateDir: t.TempDir(), Timeout: time.Minute, Auth: auth,
		Overrides: overrides,
	})
	if err != nil {
		t.Fatalf("cliagent.New(%s): %v", agent, err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "sub", Provider: provider}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	seat := &org.Role{Name: "SWE", LLM: org.ProviderKeys{"sub"}}
	return &Company{Models: models, Config: &config.Company{}}, seat
}

// guardOn is the credential guard as a launch asks it of seat's executor: on
// the member of its chain the launch runs on — the head, in every company
// here — over the run environment env.
func guardOn(t *testing.T, c *Company, seat *org.Role, placement sandbox.Placement,
	env map[string]string) error {
	t.Helper()
	member, err := c.Models.Head(seat, phase.Execute)
	if err != nil {
		t.Fatalf("%s's executor chain: %v", seat.Name, err)
	}
	return sandboxCredentials(member.Provider.(*cliagent.Provider), member.Key, seat.Handle(),
		placement, env)
}

// A SUBSCRIPTION LOGIN CANNOT FOLLOW A RUN INTO A REMOTE BOX.
//
// The credential files stay on the engine host — they carry a refresh token
// whose rotation is fleet state — so a CLI that mints no headless token
// (OpenCode, whose login is a database it keeps) has nothing inside a remote
// box to authenticate with unless its provider's key travels. Left unchecked the run
// provisions a box, installs the agent, applies every setup step and starts
// the job, and the agent fails at its first model call with the vendor's own
// "not authenticated", minutes in, naming nothing an operator could act on.
func TestARemoteRunWithNoTravellingCredentialIsRefused(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompany(t, "opencode")
	err := guardOn(t, c, seat, sandbox.E2B, nil)
	if err == nil {
		t.Fatal("a remote run with no credential that travels was launched")
	}
	var credErr *SandboxCredentialError
	if !errors.As(err, &credErr) {
		t.Fatalf("error is %T, want *SandboxCredentialError — the remedy is a "+
			"config edit, and retrying it as a provider outage would loop", err)
	}
	// AND CLASSED AS ONE, which is what whoever is told the run did not
	// start reads: a person's edit clears it, never a wait.
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
		t.Errorf("the refusal is classed %q (%v), want %q", why, ok, sandbox.WhyConfiguration)
	}
	// The remedy has to be THIS operator's: OpenCode mints no token, so
	// telling them to capture one would send them at a flag that does not
	// exist.
	if strings.Contains(err.Error(), "capture-token") {
		t.Errorf("the error offers a token opencode cannot mint: %v", err)
	}
	if !strings.Contains(err.Error(), string(sandbox.Direct)) {
		t.Errorf("the error does not name the local cell that would work: %v", err)
	}
}

// THE OTHER CASE GETS THE OTHER REMEDY. Claude Code mints a headless token
// that travels to any box, so the operator's fix is one command rather than a
// placement change — and offering only the placement change would move a seat
// onto the engine host for no reason.
func TestTheRemedyNamesTheTokenWhereTheCLIMintsOne(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompany(t, "claude-code")
	err := guardOn(t, c, seat, sandbox.E2B, nil)
	if err == nil {
		t.Fatal("a remote run with no resolved token was launched")
	}
	if !strings.Contains(err.Error(), "-capture-token") {
		t.Errorf("the error does not name the token claude-code can mint: %v", err)
	}
}

// THE REMEDY FOLLOWS THE ENTRY'S AUTH MODE, not only its CLI. Claude Code
// mints a headless token, but an api-key entry never reads one — the mode
// gives the token no fallback and removes its variable from the child — and an
// inherit-env entry reads only the engine's own environment, never the secret
// store `-capture-token` writes into. Told to mint one, the operator of either
// stored a token nothing reads, and the next launch was refused with the same
// advice. Each mode's refusal has to name what that mode reads.
//
// Not parallel: inherit-env reads the engine's process environment, which the
// test empties of both variables so that a key exported on the machine running
// the suite cannot sign the entry in.
//
// Mutation: choose the remedy by the profile alone, and the api-key and
// inherit-env cases offer -capture-token and go red.
func TestTheRemedyFollowsTheEntrysAuthMode(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	for _, tc := range []struct {
		mode    cliagent.AuthMode
		want    []string
		notWant []string
	}{
		{
			mode:    cliagent.AuthAPIKey,
			want:    []string{"api-key", "providers.llm.sub.auth.api_key", "ANTHROPIC_API_KEY", "crewlet secrets set", "role.sandbox.env"},
			notWant: []string{"capture-token", "subscription login", "CLAUDE_CODE_OAUTH_TOKEN"},
		},
		{
			mode:    cliagent.AuthInheritEnv,
			want:    []string{"inherit-env", "CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY", "engine's", "never from the secret store", "role.sandbox.env", string(sandbox.Direct)},
			notWant: []string{"capture-token", "subscription login", "crewlet secrets set"},
		},
		{
			// The env is the whole sign-in and seeds no login, so a local
			// cell is no remedy and a token is never asked for.
			mode:    cliagent.AuthEnv,
			want:    []string{"env", "CLAUDE_CODE_USE_BEDROCK", "providers.llm.sub.env", "role.sandbox.env"},
			notWant: []string{"capture-token", "local cell", "seeded with its login", "crewlet secrets set"},
		},
		{
			mode:    cliagent.AuthSubscription,
			want:    []string{"subscription login", "crewlet llm login sub -capture-token", string(sandbox.Direct)},
			notWant: []string{"auth.api_key", "inherit-env"},
		},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			c, seat := creditedCompanyAuth(t, "claude-code", cliagent.Auth{Mode: tc.mode})
			err := guardOn(t, c, seat, sandbox.E2B, nil)
			var credErr *SandboxCredentialError
			if !errors.As(err, &credErr) {
				t.Fatalf("a remote run nothing signs in answered %v, want a *SandboxCredentialError", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("the refusal names %q, which this mode never reads: %v", bad, err)
				}
			}
		})
	}
}

// AND WHAT EACH MODE'S REMEDY NAMES SIGNS THE RUN IN. An api-key entry whose
// key the seat brings in role.sandbox.env passes, and so does an inherit-env
// entry once the engine's environment holds a variable it forwards — the
// refusal's advice has to be advice that works. Judged on the environment the
// launch judges, the one [newCodingRun] lays: the seat's own, with the entry's
// sign-in underneath.
func TestEachModesRemedySignsTheRunIn(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	launch := func(c *Company, seat *org.Role, seatEnv map[string]string) error {
		t.Helper()
		return guardOn(t, c, seat, sandbox.E2B, runOn(t, c, seat, seatEnv).env)
	}

	key, keySeat := creditedCompanyAuth(t, "claude-code", cliagent.Auth{Mode: cliagent.AuthAPIKey})
	if err := launch(key, keySeat, map[string]string{"ANTHROPIC_API_KEY": "sk-ant-not-a-real-key"}); err != nil {
		t.Errorf("an api-key entry whose seat brings the key was refused: %v", err)
	}
	if err := launch(key, keySeat, nil); err == nil {
		t.Error("an api-key entry with no key anywhere was launched")
	}
	stored, storedSeat := creditedCompanyAuth(t, "claude-code",
		cliagent.Auth{Mode: cliagent.AuthAPIKey, APIKey: "sk-ant-not-a-real-key"})
	if err := launch(stored, storedSeat, nil); err != nil {
		t.Errorf("an api-key entry whose key resolved was refused: %v", err)
	}

	inherit, inheritSeat := creditedCompanyAuth(t, "claude-code", cliagent.Auth{Mode: cliagent.AuthInheritEnv})
	if err := launch(inherit, inheritSeat, nil); err == nil {
		t.Error("an inherit-env entry with nothing in the engine's environment was launched")
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-not-a-real-key")
	if err := launch(inherit, inheritSeat, nil); err != nil {
		t.Errorf("an inherit-env entry whose engine environment holds the key was refused: %v", err)
	}
}

// A MODE WHOSE PROFILE NAMES NO VARIABLE FOR IT HANDS THE CLI NOTHING —
// api-key on OpenCode, which reads each provider's key from that provider's own
// variable and so names no api_key_env. Every write of the configuration
// refuses the pairing, but an apply only warns, so a node can run it. Its
// refusal names the mode as the remedy, and the local cell the login it then
// signs in with needs; never a variable to set, since none would reach the CLI.
func TestARemedyNamesNoVariableTheModeDoesNotHave(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompanyAuth(t, "opencode", cliagent.Auth{Mode: cliagent.AuthAPIKey})
	err := guardOn(t, c, seat, sandbox.E2B, nil)
	var credErr *SandboxCredentialError
	if !errors.As(err, &credErr) {
		t.Fatalf("a remote run nothing signs in answered %v, want a *SandboxCredentialError", err)
	}
	for _, want := range []string{"no api_key_env", "set auth.mode: subscription", string(sandbox.Direct)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	for _, bad := range []string{"capture-token", "role.sandbox.env", "crewlet secrets set"} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("the refusal names %q, which reaches nothing under this mode: %v", bad, err)
		}
	}
}

// A REMEDY OFFERS NO VARIABLE NO STORE CAN HOLD. overrides can set token_env or
// api_key_env to a name outside the reference grammar — MY-TOKEN, MY=KEY —
// which every write of the configuration refuses and an apply only warns
// about, so a node can run it. The refusal told such a subscription entry to
// "mint a token that travels with `crewlet llm login sub -capture-token`",
// which that command refuses before it mints, and an api-key one to store or
// export a key, or give the seat its own in role.sandbox.env, under a name no
// store, ${VAR} or export can hold and every write refuses. Each now names the
// overrides field, and an inherit-env entry is told only the variable a shell
// can export. A route that runs whatever token_env says stays: OpenCode reads
// its provider's key from its environment, so it is still told to set that
// key, with the field beside it.
//
// Not parallel: inherit-env reads the engine's process environment, emptied
// here of the variable it can forward.
//
// An api-key entry is offered no local cell either, as no api-key refusal
// offers an entry that keeps its key: the login seeded there would bill in
// place of the key the mode names.
//
// Mutation: let MintsHeadlessToken ignore the grammar, and the subscription
// case offers -capture-token; drop the api-key misnamed case here, and that
// refusal falls through to inherit-env's local cell; judge the misnamed token
// before the provider's key, and the OpenCode case loses its key.
func TestARemedyOffersNoVariableNoStoreCanHold(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	for _, tc := range []struct {
		name          string
		agent         string
		mode          cliagent.AuthMode
		overrides     map[string]any
		want, notWant []string
	}{
		{
			name: "subscription", mode: cliagent.AuthSubscription,
			overrides: map[string]any{"token_env": "MY-TOKEN"},
			want: []string{`providers.llm.sub.overrides.token_env is "MY-TOKEN"`,
				"Name the variable your build of the CLI reads", string(sandbox.Direct)},
			notWant: []string{"crewlet llm login sub -capture-token"},
		},
		{
			name: "subscription on a CLI that reads its provider's key", agent: "opencode",
			mode: cliagent.AuthSubscription, overrides: map[string]any{"token_env": "MY-TOKEN"},
			want: []string{"Set the key of", "providers.llm.sub.env",
				`providers.llm.sub.overrides.token_env is "MY-TOKEN"`, string(sandbox.Direct)},
			notWant: []string{"crewlet llm login sub -capture-token"},
		},
		{
			name: "api-key", mode: cliagent.AuthAPIKey,
			overrides: map[string]any{"api_key_env": "MY=KEY"},
			want: []string{"providers.llm.sub.overrides.api_key_env",
				"name the variable your build of the CLI reads"},
			notWant: []string{"role.sandbox.env", "crewlet secrets set", "capture-token", "local cell"},
		},
		{
			name: "inherit-env with one variable a shell can export", mode: cliagent.AuthInheritEnv,
			overrides: map[string]any{"token_env": "MY-TOKEN"},
			want: []string{"export ANTHROPIC_API_KEY in the engine's environment",
				"set ANTHROPIC_API_KEY in this seat's role.sandbox.env", string(sandbox.Direct)},
			notWant: []string{"MY-TOKEN or ANTHROPIC_API_KEY in this seat's", "capture-token"},
		},
		{
			name: "inherit-env with none", mode: cliagent.AuthInheritEnv,
			overrides: map[string]any{"token_env": "MY-TOKEN", "api_key_env": "MY-KEY"},
			want: []string{"providers.llm.sub.overrides.token_env and providers.llm.sub.overrides.api_key_env",
				"seeded with its login", string(sandbox.Direct)},
			notWant: []string{"role.sandbox.env", "export one in", "capture-token"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := tc.agent
			if agent == "" {
				agent = "claude-code"
			}
			c, seat := creditedCompanyOverrides(t, agent, cliagent.Auth{Mode: tc.mode}, tc.overrides)
			err := guardOn(t, c, seat, sandbox.E2B, nil)
			var credErr *SandboxCredentialError
			if !errors.As(err, &credErr) {
				t.Fatalf("a remote run nothing signs in answered %v, want a *SandboxCredentialError", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("the refusal names %q, a route this entry cannot take: %v", bad, err)
				}
			}
		})
	}
}

// A CREDENTIAL THE OPERATOR DECLARED THEMSELVES COUNTS, which is what lets
// this be a refusal rather than a warning: the engine names no tool-specific
// variable of its own, so it asks the profile which names authenticate and
// looks for those in the merged run environment — where role.sandbox.env
// already sits.
func TestASeatThatBroughtItsOwnKeyIsNotRefused(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompany(t, "claude-code")
	env := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-not-a-real-token"}
	if err := guardOn(t, c, seat, sandbox.E2B, env); err != nil {
		t.Fatalf("a seat carrying its own key was refused: %v", err)
	}
	// Blank is not a value: an unresolved ${VAR} lands here as empty, and
	// reading that as "authenticated" is the whole failure again.
	if err := guardOn(t, c, seat, sandbox.E2B, map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "  ",
	}); err == nil {
		t.Error("an empty credential variable was read as authentication")
	}
}

// A SEAT SIGNED IN TO CLAUDE CODE BY ITS OWN CLOUD OR BEARER TOKEN IS SIGNED
// IN. Such a seat is handed none of the entry's sign-in — Claude Code ranks a
// cloud toggle and a bearer token above the entry's token, so the entry's
// would only ride along unread — and a guard that knew only the profile's own
// two variables then refused its remote run as signed in to nothing.
//
// Mutation: drop the profile's sign_in_env from the guard, and every case here
// is refused.
func TestASeatsOwnClaudeCodeSignInPassesTheRemoteGuard(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompany(t, "claude-code")
	for _, own := range []map[string]string{
		{"CLAUDE_CODE_USE_BEDROCK": "1"},
		{"CLAUDE_CODE_USE_VERTEX": "1"},
		{"CLAUDE_CODE_USE_FOUNDRY": "1"},
		{"ANTHROPIC_AUTH_TOKEN": "bearer-from-the-seat"},
	} {
		if err := guardOn(t, c, seat, sandbox.E2B, own); err != nil {
			t.Errorf("%v: a seat signed in by its own environment was refused: %v", own, err)
		}
	}
	// And only Claude Code's: an OpenCode run is not signed in by a
	// Bedrock toggle it never reads.
	oc, ocSeat := creditedCompany(t, "opencode")
	if err := guardOn(t, oc, ocSeat, sandbox.E2B,
		map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}); err == nil {
		t.Error("an opencode run was let through on Claude Code's cloud toggle")
	}
}

// A LOCAL CELL SEEDS THE FILES AND WRITES A REFRESHED ONE BACK, so files alone
// are a complete answer there — which is the entire reason the check turns on
// the placement rather than on the provider.
func TestALocalRunNeedsNoTravellingCredential(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompany(t, "opencode")
	for _, placement := range []sandbox.Placement{sandbox.Direct, sandbox.Container} {
		if err := guardOn(t, c, seat, placement, nil); err != nil {
			t.Errorf("run_in %q was refused: %v", placement, err)
		}
	}
}

// A CLI THAT SIGNS IN THROUGH env TAKES THAT SIGN-IN INTO ITS BOX.
//
// OpenCode reads its provider's key from its own environment, so an entry's
// whole sign-in can be one variable in env — which the doctor counts. The
// run environment carried only the token and the api-key variable, so an
// executor run on such an entry started its CLI signed in to nothing, and a
// remote one was refused as having no credential at all.
func TestACLIEnvSignInTravelsIntoTheRun(t *testing.T) {
	t.Parallel()
	company := func(env map[string]string) (*Company, *org.Role) {
		provider, err := cliagent.New(cliagent.Config{
			Key: "sub", Agent: "opencode", Model: "anthropic/claude-sonnet-4",
			StateDir: t.TempDir(), Timeout: time.Minute, Env: env,
		})
		if err != nil {
			t.Fatalf("cliagent.New: %v", err)
		}
		models, err := phase.NewRegistry([]phase.Entry{{Key: "sub", Provider: provider}})
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		cfg := &config.Company{Providers: config.Providers{LLM: map[string]config.LLMProvider{
			"sub": {Harness: config.HarnessOpenCode, Model: "anthropic/claude-sonnet-4", Env: env},
		}}}
		return &Company{Models: models, Config: cfg},
			&org.Role{Name: "SWE", LLM: org.ProviderKeys{"sub"}}
	}

	c, seat := company(map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-not-real", "HTTPS_PROXY": "http://proxy.example.com",
	})
	env := runOn(t, c, seat, nil).env
	if env["ANTHROPIC_API_KEY"] != "sk-ant-not-real" {
		t.Fatalf("the run environment does not carry the env sign-in: %v", env)
	}
	if err := guardOn(t, c, seat, sandbox.E2B, env); err != nil {
		t.Errorf("a remote run carrying its env key was refused: %v", err)
	}
	// THE REST OF env TRAVELS BESIDE IT, and is no sign-in: an entry whose
	// env holds a proxy alone reaches the box with it, and is refused a
	// remote one as signed in to nothing.
	if env["HTTPS_PROXY"] != "http://proxy.example.com" {
		t.Errorf("the entry's proxy did not reach the run environment: %v", env)
	}
	c, seat = company(map[string]string{"HTTPS_PROXY": "http://proxy.example.com"})
	if err := guardOn(t, c, seat, sandbox.E2B, runOn(t, c, seat, nil).env); err == nil {
		t.Error("a remote run whose entry's env holds a proxy alone was launched as signed in")
	}

	// THE KEY IN THE SEAT'S OWN SANDBOX ENV passes too: which variable this
	// CLI reads depends on the provider its model names, so the guard
	// cannot ask the profile, and an operator legitimately declares their
	// own key in role.sandbox.env.
	c, seat = company(nil)
	if err := guardOn(t, c, seat, sandbox.E2B,
		map[string]string{"OPENROUTER_API_KEY": "sk-or-not-real"}); err != nil {
		t.Errorf("a remote run carrying its key in role.sandbox.env was refused: %v", err)
	}
	// A count of model tokens is not a key.
	if err := guardOn(t, c, seat, sandbox.E2B,
		map[string]string{"OPENCODE_MAX_TOKENS": "4096"}); err == nil {
		t.Error("a remote run whose only variable is a token count was launched")
	}

	err := guardOn(t, c, seat, sandbox.E2B, nil)
	if err == nil {
		t.Fatal("a remote run with no sign-in was launched")
	}
	for _, route := range []string{"providers.llm.sub.env", "role.sandbox.env"} {
		if !strings.Contains(err.Error(), route) {
			t.Errorf("the refusal does not name the %s route this CLI takes: %v", route, err)
		}
	}
	// And WHOSE key: the provider segment of the model the box is handed is
	// the one fact that says which provider has to sign the run in.
	if !strings.Contains(err.Error(), `"anthropic", the provider its model names`) {
		t.Errorf("the refusal does not name the provider whose key is missing: %v", err)
	}
}
