package sandbox_test

import (
	"context"
	"maps"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A SETUP STEP RUNS IN THE LOGIN SHELL THE CODING JOB DOES, with the run
// environment.
//
// A step that installs or configures a tool has to find the tool the agent will
// find — one a version manager puts on PATH only in a login shell — and see the
// variables the agent will see, over anything the box's profile exports. A
// local box ran its steps in a plain shell while its job ran in a login one, so
// a step's `npm` could be another than the job's.
//
// Mutation: drop Login from the step's exec and this goes red.
func TestASetupStepRunsInTheLoginShellTheJobDoes(t *testing.T) {
	t.Parallel()
	box := sandbox.NewFakeSandbox("box")
	var got []sandbox.ExecOptions
	box.ExecFunc = func(_ context.Context, _ string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
		got = append(got, opts)
		return sandbox.ExecResult{}, nil
	}
	env := map[string]string{"GITHUB_TOKEN": "declared-by-config"}
	if err := sandbox.ApplySetup(t.Context(), box, []sandbox.SetupStep{
		{Name: "deps", Commands: []string{"npm ci"}},
	}, env); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Login || !maps.Equal(got[0].Env, env) {
		t.Errorf("the step ran with %+v, want a login shell with the run environment", got)
	}
}
