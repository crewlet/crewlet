package cliagent

import (
	"maps"
	"testing"
	"time"
)

// sandboxEnvOf is [Provider.SandboxEnv] of a claude-code entry with env,
// signed in under auth.
func sandboxEnvOf(t *testing.T, env map[string]string, auth Auth) map[string]string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "claude", Agent: "claude-code", Model: "sonnet", StateDir: dir,
		Timeout: time.Minute, Env: env, Auth: auth,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.SandboxEnv()
}

// A BOX IS HANDED ITS ENTRY'S WHOLE ENV, NOT ONLY ITS SIGN-IN.
//
// An entry's env is extra environment for every invocation of its CLI, and a
// seat's executor run is one. Cut to the credential names, an entry that
// reached its model through a gateway ran its box against the vendor with the
// gateway's key, and one on Bedrock called the Anthropic API signed in to
// nothing, while every other phase of the same entry worked. What stays on
// this host is this host's: the allowlist, the seat home's isolation and the
// profile's own tuning, none of which is the entry's.
//
// Mutation: read only the credential names, as the accessor once did, and the
// gateway's address, the cloud switch and its region go missing.
func TestABoxIsHandedItsEntrysWholeEnv(t *testing.T) {
	t.Parallel()
	got := sandboxEnvOf(t, map[string]string{
		"ANTHROPIC_BASE_URL":      "https://gateway.example.com",
		"ANTHROPIC_AUTH_TOKEN":    "gateway-bearer",
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"AWS_REGION":              "us-east-1",
		"HTTPS_PROXY":             "http://proxy.example.com:3128",
	}, Auth{Mode: AuthSubscription, Token: "sk-ant-oat-entry"})
	want := map[string]string{
		"ANTHROPIC_BASE_URL":      "https://gateway.example.com",
		"ANTHROPIC_AUTH_TOKEN":    "gateway-bearer",
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"AWS_REGION":              "us-east-1",
		"HTTPS_PROXY":             "http://proxy.example.com:3128",
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-entry",
	}
	// EXACTLY these: the profile's fixed env (CLAUDE_CODE_DISABLE_*), its
	// isolation (HOME, CLAUDE_CONFIG_DIR, the CLAUDE.md switches) and the
	// host allowlist (PATH) are this host's call's, never the box's.
	if !maps.Equal(got, want) {
		t.Errorf("the box's entry environment = %v, want exactly %v", got, want)
	}
}

// WHAT THE AUTH MODE REMOVES FROM A CALL ON THIS HOST IT REMOVES FROM THE BOX,
// and what it puts there it puts here.
//
// The whole env travels now, so a variable the mode keeps out of the entry's
// own calls must not ride into its box under the env's name: a subscription
// entry carrying a stray metered key would bill it in the box while the
// operator believed they were on the plan, and an api-key entry carrying a
// plan token would be signed in two ways at once. Config refuses both names in
// env on every write, but an apply only warns, so a node can run them.
//
// Mutation: read the entry's env as written rather than as [childEnv] leaves
// it, and both stray credentials reach the box.
func TestTheAuthModesRemovalsHoldInTheBox(t *testing.T) {
	t.Parallel()
	gateway := "https://gateway.example.com"

	plan := sandboxEnvOf(t, map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-stray", "ANTHROPIC_BASE_URL": gateway,
	}, Auth{Mode: AuthSubscription, Token: "sk-ant-oat-plan"})
	if want := (map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-plan", "ANTHROPIC_BASE_URL": gateway,
	}); !maps.Equal(plan, want) {
		t.Errorf("a subscription entry's box environment = %v, want exactly %v", plan, want)
	}

	key := sandboxEnvOf(t, map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-stray", "ANTHROPIC_BASE_URL": gateway,
	}, Auth{Mode: AuthAPIKey, APIKey: "sk-ant-entry"})
	if want := (map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-entry", "ANTHROPIC_BASE_URL": gateway,
	}); !maps.Equal(key, want) {
		t.Errorf("an api-key entry's box environment = %v, want exactly %v", key, want)
	}
}

// INHERIT-ENV FORWARDS THE ENGINE'S OWN CREDENTIAL INTO THE BOX as it does into
// a call here — the one mode in which a host credential reaches a child — and
// the rest of the engine's environment stays out of both.
func TestAnInheritEnvEntrysBoxCarriesWhatTheModeForwards(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-from-the-engine")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://not-the-entrys.example.com")
	got := sandboxEnvOf(t, nil, Auth{Mode: AuthInheritEnv})
	if want := (map[string]string{"ANTHROPIC_API_KEY": "sk-ant-from-the-engine"}); !maps.Equal(got, want) {
		t.Errorf("an inherit-env entry's box environment = %v, want exactly %v", got, want)
	}
}

// A VARIABLE THAT ISOLATES A CALL ON THIS HOST NEVER REACHES A BOX, nor does a
// value that resolved to nothing.
//
// An entry's env naming HOME, the CLI's relocation variable or one of its
// isolation switches is refused on every write and only warned about where it
// is applied, so a node can run one, and on this host it replaces the seat
// home's isolation; in a box it would point the run's HOME at the engine
// host's path, which a box sets for itself. A blank value is a reference that
// resolved to nothing, which no box is handed as an empty string.
//
// Mutation: drop the isolation filter, and HOME and CLAUDE_CONFIG_DIR reach
// the box.
func TestABoxIsNeverHandedTheIsolationOrABlankValue(t *testing.T) {
	t.Parallel()
	got := sandboxEnvOf(t, map[string]string{
		"HOME":                           "/home/engine",
		"CLAUDE_CONFIG_DIR":              "/home/engine/.claude",
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS": "0",
		"ANTHROPIC_BASE_URL":             "  ",
		"ANTHROPIC_MODEL":                "claude-sonnet-5",
	}, Auth{Mode: AuthAPIKey, APIKey: "sk-ant-entry"})
	if want := (map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-entry", "ANTHROPIC_MODEL": "claude-sonnet-5",
	}); !maps.Equal(got, want) {
		t.Errorf("the box's entry environment = %v, want exactly %v", got, want)
	}
}
