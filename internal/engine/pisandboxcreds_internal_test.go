package engine

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A PI EXECUTOR'S LOGIN STAYS ON THE ENGINE HOST, ITS KEY GOES ANYWHERE: pi
// keeps a Claude, ChatGPT or Copilot login in its agent directory's auth.json,
// which a local cell is seeded with and writes a refreshed one back from, and
// which never travels; so an entry signed in by that login alone is refused a
// remote box, as a configuration to change, while a provider key in the run's
// environment signs it in on every cell.
//
// Mutation: drop the pi profile's env_sign_in, or let a login-only entry reach
// e2b: this goes red.
func TestAPiExecutorsLoginStaysLocalAndItsKeyTravels(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompany(t, "pi")
	for _, placement := range []sandbox.Placement{sandbox.Direct, sandbox.Container} {
		if err := guardOn(t, c, seat, placement, nil); err != nil {
			t.Errorf("a login-only pi run in %q was refused: %v", placement, err)
		}
	}
	err := guardOn(t, c, seat, sandbox.E2B, nil)
	var credErr *SandboxCredentialError
	if !errors.As(err, &credErr) {
		t.Fatalf("a login-only pi run on e2b = %v, want a SandboxCredentialError", err)
	}
	if err := guardOn(t, c, seat, sandbox.E2B, map[string]string{"ANTHROPIC_API_KEY": "sk-ant-not-real"}); err != nil {
		t.Errorf("a pi run carrying its provider's key was refused a remote box: %v", err)
	}
}

// A PI RUN NAMES THE VARIABLES THAT SIGN ITS HARNESS IN, which its runner keeps
// out of the model's shell: the credentials the entry's own env sets, never a
// setting beside them nor a credential the seat brings for its own work.
//
// Mutation: name every credential of the run, or none: this goes red.
func TestAPiRunNamesTheVariablesThatSignItIn(t *testing.T) {
	t.Parallel()
	provider, err := harness.New(harness.Config{
		Key: "sub", Harness: "pi", Model: "anthropic/claude-sonnet-4", StateDir: t.TempDir(), Timeout: time.Minute,
		Env: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-not-real", "HTTPS_PROXY": "http://proxy.example.com"},
	})
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	run := newExecutorRun(provider, runEnv{env: map[string]string{"NPM_TOKEN": "npm-not-real"}})
	if run.env["ANTHROPIC_API_KEY"] == "" || run.env["NPM_TOKEN"] == "" {
		t.Fatalf("the run's environment is %v", run.env)
	}
	if !slices.Equal(run.signInNames, []string{"ANTHROPIC_API_KEY"}) {
		t.Errorf("the run names %v as its sign-in, want ANTHROPIC_API_KEY alone", run.signInNames)
	}
}
