package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
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
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	started := jobs.Started()
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want the executor's one", len(started))
	}
	row, _, err := e.sandbox.Load().pending.Get(t.Context(), "t1")
	if err != nil {
		t.Fatalf("reading the run's row: %v", err)
	}
	return started[0].Env, sandbox.Placement(row.Placement)
}

// AN EXECUTOR'S BOX IS HANDED ITS ENTRY'S WHOLE ENV, IN EVERY CELL.
//
// An entry's env is extra environment for every invocation of its CLI, and
// the executor's run is one. Built from the entry's sign-in alone, a run on an
// entry that reaches its model through a company gateway called the vendor's
// own endpoint with nothing it accepts — the gateway's bearer token is no
// Anthropic credential — and a remote one was refused before its box existed
// as signed in to nothing, while the same entry's reviewer worked. The
// gateway's bearer token is a Claude Code sign-in, so it is what lets the
// remote run start.
//
// Mutation: hand the box the entry's credential names alone, and the gateway's
// address goes missing in both cells and the e2b launch is refused.
func TestAnExecutorsBoxIsHandedItsEntrysEnvInEveryCell(t *testing.T) {
	t.Parallel()
	for _, cell := range []config.Placement{config.PlacementDirect, config.PlacementE2B} {
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
// The entry's env lies beneath what the seat's config assembled: a seat that
// points its runs at another gateway, or another region, names that variable
// in role.sandbox.env and means it, and the rest of the entry's env is still
// its runs'.
//
// Mutation: lay the entry's env over the seat's, and the seat's address is
// replaced by the entry's.
func TestASeatsOwnVariableWinsOverItsEntrysByName(t *testing.T) {
	t.Parallel()
	c, seat := chainCompany(t, chainMember{key: "gw", agent: "claude-code", model: "sonnet",
		runIn: config.PlacementE2B, subscription: true, env: gatewayEnv()})
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
// Claude Code ranks its sign-ins by name — a cloud toggle above a key, a key
// above a plan token — so the entry's sign-in beside a seat's own plan token
// moved the seat onto the company's account; every part of it stays out: the
// metered key, the gateway's bearer token, the cloud toggle and the cloud's
// own key, which with the toggle gone no request reads. What is not a sign-in
// still lies beneath the seat's: the base URL and the region say where the
// run's requests go, not whom they are made as.
//
// Mutation: drop the entry's whole env for such a seat, as its sign-in once
// was, and the gateway's address goes missing; drop nothing, and the
// company's key rides beside the seat's token.
func TestASeatsOwnTokenReplacesTheEntrysSignInAndKeepsItsRoute(t *testing.T) {
	t.Parallel()
	env := gatewayEnv()
	env["CLAUDE_CODE_USE_BEDROCK"] = "1"
	env["AWS_BEARER_TOKEN_BEDROCK"] = "bedrock-company-key"
	c, seat := chainCompany(t, chainMember{key: "api", agent: "claude-code", model: "sonnet",
		runIn: config.PlacementE2B, env: env})
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
// CALLED. An override names the variable a build of the CLI reads its token
// from, and a name of no credential's shape is still the entry's sign-in: a
// seat that signs in itself must not be handed it beside its own.
//
// Mutation: judge the sign-in by Claude Code's names and the credential shape
// alone, and the renamed token rides in beside the seat's key.
func TestAnOverriddenTokenVariableIsPartOfTheEntrysSignIn(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompanyOverrides(t, "claude-code",
		cliagent.Auth{Mode: cliagent.AuthSubscription, Token: "sk-ant-oat-entry"},
		map[string]any{"token_env": "CLAUDE_SESSION_STRING"})
	if env := runOn(t, c, seat, nil).env; env["CLAUDE_SESSION_STRING"] != "sk-ant-oat-entry" {
		t.Fatalf("the entry's token under its renamed variable did not reach the box: %v", env)
	}
	env := runOn(t, c, seat, map[string]string{"ANTHROPIC_API_KEY": "sk-ant-the-seats"}).env
	if value, present := env["CLAUDE_SESSION_STRING"]; present {
		t.Errorf("the entry's token %q rode in beside the seat's own key", value)
	}
}

// THE NAMES THAT SIGN A CLI IN ARE ITS PROFILE'S LIST, read wherever a box's
// sign-in is judged: whether a seat already signs the CLI in, so the entry's
// own sign-in stays out ([entryEnv]), and whether a remote run is signed in at
// all ([entrySignsIn]). The list is the one `auth.mode: env` reads, so a
// switch an override adds to it counts in both, and one it drops counts in
// neither.
//
// Mutation: judge by a fixed list of Claude Code's names, and the added switch
// neither keeps the entry's key out nor lets the remote run start, while the
// dropped Bedrock toggle still keeps the key out.
func TestTheNamesThatSignACLIInAreItsProfilesList(t *testing.T) {
	t.Parallel()
	overrides := map[string]any{"sign_in_env": []any{"CLAUDE_CODE_USE_MYCLOUD"}}
	c, seat := creditedCompanyOverrides(t, "claude-code",
		cliagent.Auth{Mode: cliagent.AuthAPIKey, APIKey: "sk-ant-entry"}, overrides)

	env := runOn(t, c, seat, map[string]string{"CLAUDE_CODE_USE_MYCLOUD": "1"}).env
	if value, present := env["ANTHROPIC_API_KEY"]; present {
		t.Errorf("the entry's key %q rode in beside the seat's own sign-in", value)
	}
	env = runOn(t, c, seat, map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}).env
	if env["ANTHROPIC_API_KEY"] != "sk-ant-entry" {
		t.Errorf("a name the profile no longer lists kept the entry's key out: %v", env)
	}

	bare, bareSeat := creditedCompanyOverrides(t, "claude-code", cliagent.Auth{}, overrides)
	if err := guardOn(t, bare, bareSeat, sandbox.E2B,
		map[string]string{"CLAUDE_CODE_USE_MYCLOUD": "1"}); err != nil {
		t.Errorf("a remote run signed in by a name the profile lists was refused: %v", err)
	}
}
