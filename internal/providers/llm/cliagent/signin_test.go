package cliagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// signInCase is one entry whose sign-in the doctor and the list must both
// judge from the environment the child is actually given.
type signInCase struct {
	name  string
	agent string
	env   map[string]string
	auth  Auth
	// files writes a credential file into the shared directory first.
	files bool

	signedIn bool
	// line is a fragment the sign-in line must carry when signed in, and
	// problem one the "no sign-in" problem must carry when not.
	line, problem string
	// without is a fragment the problem must NOT carry.
	without string
}

func signInCases() []signInCase {
	return []signInCase{
		{
			// THE REPORTED BUG: a hermes key given through cli.env, the
			// route its profile names, was "no login of its own".
			name:  "a cli.env key on a CLI that reads its provider's key from its environment",
			agent: "hermes", env: map[string]string{"OPENROUTER_API_KEY": "sk-or-x"},
			signedIn: true, line: "cli.env sets OPENROUTER_API_KEY",
		},
		{
			name: "the same key on opencode, which also reads it", agent: "opencode",
			env:      map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x"},
			signedIn: true, line: "cli.env sets ANTHROPIC_API_KEY",
		},
		{
			// kimi-code is handed KIMI_API_KEY and ignores it.
			name:  "a cli.env key on a CLI that does not read one",
			agent: "kimi-code", env: map[string]string{"KIMI_API_KEY": "sk-kimi"},
			problem: "no sign-in",
		},
		{
			name:  "a cli.env variable that is configuration, not a credential",
			agent: "hermes", env: map[string]string{"HTTPS_PROXY": "http://proxy.example.com"},
			problem: "cli.env, under that provider's own variable",
		},
		{
			// A count of model tokens is configuration: read as a
			// credential by a substring match, it reported an entry with
			// no key at all as signed in.
			name:  "a cli.env tuning variable whose name holds TOKENS",
			agent: "hermes", env: map[string]string{"HERMES_MAX_TOKENS": "4096"},
			problem: "cli.env, under that provider's own variable",
		},
		{
			// A CLI that fronts any provider may be pointed at a server
			// that takes no key, which only the smoke test can tell
			// apart — so the problem says how to settle it.
			name:  "a CLI that may be served by a keyless endpoint",
			agent: "pi", env: map[string]string{"OPENAI_BASE_URL": "http://127.0.0.1:11434/v1"},
			problem: "served by an endpoint that takes no key, run the doctor without -no-smoke",
		},
		{
			name:  "a cli.env key whose ${VAR} resolved to nothing",
			agent: "pi", env: map[string]string{"ANTHROPIC_API_KEY": ""},
			problem: "cli.env sets ANTHROPIC_API_KEY, but the ${VAR} it references resolved to nothing",
			// It already took the cli.env route; being told to take it
			// would read as a second fault.
			without: "or set the key of the provider its model names",
		},
		{
			// Subscription mode removes the profile's api_key_env from
			// the child, which is the guard against a metered bill.
			name:  "a metered key in cli.env that subscription mode removes",
			agent: "claude-code", env: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x"},
			auth:    Auth{Mode: AuthSubscription},
			problem: "run `crewlet llm login sub`",
			// Claude Code reaches only Anthropic: no keyless endpoint to
			// point it at, so no such note.
			without: "endpoint that takes no key",
		},
		{
			// The operator wrote a token reference; that it resolved to
			// nothing is the fault to name, ahead of the routes they did
			// not take.
			name: "a cli.auth.token whose ${VAR} resolved to nothing", agent: "claude-code",
			auth:    Auth{Mode: AuthSubscription, TokenConfigured: true},
			problem: "cli.auth.token is set, but the ${VAR} it references resolved to nothing",
		},
		{
			// THE FALSE HEALTHY: api-key mode removes the token, so a
			// resolved token is not a sign-in there.
			name: "api-key mode with no key and a token it removes", agent: "claude-code",
			auth:    Auth{Mode: AuthAPIKey, Token: "oauth-token"},
			problem: "auth.mode is api-key and its api_keys value resolved to nothing",
		},
		{
			name: "a keyed api-key entry", agent: "grok",
			auth:     Auth{Mode: AuthAPIKey, APIKey: "xai-key"},
			signedIn: true, line: "API key in XAI_API_KEY (auth.mode api-key)",
		},
		{
			name: "a headless token in subscription mode", agent: "claude-code",
			auth:     Auth{Mode: AuthSubscription, Token: "oauth-token"},
			signedIn: true, line: "headless token in CLAUDE_CODE_OAUTH_TOKEN",
		},
		{
			name: "credential files on disk", agent: "hermes", files: true,
			signedIn: true, line: "credential files in",
		},
	}
}

func newSignInProvider(t *testing.T, c signInCase) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	auth := c.auth
	if auth.Mode == "" {
		auth.Mode = AuthSubscription
	}
	p, err := New(Config{
		Key: "sub", Agent: c.agent, StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
		// No binary to run: the version probe is skipped and the sign-in
		// is judged on configuration and files alone.
		Overrides: map[string]any{"binary": "crewlet-no-such-cli-binary"},
		Env:       c.env, Auth: auth,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.files {
		shared := p.Workspace().CredentialsDir()
		if err := os.MkdirAll(shared, 0o700); err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(p.Profile().CredentialPaths[0])
		if err := os.WriteFile(filepath.Join(shared, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// checkSignIn asserts the doctor's verdict and the list's agree, and that the
// verdict is the expected one.
func checkSignIn(t *testing.T, p *Provider, c signInCase) {
	t.Helper()
	d := p.Diagnose(t.Context(), DiagnoseOptions{})
	var problem string
	for _, line := range d.Problems {
		if strings.HasPrefix(line, "no sign-in") {
			problem = line
		}
	}
	state := p.SignInState()

	// THE TWO SURFACES CANNOT DISAGREE: they did, because each kept its
	// own idea of what "logged in" meant.
	if (state == "none") != (problem != "") {
		t.Errorf("llm list says %q while doctor's problem is %q", state, problem)
	}
	if c.signedIn {
		if problem != "" {
			t.Errorf("a signed-in entry was reported as having no sign-in: %s", problem)
		}
		if !strings.Contains(d.SignIn, c.line) {
			t.Errorf("sign-in line %q does not name %q", d.SignIn, c.line)
		}
		return
	}
	if problem == "" {
		t.Fatalf("nothing authenticates this entry and the doctor raised no problem (sign-in: %q)", d.SignIn)
	}
	if d.SignIn != "none" {
		t.Errorf("sign-in line = %q, want none", d.SignIn)
	}
	if !strings.Contains(problem, c.problem) {
		t.Errorf("the problem does not say %q:\n%s", c.problem, problem)
	}
	if c.without != "" && strings.Contains(problem, c.without) {
		t.Errorf("the problem says %q, which does not apply here:\n%s", c.without, problem)
	}
}

// SIGN-IN IS JUDGED ON THE ENVIRONMENT THE CHILD IS GIVEN, after applyAuth —
// never on a second reading of the configuration beside it.
func TestSignInIsReadOffTheEnvironmentTheChildIsGiven(t *testing.T) {
	t.Parallel()
	for _, c := range signInCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			checkSignIn(t, newSignInProvider(t, c), c)
		})
	}
}

// inherit-env forwards the engine's own variable, so it is a sign-in exactly
// when this process has one — not because the mode was named.
func TestAnInheritedTokenIsASignInOnlyWhenThisProcessHasOne(t *testing.T) {
	c := signInCase{
		name: "inherit-env", agent: "claude-code", auth: Auth{Mode: AuthInheritEnv},
		problem: "no sign-in",
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	checkSignIn(t, newSignInProvider(t, c), c)

	// The KEY half, read off the child as well: an inherit-env entry's key
	// is in no configuration field at all, only in this process's
	// environment, so a sign-in judged from the configuration could not see
	// it.
	t.Setenv("ANTHROPIC_API_KEY", "inherited")
	c.signedIn, c.line = true, "ANTHROPIC_API_KEY from this process's environment"
	checkSignIn(t, newSignInProvider(t, c), c)
	if got := newSignInProvider(t, c).SignInState(); got != "inherited key" {
		t.Errorf("SignInState = %q, want inherited key", got)
	}

	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "inherited")
	c.line = "CLAUDE_CODE_OAUTH_TOKEN from this process's environment"
	checkSignIn(t, newSignInProvider(t, c), c)
	if got := newSignInProvider(t, c).SignInState(); got != "inherited token" {
		t.Errorf("SignInState = %q, want inherited token", got)
	}
}

// AN ANSWERED SMOKE TEST IS A SIGN-IN, whatever the configuration shows. A
// CLI pointed at an endpoint that takes no key, or holding a key in a file of
// its own, is signed in to nothing this engine hands it and works — and a
// "no sign-in" problem beside a passing completion is the false problem the
// doctor was raising for hermes. Without an answer the problem stands.
func TestAnAnsweredSmokeTestClearsANoSignInProblem(t *testing.T) {
	t.Parallel()
	reply := "```json\n{\"message\":\"\",\"tool_calls\":" +
		"[{\"name\":\"crewlet_smoke\",\"arguments\":{\"ok\":true}}]}\n```"
	for _, tc := range []struct {
		name     string
		env      map[string]string
		answered bool
	}{
		{"the CLI answers", map[string]string{"FAKE_STDOUT": reply}, true},
		{"the CLI fails", map[string]string{"FAKE_STDERR": "unauthorized", "FAKE_EXIT": "1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := fakeProvider(t, tc.env, map[string]any{"usage": map[string]any{"input": []any{[]any{"in"}}}})
			d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
			var problem string
			for _, line := range d.Problems {
				if strings.HasPrefix(line, "no sign-in") {
					problem = line
				}
			}
			if tc.answered {
				if problem != "" {
					t.Errorf("a CLI that answered was reported as signed in to nothing: %s", problem)
				}
				if !strings.Contains(d.SignIn, "the smoke test was answered") {
					t.Errorf("sign-in line %q does not say why no problem was raised", d.SignIn)
				}
				return
			}
			if problem == "" {
				t.Errorf("a CLI that failed with nothing to sign it in raised no problem (sign-in: %q)", d.SignIn)
			}
		})
	}
}
