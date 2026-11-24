package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

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
