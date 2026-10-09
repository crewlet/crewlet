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
	state := p.LoginState()

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

	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "inherited")
	c.signedIn, c.line = true, "CLAUDE_CODE_OAUTH_TOKEN from this process's environment"
	checkSignIn(t, newSignInProvider(t, c), c)
	if got := newSignInProvider(t, c).LoginState(); got != "inherited token" {
		t.Errorf("LoginState = %q, want inherited token", got)
	}
}
