package config

import (
	"errors"
	"strings"
	"testing"
)

// cliEntry is a company with one cli-agent entry, "sub", whose body is the
// YAML lines given (indented under the entry).
func cliEntry(lines ...string) string {
	var b strings.Builder
	b.WriteString("name: Acme\nproviders:\n  llm:\n    sub:\n      type: cli-agent\n      model: m\n")
	for _, line := range lines {
		b.WriteString("      " + line + "\n")
	}
	return b.String()
}

// EVERY CREDENTIAL A CLI-AGENT ENTRY CONFIGURES REACHES THE CLI, OR THE ENTRY
// IS REFUSED. The backend places a credential only in a variable its profile
// names and drops it silently otherwise, so each of these validated clean and
// ran signed in to nothing — and only `crewlet validate` could ever have said
// so, since no API write builds a provider. Judged against the MERGED profile,
// so an override that names the variable is what makes an entry valid.
func TestACLIAgentCredentialWithNowhereToGoIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		lines []string
		path  string
		kind  error
		says  string
	}{
		{
			name:  "api-key on a CLI that reads its provider's key from its environment",
			lines: []string{`api_keys: ["${OPENROUTER_API_KEY}"]`, "cli: {agent: hermes, auth: {mode: api-key}}"},
			path:  "providers.llm.sub.cli.auth.mode", kind: ErrConflict,
			says: `set the key in cli.env under the variable of the provider its model names`,
		},
		{
			name:  "api-key on a CLI that reads no key from its environment",
			lines: []string{`api_keys: ["${KIMI_API_KEY}"]`, "cli: {agent: kimi-code, auth: {mode: api-key}}"},
			path:  "providers.llm.sub.cli.auth.mode", kind: ErrConflict,
			says: "sign the CLI in with `crewlet llm login` instead",
		},
		{
			name:  "api-key with no key",
			lines: []string{"cli: {agent: claude-code, auth: {mode: api-key}}"},
			path:  "providers.llm.sub.api_keys", kind: ErrMissing,
			says: `add one, e.g. ["${ANTHROPIC_API_KEY}"]`,
		},
		{
			name:  "api_keys under subscription",
			lines: []string{`api_keys: ["${ANTHROPIC_API_KEY}"]`, "cli: {agent: claude-code}"},
			path:  "providers.llm.sub.api_keys", kind: ErrConflict,
			says: "reads api_keys only under cli.auth.mode api-key",
		},
		{
			name:  "two keys on an entry that rotates nothing",
			lines: []string{`api_keys: ["${A_KEY}", "${B_KEY}"]`, "cli: {agent: grok, auth: {mode: api-key}}"},
			path:  "providers.llm.sub.api_keys", kind: ErrConflict,
			says: "only the first key would ever reach the CLI",
		},
		{
			name:  "a token on a CLI that mints none",
			lines: []string{`cli: {agent: codex, auth: {token: "${CODEX_TOKEN}"}}`},
			path:  "providers.llm.sub.cli.auth.token", kind: ErrConflict,
			says: `the "codex" profile names no token_env`,
		},
		{
			name: "a token under api-key, which removes it",
			lines: []string{`api_keys: ["${ANTHROPIC_API_KEY}"]`,
				`cli: {agent: claude-code, auth: {mode: api-key, token: "${TOK}"}}`},
			path: "providers.llm.sub.cli.auth.token", kind: ErrConflict,
			says: "a token is read only under auth.mode subscription",
		},
		{
			name:  "inherit-env on a CLI that names neither variable",
			lines: []string{"cli: {agent: pi, auth: {mode: inherit-env}}"},
			path:  "providers.llm.sub.cli.auth.mode", kind: ErrConflict,
			says: `the "pi" profile names neither`,
		},
		{
			name:  "the key variable written into cli.env",
			lines: []string{`cli: {agent: grok, env: {XAI_API_KEY: "${XAI_API_KEY}"}}`},
			path:  "providers.llm.sub.cli.env.XAI_API_KEY", kind: ErrConflict,
			says: "give the key as api_keys with cli.auth.mode api-key",
		},
		{
			name:  "the token variable written into cli.env",
			lines: []string{`cli: {agent: claude-code, env: {CLAUDE_CODE_OAUTH_TOKEN: "${TOK}"}}`},
			path:  "providers.llm.sub.cli.env.CLAUDE_CODE_OAUTH_TOKEN", kind: ErrConflict,
			says: "give the token as cli.auth.token",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, cliEntry(tc.lines...), tc.path)
			if !errors.Is(err, tc.kind) {
				t.Errorf("want %v, got %v", tc.kind, err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not say %q:\n%v", tc.says, err)
			}
		})
	}
}

// The routes each CLI does read are accepted, or the rules above refuse what
// they exist to steer people to — and the MERGED profile is what is judged,
// so naming a variable with cli.overrides makes api-key valid on a CLI whose
// built-in profile has none.
func TestEveryCredentialRouteACLIReadsIsAccepted(t *testing.T) {
	t.Parallel()
	for name, lines := range map[string][]string{
		"a hermes key in cli.env": {
			`cli: {agent: hermes, env: {OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"}}`},
		"a metered key in api-key mode": {
			`api_keys: ["${XAI_API_KEY}"]`, "cli: {agent: grok, auth: {mode: api-key}}"},
		"a headless token in subscription mode": {
			`cli: {agent: claude-code, auth: {token: "${MY_OAUTH_TOKEN}"}}`},
		"inherit-env on a CLI with a key variable": {
			"cli: {agent: codex, auth: {mode: inherit-env}}"},
		"api-key once an override names the variable": {
			`api_keys: ["${OPENROUTER_API_KEY}"]`,
			"cli: {agent: hermes, auth: {mode: api-key}, overrides: {api_key_env: OPENROUTER_API_KEY}}"},
		"cursor-agent's own key variable": {
			`api_keys: ["${CURSOR_API_KEY}"]`, "cli: {agent: cursor-agent, auth: {mode: api-key}}"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mustCompany(t, cliEntry(lines...))
		})
	}
}

// A profile that cannot load is refused where every write path sees it — the
// API admits through these rules and never builds a provider — at the field
// that holds the fault, rather than with an empty path from the epoch build.
func TestACLIAgentProfileThatCannotLoadIsRefusedAtItsOverrides(t *testing.T) {
	t.Parallel()
	err := rejects(t, cliEntry("cli: {agent: custom}"), "providers.llm.sub.cli.overrides")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "set cli.overrides.binary") {
		t.Errorf("want a conflict naming cli.overrides.binary, got %v", err)
	}

	err = rejects(t, cliEntry("cli: {agent: codex, overrides: {complete_arg: [exec]}}"),
		"providers.llm.sub.cli.overrides")
	if !errors.Is(err, ErrShape) || !strings.Contains(err.Error(), "complete_arg") {
		t.Errorf("want a shape error naming the misspelled field, got %v", err)
	}
}
