package harness

import (
	"os"
	"path/filepath"
	"slices"
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
			// THE REPORTED BUG: a hermes key given through env, the
			// route its profile names, was "no login of its own".
			name:  "a env key on a harness that reads its provider's key from its environment",
			agent: "hermes", env: map[string]string{"OPENROUTER_API_KEY": "sk-or-x"},
			signedIn: true, line: "env sets OPENROUTER_API_KEY",
		},
		{
			name: "the same key on opencode, which also reads it", agent: "opencode",
			env:      map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x"},
			signedIn: true, line: "env sets ANTHROPIC_API_KEY",
		},
		{
			// kimi-code is handed KIMI_API_KEY and ignores it.
			name:  "a env key on a harness that does not read one",
			agent: "kimi-code", env: map[string]string{"KIMI_API_KEY": "sk-kimi"},
			problem: "no sign-in",
		},
		{
			name:  "a env variable that is configuration, not a credential",
			agent: "hermes", env: map[string]string{"HTTPS_PROXY": "http://proxy.example.com"},
			problem: "env, under that provider's own variable",
		},
		{
			// A count of model tokens is configuration: read as a
			// credential by a substring match, it reported an entry with
			// no key at all as signed in.
			name:  "a env tuning variable whose name holds TOKENS",
			agent: "hermes", env: map[string]string{"HERMES_MAX_TOKENS": "4096"},
			problem: "env, under that provider's own variable",
		},
		{
			// A harness that fronts any provider may be pointed at a server
			// that takes no key, which only the smoke test can tell
			// apart — so the problem says how to settle it.
			name:  "a harness that may be served by a keyless endpoint",
			agent: "pi", env: map[string]string{"OPENAI_BASE_URL": "http://127.0.0.1:11434/v1"},
			problem: "served by an endpoint that takes no key, run the doctor without -no-smoke",
		},
		{
			name:  "a env key whose ${VAR} resolved to nothing",
			agent: "pi", env: map[string]string{"ANTHROPIC_API_KEY": ""},
			problem: "env sets ANTHROPIC_API_KEY, but the ${VAR} it references resolved to nothing",
			// It already took the env route; being told to take it
			// would read as a second fault.
			without: "or set the key of the provider its model names",
		},
		{
			// Subscription mode removes the profile's api_key_env from
			// the child, which is the guard against a metered bill.
			name:  "a metered key in env that subscription mode removes",
			agent: "claude-code", env: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x"},
			auth:    Auth{Mode: AuthSubscription},
			problem: "run `crewlet llm login sub`",
			// Claude Code reaches only Anthropic: no keyless endpoint to
			// point it at, so no such note.
			without: "endpoint that takes no key",
		},
		{
			// Gemini CLI declares no login subcommand, so `crewlet llm
			// login sub` refuses for it: the route that works is a login
			// made with the harness by hand, adopted.
			name: "a harness with no login command of its own", agent: "gemini-cli",
			problem: "`crewlet llm login sub -from-host`",
			without: "run `crewlet llm login sub`",
		},
		{
			// Qwen Code keeps no login of its own — both measured builds
			// refuse the OAuth login it once kept — so there is nothing to
			// adopt or broker: its key is the sign-in.
			name: "a harness that keeps no login", agent: "qwen-code",
			problem: "set auth.mode: api-key and give the key as auth.api_key (the harness reads it from OPENAI_API_KEY)",
			without: "-from-host",
		},
		{
			// The operator wrote a token reference; that it resolved to
			// nothing is the fault to name, ahead of the routes they did
			// not take.
			name: "a auth.token whose ${VAR} resolved to nothing", agent: "claude-code",
			auth:    Auth{Mode: AuthSubscription, TokenConfigured: true},
			problem: "auth.token is set, but the ${VAR} it references resolved to nothing",
		},
		{
			// THE FALSE HEALTHY: api-key mode removes the token, so a
			// resolved token is not a sign-in there.
			name: "api-key mode with no key and a token it removes", agent: "claude-code",
			auth: Auth{Mode: AuthAPIKey, Token: "oauth-token"},
			problem: "its auth.mode is api-key, which hands the harness only its key " +
				"(providers.llm.sub.auth.api_key, else ANTHROPIC_API_KEY), and that key resolved to nothing",
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
		Key: "sub", Harness: c.agent, StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
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
	// AN EMPTY HOME: a login the machine running the suite holds would turn
	// every remedy into "adopt it", whatever the case is about.
	d := p.Diagnose(t.Context(), DiagnoseOptions{HostHome: t.TempDir()})
	var problem string
	for _, line := range d.Problems {
		if strings.HasPrefix(line, "no sign-in") {
			problem = line
		}
	}
	state := p.SignInState(t.Context())

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
	if got := newSignInProvider(t, c).SignInState(t.Context()); got != "inherited key" {
		t.Errorf("SignInState = %q, want inherited key", got)
	}

	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "inherited")
	c.line = "CLAUDE_CODE_OAUTH_TOKEN from this process's environment"
	checkSignIn(t, newSignInProvider(t, c), c)
	if got := newSignInProvider(t, c).SignInState(t.Context()); got != "inherited token" {
		t.Errorf("SignInState = %q, want inherited token", got)
	}
}

// AN ANSWERED SMOKE TEST IS A SIGN-IN, whatever the configuration shows. A
// harness pointed at an endpoint that takes no key, or holding a key in a file of
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
		{"the harness answers", map[string]string{"FAKE_STDOUT": reply}, true},
		{"the harness fails", map[string]string{"FAKE_STDERR": "unauthorized", "FAKE_EXIT": "1"}, false},
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
					t.Errorf("a harness that answered was reported as signed in to nothing: %s", problem)
				}
				if !strings.Contains(d.SignIn, "the smoke test was answered") {
					t.Errorf("sign-in line %q does not say why no problem was raised", d.SignIn)
				}
				return
			}
			if problem == "" {
				t.Errorf("a harness that failed with nothing to sign it in raised no problem (sign-in: %q)", d.SignIn)
			}
		})
	}
}

// A MODE'S SIGN-IN NAMES ONLY THE VARIABLES ITS PROFILE HAS, since those are
// the only ones [applyAuth] hands the harness. Codex has a key variable and no
// token one, so inherit-env forwards one name, and an entry whose profile has
// none for its mode — which every write of the configuration refuses, and an
// apply only warns about — is told that the mode itself is the remedy, never
// to set a variable that would reach nothing. Subscription has none at all: its
// credentials are a login and a stored token, which each message words for its
// own setting.
func TestEachModesSignInNamesOnlyTheVariablesItsProfileHas(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		agent     string
		mode      AuthMode
		variables []string
		want      []string
		notWant   []string
	}{
		{agent: "claude-code", mode: AuthSubscription},
		{
			agent: "claude-code", mode: AuthInheritEnv,
			variables: []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"},
			want:      []string{"CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY", "neither is set there", "export one"},
		},
		{
			agent: "codex", mode: AuthAPIKey, variables: []string{"CODEX_API_KEY"},
			want: []string{"its auth.mode is api-key", "providers.llm.sub.auth.api_key, else CODEX_API_KEY",
				"`crewlet secrets set`"},
		},
		{
			// OPENAI_API_KEY holds other vendors' keys too: the engine
			// never reads it in the entry's place, so nothing says to
			// export it there.
			agent: "qwen-code", mode: AuthAPIKey, variables: []string{"OPENAI_API_KEY"},
			want: []string{"its key, as OPENAI_API_KEY (providers.llm.sub.auth.api_key)",
				"under the name auth.api_key references"},
			notWant: []string{"else ", "export"},
		},
		{
			agent: "codex", mode: AuthInheritEnv, variables: []string{"CODEX_API_KEY"},
			want:    []string{"only CODEX_API_KEY from the engine's own process environment", "it is not set there", "export it"},
			notWant: []string{" or ", "neither"},
		},
		{
			agent: "hermes", mode: AuthAPIKey,
			want:    []string{"no api_key_env", "set auth.mode: subscription"},
			notWant: []string{"else ", "crewlet secrets set"},
		},
		{
			agent: "hermes", mode: AuthInheritEnv,
			want:    []string{"neither a token_env nor an api_key_env", "set auth.mode: subscription"},
			notWant: []string{"export"},
		},
	} {
		t.Run(tc.agent+"/"+string(tc.mode), func(t *testing.T) {
			t.Parallel()
			p := newSignInProvider(t, signInCase{agent: tc.agent, auth: Auth{Mode: tc.mode}})
			sign, fromEnv := p.ModeSignIn()
			if fromEnv != (tc.mode != AuthSubscription) {
				t.Fatalf("ModeSignIn reports %v for %s", fromEnv, tc.mode)
			}
			if !fromEnv {
				return
			}
			if !slices.Equal(sign.Variables, tc.variables) {
				t.Errorf("Variables = %v, want %v", sign.Variables, tc.variables)
			}
			missing := sign.Missing()
			for _, want := range tc.want {
				if !strings.Contains(missing, want) {
					t.Errorf("%q does not say %q", missing, want)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(missing, bad) {
					t.Errorf("%q says %q, which this profile does not have", missing, bad)
				}
			}
		})
	}
}
