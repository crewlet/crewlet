package config

import (
	"errors"
	"strings"
	"testing"
)

// AN ENTRY UNDER auth.mode env IS ADMITTED ONLY WHERE ITS ENV SIGNS THE HARNESS
// IN, and a name that signs the harness in is admitted in env only under that mode.
// The mode hands the harness no token, no key and no login, so an env that signs
// nothing in is an entry signed in to nothing; and under any other mode the
// harness is handed the mode's own credential beside the env's, which it ranks by
// rules of its own (the plan token reached a Bedrock entry's box, where the
// cloud switch outranks it). Each is an admission rule: the harness still starts.
func TestAnEnvSignInIsTheEntrysOnlySignIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		lines []string
		path  string
		kind  error
		says  string
	}{
		{
			name:  "env mode with an env that sets no name the harness signs in by",
			lines: []string{"harness: claude-code", "auth: {mode: env}", "env: {AWS_REGION: eu-west-1}"},
			path:  "providers.llm.sub.env", kind: ErrMissing,
			says: "env sets none of the names the \"claude-code\" harness signs in by (ANTHROPIC_AUTH_TOKEN, " +
				"CLAUDE_CODE_USE_BEDROCK, CLAUDE_CODE_USE_VERTEX, CLAUDE_CODE_USE_FOUNDRY)",
		},
		{
			// Blank authenticates nothing, as everywhere a sign-in is read.
			name:  "env mode with a sign-in name written blank",
			lines: []string{"harness: claude-code", "auth: {mode: env}", `env: {CLAUDE_CODE_USE_BEDROCK: " "}`},
			path:  "providers.llm.sub.env", kind: ErrMissing,
			says: "the entry would sign in to nothing",
		},
		{
			name:  "env mode on a harness that reads its provider's key, with no credential in env",
			lines: []string{"harness: hermes", "auth: {mode: env}", "env: {OPENROUTER_BASE_URL: https://example.com}"},
			path:  "providers.llm.sub.env", kind: ErrMissing,
			says: "env holds no credential",
		},
		{
			name:  "env mode on a harness with no env sign-in at all",
			lines: []string{"harness: codex", "auth: {mode: env}"},
			path:  "providers.llm.sub.auth.mode", kind: ErrConflict,
			says: `the "codex" profile names no variable that does (sign_in_env)`,
		},
		{
			// The engine pins every harness's zone and reads resets in it, so an
			// entry's own TZ would be replaced, or read the reset wrong.
			name:  "a zone in env",
			lines: []string{"harness: claude-code", "env: {TZ: Europe/Berlin}"},
			path:  "providers.llm.sub.env.TZ", kind: ErrConflict,
			says: "the zone the engine hands every harness it starts",
		},
		{
			// A bundle is a login, and env seeds none anywhere.
			name: "a credential bundle under env mode",
			lines: []string{"harness: claude-code", `auth: {mode: env, credential_bundle: "${BUNDLE}"}`,
				"env: {CLAUDE_CODE_USE_BEDROCK: \"1\"}"},
			path: "providers.llm.sub.auth.credential_bundle", kind: ErrConflict,
			says: "a credential bundle reaches nothing",
		},
		{
			name: "a token under env mode",
			lines: []string{"harness: claude-code", `auth: {mode: env, token: "${TOK}"}`,
				"env: {CLAUDE_CODE_USE_BEDROCK: \"1\"}"},
			path: "providers.llm.sub.auth.token", kind: ErrConflict,
			says: "a token is read only under auth.mode subscription: env removes CLAUDE_CODE_OAUTH_TOKEN",
		},
		{
			name: "an API key under env mode",
			lines: []string{"harness: claude-code", `auth: {mode: env, api_key: "${KEY}"}`,
				"env: {CLAUDE_CODE_USE_BEDROCK: \"1\"}"},
			path: "providers.llm.sub.auth.api_key", kind: ErrConflict,
			says: "under env it reaches nothing",
		},
		{
			name:  "a cloud switch under the default mode",
			lines: []string{"harness: claude-code", `env: {CLAUDE_CODE_USE_BEDROCK: "1", AWS_REGION: eu-west-1}`},
			path:  "providers.llm.sub.env.CLAUDE_CODE_USE_BEDROCK", kind: ErrConflict,
			says: "set auth.mode: env",
		},
		{
			name: "a gateway's token under api-key",
			lines: []string{"harness: claude-code", "auth: {mode: api-key}",
				`env: {ANTHROPIC_AUTH_TOKEN: "${GATEWAY_TOKEN}"}`},
			path: "providers.llm.sub.env.ANTHROPIC_AUTH_TOKEN", kind: ErrConflict,
			says: "auth.mode api-key hands it a credential of its own beside it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, harnessEntry(tc.lines...), tc.path)
			if !errors.Is(err, tc.kind) {
				t.Errorf("want %v, got %v", tc.kind, err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not say %q:\n%v", tc.says, err)
			}
			assertAdmissionOnly(t, harnessEntry(tc.lines...), tc.path)
		})
	}
}

// The routes auth.mode env exists for are admitted: a cloud provider's switch
// with its region, a gateway's address and token, and on a harness that reads its
// provider's key from its environment, that key.
func TestEveryEnvSignInRouteIsAccepted(t *testing.T) {
	t.Parallel()
	for name, lines := range map[string][]string{
		"bedrock": {"harness: claude-code", "auth: {mode: env}",
			`env: {CLAUDE_CODE_USE_BEDROCK: "1", AWS_REGION: us-east-1, AWS_BEARER_TOKEN_BEDROCK: "${AWS_BEARER_TOKEN_BEDROCK}"}`},
		"vertex": {"harness: claude-code", "auth: {mode: env}",
			`env: {CLAUDE_CODE_USE_VERTEX: "1", CLOUD_ML_REGION: us-east5, ANTHROPIC_VERTEX_PROJECT_ID: "${GCP_PROJECT}"}`},
		"a gateway": {"harness: claude-code", "auth: {mode: env}",
			`env: {ANTHROPIC_BASE_URL: https://gateway.example.com, ANTHROPIC_AUTH_TOKEN: "${GATEWAY_TOKEN}"}`},
		"a provider key on an env_sign_in harness": {"harness: hermes", "auth: {mode: env}",
			`env: {OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mustCompany(t, harnessEntry(lines...))
		})
	}
}

// AN ENV ENTRY READS NO TOKEN AND NO KEY by convention: the plan token every
// subscription entry falls back to is exactly the credential the mode exists
// to keep away from a cloud or gateway entry. The bundle is read as under
// every mode.
func TestAnEnvEntryFallsBackToNoTokenAndNoKey(t *testing.T) {
	t.Parallel()
	entry := LLMProvider{Harness: HarnessClaudeCode, Model: "sonnet", Auth: HarnessAuth{Mode: AuthEnv}}
	sources := entry.CredentialSources("bedrock")
	if sources.Token.Fallback != "" || sources.APIKey.Fallback != "" {
		t.Errorf("an env entry falls back to token %q and key %q, want neither",
			sources.Token.Fallback, sources.APIKey.Fallback)
	}
	if sources.Bundle.Fallback != "CREWLET_LLM_CLI_BEDROCK_CREDENTIALS" {
		t.Errorf("bundle fallback = %q, want the entry's own bundle variable", sources.Bundle.Fallback)
	}
	// The counterfactual: the same entry under the default mode does fall
	// back to the plan token, so the assertion above is about the mode.
	entry.Auth.Mode = ""
	if got := entry.CredentialSources("bedrock").Token.Fallback; got != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("a subscription entry falls back to token %q, want CLAUDE_CODE_OAUTH_TOKEN", got)
	}
}
