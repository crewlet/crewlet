package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// harnessEntry is a company with one providers.llm entry, "sub", whose body is the
// YAML lines given (indented under the entry).
func harnessEntry(lines ...string) string {
	var b strings.Builder
	b.WriteString("name: Acme\nproviders:\n  llm:\n    sub:\n      model: m\n")
	for _, line := range lines {
		b.WriteString("      " + line + "\n")
	}
	return b.String()
}

// EVERY CREDENTIAL A PROVIDERS.LLM ENTRY CONFIGURES REACHES THE HARNESS, OR THE
// ENTRY IS REFUSED. The backend places a credential only in a variable its profile
// names and drops it silently otherwise, so each of these validated clean and
// ran signed in to nothing — and only `crewlet validate` could ever have said
// so, since no API write builds a provider. Judged against the MERGED profile,
// so an override that names the variable is what makes an entry valid.
func TestAHarnessCredentialWithNowhereToGoIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		lines []string
		path  string
		kind  error
		says  string
	}{
		{
			name: "api-key on a harness that reads its provider's key from its environment",
			lines: []string{"harness: hermes",
				`auth: {mode: api-key, api_key: "${OPENROUTER_API_KEY}"}`},
			path: "providers.llm.sub.auth.mode", kind: ErrConflict,
			says: `set the key in env under the variable of the provider its model names`,
		},
		{
			name: "api-key on a harness that reads no key from its environment",
			lines: []string{"harness: kimi-code",
				`auth: {mode: api-key, api_key: "${KIMI_API_KEY}"}`},
			path: "providers.llm.sub.auth.mode", kind: ErrConflict,
			says: "config.toml), so put it there",
		},
		{
			// OPENAI_API_KEY holds other vendors' keys too, and Qwen Code
			// sends whatever it finds there to DashScope unless
			// OPENAI_BASE_URL says otherwise: nothing falls back to it.
			name:  "api-key naming no key on a harness whose key variable is shared",
			lines: []string{"harness: qwen-code", "auth: {mode: api-key}"},
			path:  "providers.llm.sub.auth.api_key", kind: ErrMissing,
			says: "reads its key from OPENAI_API_KEY, a variable other vendors' keys are kept under too",
		},
		{
			// inherit-env would forward whatever the engine's process
			// holds under OPENAI_API_KEY — the embeddings key, on most
			// hosts — to Qwen Code's endpoint.
			name:  "inherit-env on a harness whose key variable is shared",
			lines: []string{"harness: qwen-code", "auth: {mode: inherit-env}"},
			path:  "providers.llm.sub.auth.mode", kind: ErrConflict,
			says: "inherit-env forwards OPENAI_API_KEY from the engine's own environment",
		},
		{
			name:  "an API key under subscription",
			lines: []string{"harness: claude-code", `auth: {api_key: "${ANTHROPIC_API_KEY}"}`},
			path:  "providers.llm.sub.auth.api_key", kind: ErrConflict,
			says: "an API key is read only under auth.mode api-key",
		},
		{
			name:  "a token on a harness that mints none",
			lines: []string{"harness: codex", `auth: {token: "${CODEX_TOKEN}"}`},
			path:  "providers.llm.sub.auth.token", kind: ErrConflict,
			says: `the "codex" profile names no token_env`,
		},
		{
			name:  "a token under api-key, which removes it",
			lines: []string{"harness: claude-code", `auth: {mode: api-key, token: "${TOK}"}`},
			path:  "providers.llm.sub.auth.token", kind: ErrConflict,
			says: "a token is read only under auth.mode subscription",
		},
		{
			// Qwen Code keeps no login of its own and mints no token: under
			// subscription, the mode that removes its key variable, nothing
			// signs it in.
			name:  "subscription on a harness that signs in only with its key",
			lines: []string{"harness: qwen-code"},
			path:  "providers.llm.sub.auth.mode", kind: ErrConflict,
			says: "set auth.mode: api-key and give the key as auth.api_key (the harness reads it from OPENAI_API_KEY)",
		},
		{
			name:  "inherit-env on a harness that names neither variable",
			lines: []string{"harness: pi", "auth: {mode: inherit-env}"},
			path:  "providers.llm.sub.auth.mode", kind: ErrConflict,
			says: `the "pi" profile names neither`,
		},
		{
			name:  "the key variable written into env",
			lines: []string{"harness: grok", `env: {XAI_API_KEY: "${XAI_API_KEY}"}`},
			path:  "providers.llm.sub.env.XAI_API_KEY", kind: ErrConflict,
			says: "the key as auth.api_key with auth.mode api-key",
		},
		{
			name:  "the token variable written into env",
			lines: []string{"harness: claude-code", `env: {CLAUDE_CODE_OAUTH_TOKEN: "${TOK}"}`},
			path:  "providers.llm.sub.env.CLAUDE_CODE_OAUTH_TOKEN", kind: ErrConflict,
			says: "the token as auth.token",
		},
		{
			// Set with the mode, over env: under subscription a value here
			// would be replaced on every call and nothing would say so.
			name:  "a subscription's login selector in env",
			lines: []string{"harness: gemini-cli", `env: {GOOGLE_GENAI_USE_GCA: "false"}`},
			path:  "providers.llm.sub.env.GOOGLE_GENAI_USE_GCA", kind: ErrConflict,
			says: "set by the \"gemini-cli\" profile under auth.mode subscription, to \"true\"",
		},
		{
			// env is layered over the variables that keep every seat's
			// harness in its own home, so this would hand every seat the
			// engine user's dotfiles and nothing would say so.
			name:  "a home isolation variable written into env",
			lines: []string{"harness: claude-code", "env: {HOME: /home/engine}"},
			path:  "providers.llm.sub.env.HOME", kind: ErrConflict,
			says: "a value here would replace that isolation for every seat",
		},
		{
			name:  "the profile's own relocation variable written into env",
			lines: []string{"harness: codex", "env: {CODEX_HOME: /srv/codex}"},
			path:  "providers.llm.sub.env.CODEX_HOME", kind: ErrConflict,
			says: "CODEX_HOME is a variable the engine sets for every call",
		},
		{
			name:  "a credential in the profile's own env",
			lines: []string{"harness: hermes", "overrides: {env: {OPENROUTER_API_KEY: sk-or-literal}}"},
			path:  "providers.llm.sub.overrides", kind: ErrConflict,
			says: "put it in the entry's own env, which is ${VAR}-resolved",
		},
		{
			// No store can hold the name, so `crewlet llm login
			// -capture-token` cannot store the token under it, and no ${VAR}
			// can name it.
			name:  "a token variable no store can hold",
			lines: []string{"harness: claude-code", "overrides: {token_env: MY-TOKEN}"},
			path:  "providers.llm.sub.overrides.token_env", kind: ErrShape,
			says: `"MY-TOKEN" is not an environment variable name`,
		},
		{
			// `MY=KEY=<key>` in the child's environment is a variable called
			// MY, which is not the one the harness reads.
			name: "a key variable carrying an equals sign",
			lines: []string{"harness: claude-code",
				`auth: {mode: api-key, api_key: "${ANTHROPIC_API_KEY}"}`,
				`overrides: {api_key_env: "MY=KEY"}`},
			path: "providers.llm.sub.overrides.api_key_env", kind: ErrShape,
			says: `"MY=KEY" is not an environment variable name`,
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

// assertAdmissionOnly holds a refusal to the ADMISSION set: the revision is
// refused on a write but applies — runnable, with the fault reported as a
// warning at the same path — because the entry it is about still runs. An
// apply that refused it would take a node off the fleet's epoch during a
// rolling upgrade over a revision a newer peer admitted.
func assertAdmissionOnly(t *testing.T, doc, path string) {
	t.Helper()
	cfg, err := ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.ValidateRunnable(); err != nil {
		t.Errorf("ValidateRunnable() = %v, want nil: the entry runs, so an apply must not refuse it", err)
	}
	if err := cfg.ValidateAdmission(); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("ValidateAdmission() = %v, want a refusal at %s", err, path)
	}
	warned := false
	for _, w := range cfg.AdmissionWarnings() {
		warned = warned || w.Path == path
	}
	if !warned {
		t.Errorf("an applied revision is not warned about at %s: %+v", path, cfg.AdmissionWarnings())
	}
}

// AN UNKNOWN AUTH MODE IS ONE FAULT, reported where it is. The credential
// rules each state what a mode does with a credential, and judged against a
// mode that does not exist they drew refusals naming "apikey" as though it
// removed a token and ignored a key.
func TestAnUnknownHarnessAuthModeIsReportedOnce(t *testing.T) {
	t.Parallel()
	doc := harnessEntry("harness: claude-code", `auth: {mode: apikey, api_key: "${K}", token: "${T}"}`)
	cfg, err := ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	problems := Problems(cfg.Validate())
	if len(problems) != 1 || problems[0].Path != "providers.llm.sub.auth.mode" {
		t.Errorf("want exactly one problem, at auth.mode; got %+v", problems)
	}
}

// A PROFILE WITH NO MODEL FLAG CANNOT RUN AN ENTRY, and every entry names a
// model: the backend refuses to build rather than drop it. That refusal lived
// only in the backend, so every API write admitted the shape and every node's
// apply then refused it. A RUNNABLE rule, unlike the credential ones: the
// provider cannot be built.
func TestAProfileWithNoModelFlagIsRefusedOnEveryPath(t *testing.T) {
	t.Parallel()
	doc := harnessEntry("harness: codex", "overrides: {model_args: []}")
	err := rejects(t, doc, "providers.llm.sub.overrides.model_args")
	if !errors.Is(err, ErrMissing) || !strings.Contains(err.Error(), `"--model", "{model}"`) {
		t.Errorf("want a missing-field refusal showing the flag to declare, got %v", err)
	}
	cfg, err := ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.ValidateRunnable(); err == nil {
		t.Error("ValidateRunnable() admitted an entry whose backend cannot be built")
	}

	// A flag WITHOUT the placeholder drops the model as silently: it
	// passes the same fixed value on every call.
	err = rejects(t, harnessEntry("harness: codex", `overrides: {model_args: ["--model", "gpt-5"]}`),
		"providers.llm.sub.overrides.model_args")
	if !errors.Is(err, ErrMissing) || !strings.Contains(err.Error(), "{model}") {
		t.Errorf("want a missing-placeholder refusal, got %v", err)
	}
}

// The routes each harness does read are accepted, or the rules above refuse what
// they exist to steer people to — and the MERGED profile is what is judged,
// so naming a variable with overrides makes api-key valid on a harness whose
// built-in profile has none.
func TestEveryCredentialRouteAHarnessReadsIsAccepted(t *testing.T) {
	t.Parallel()
	for name, lines := range map[string][]string{
		"a hermes key in env": {
			"harness: hermes", `env: {OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"}`},
		"a metered key in api-key mode": {
			"harness: grok", `auth: {mode: api-key, api_key: "${XAI_API_KEY}"}`},
		// The key named nowhere in the document: the harness's own key
		// variable, read through the resolver at build — which is what
		// lets a metered entry run on the variable every operator of the
		// vendor already exports.
		"api-key on the harness's own variable": {
			"harness: claude-code", "auth: {mode: api-key}"},
		"a headless token in subscription mode": {
			"harness: claude-code", `auth: {token: "${MY_OAUTH_TOKEN}"}`},
		"inherit-env on a harness with a key variable": {
			"harness: codex", "auth: {mode: inherit-env}"},
		"api-key once an override names the variable": {
			"harness: hermes", `auth: {mode: api-key, api_key: "${OPENROUTER_API_KEY}"}`,
			"overrides: {api_key_env: OPENROUTER_API_KEY}"},
		"cursor-agent's own key variable": {
			"harness: cursor-agent", `auth: {mode: api-key, api_key: "${CURSOR_API_KEY}"}`},
		// Under api-key the profile sets nothing there, so the operator's
		// own choice of sign-in stands.
		"a login selector in env under api-key": {
			"harness: gemini-cli", `auth: {mode: api-key, api_key: "${GEMINI_API_KEY}"}`,
			`env: {GOOGLE_GENAI_USE_GCA: "false"}`},
		"qwen-code's key and its endpoint": {
			"harness: qwen-code", `auth: {mode: api-key, api_key: "${DASHSCOPE_API_KEY}"}`,
			"env: {OPENAI_BASE_URL: https://dashscope-intl.aliyuncs.com/compatible-mode/v1}"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mustCompany(t, harnessEntry(lines...))
		})
	}
}

// A profile that cannot load is refused where every write path sees it — the
// API admits through these rules and never builds a provider — at the field
// that holds the fault, rather than with an empty path from the epoch build.
func TestAHarnessProfileThatCannotLoadIsRefusedAtItsOverrides(t *testing.T) {
	t.Parallel()
	err := rejects(t, harnessEntry("harness: custom"), "providers.llm.sub.overrides")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "set overrides.binary") {
		t.Errorf("want a conflict naming overrides.binary, got %v", err)
	}

	err = rejects(t, harnessEntry("harness: codex", "overrides: {complete_arg: [exec]}"),
		"providers.llm.sub.overrides")
	if !errors.Is(err, ErrShape) || !strings.Contains(err.Error(), "complete_arg") {
		t.Errorf("want a shape error naming the misspelled field, got %v", err)
	}
}

// EVERY SHIPPED PROFILE HANDS ITS CREDENTIALS OVER UNDER NAMES OF THE
// REFERENCE GRAMMAR, which is what lets the refusal of one outside it be about
// overrides alone: a shipped name outside it would be refused at a field
// the operator never wrote, and its token could never be stored.
func TestEveryShippedProfileNamesItsCredentialsInTheGrammar(t *testing.T) {
	t.Parallel()
	for _, name := range cliprofile.BuiltinNames() {
		profile, ok := cliprofile.Builtin(name)
		if !ok {
			t.Fatalf("%s is listed and not built in", name)
		}
		for field, variable := range map[string]string{
			"token_env": profile.TokenEnv, "api_key_env": profile.APIKeyEnv,
		} {
			if variable != "" && !envref.ValidName(variable) {
				t.Errorf("%s: %s %q is not an environment variable name", name, field, variable)
			}
		}
	}
}

// A SHARED KEY VARIABLE IS NEVER READ IN AN ENTRY'S PLACE. The fallback exists
// for a vendor's own variable — ANTHROPIC_API_KEY for Claude Code is the key
// somebody exported for exactly that harness — and must not reach for a generic
// one: Qwen Code's OPENAI_API_KEY on the engine's host is usually an OpenAI
// key, and Qwen Code would send it to DashScope.
func TestASharedKeyVariableIsNeverAnEntrysFallback(t *testing.T) {
	t.Parallel()
	for agent, want := range map[Harness]string{
		"claude-code": "ANTHROPIC_API_KEY",
		"codex":       "CODEX_API_KEY",
		"qwen-code":   "",
		"copilot":     "COPILOT_GITHUB_TOKEN",
	} {
		entry := LLMProvider{Harness: agent, Model: "m", Auth: HarnessAuth{Mode: AuthAPIKey}}
		if got := entry.CredentialSources("sub").APIKey.Fallback; got != want {
			t.Errorf("%s: an api-key entry naming no key falls back to %q, want %q", agent, got, want)
		}
	}
}
