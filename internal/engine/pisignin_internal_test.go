package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

// A PI RUN'S ENVIRONMENT HOLDS ITS SIGN-IN AND THE SEAT'S OWN CREDENTIALS:
// the key the entry's own env sets and a credential the seat brings for its
// own work both reach the run, whose model's shell holds them as a person's
// would.
//
// Mutation: leave the entry's env or the seat's out of the run's.
func TestAPiRunsEnvironmentHoldsItsSignInAndTheSeatsCredentials(t *testing.T) {
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
}
