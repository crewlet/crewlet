package executor_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notehelper/helperbuild"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// NO RUNNER STARTS WITHOUT THE HELPER ITS BOX HAS A BUILD FOR: every
// registered runner that writes the note helper into an x86_64 box when the
// engine has the build refuses the run, naming where it looked, when the
// engine is missing it. Several runners start without the helper on a machine
// no build exists for; a missing install must never reach that path.
// Mutation, red: answer a missing build as the no-build-for-this-machine error.
func TestNoRunnerStartsWithoutTheHelperItsBoxHasABuildFor(t *testing.T) {
	built, err := os.ReadFile(filepath.Join(os.Getenv(helperbuild.DirEnv), helperbuild.File("amd64")))
	if err != nil {
		t.Fatalf("TestMain's build of the note helper: %v", err)
	}
	var writers []executor.Harness
	for _, h := range executor.Registered() {
		box := x86Box(h.Name())
		req := sandbox.RunRequest{Brief: "x"}
		if _, err = h.WriteConfig(t.Context(), box, req, h.Session(req), executor.PathsFor(box)); err != nil {
			t.Fatalf("%s: WriteConfig with the helper at hand: %v", h.Name(), err)
		}
		for _, f := range box.Files() {
			if got, readErr := box.ReadFile(t.Context(), f); readErr == nil && bytes.Equal(got, built) {
				writers = append(writers, h)
				break
			}
		}
	}
	if len(writers) == 0 {
		t.Fatal("no registered runner wrote the note helper into its box, so this case judges nothing")
	}
	t.Setenv(helperbuild.DirEnv, t.TempDir())
	for _, h := range writers {
		box := x86Box(h.Name())
		req := sandbox.RunRequest{Brief: "x"}
		_, err = h.WriteConfig(t.Context(), box, req, h.Session(req), executor.PathsFor(box))
		if err == nil || !strings.Contains(err.Error(), "no note helper for a linux/amd64 box") {
			t.Errorf("%s with no helper installed: WriteConfig answered %v, want the refusal", h.Name(), err)
		}
	}
}

// x86Box is a fake box whose machine is x86_64, which the engine builds the
// note helper for.
func x86Box(name string) *sandbox.FakeSandbox {
	box := sandbox.NewFakeSandbox("box-" + name)
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if cmd == "uname -m" {
			return sandbox.ExecResult{Stdout: "x86_64\n"}, nil
		}
		return sandbox.ExecResult{}, nil
	}
	return box
}
