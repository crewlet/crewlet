//go:build unix

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
)

// A REUSED BOX KEEPS WHAT IT HOLDS AND IS GIVEN NOTHING AGAIN. A continuing job
// reattaches to the box whose checkout holds the agent's work. The run, not the
// launch, checks out each repository (from `code_checkout`'s commands), so the
// second launch clones and fetches nothing though it carries the first job's
// grant, the agent's file survives, and the operator's setup is not applied
// twice. Run on the direct cell, against a stand-in git that records every call.
//
// Mutation: apply the operator's setup to a reused box as well — it is
// applied twice, red.
func TestAReusedBoxKeepsWhatItHoldsAndIsGivenNothingAgain(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	local := newDirect(t)
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: local},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	calls := filepath.Join(t.TempDir(), "git-calls")
	stub := gittest.Fake(t, "git version 2.56.0", `printf '%s\n' "$*" >> "$GIT_CALLS"
exit 0`)
	env := map[string]string{"GIT_CALLS": calls, "PATH": filepath.Dir(stub) + ":" + os.Getenv("PATH")}
	operator := []SetupStep{{Name: "operator", Commands: []string{"echo applied >> provisioned"}}}

	first := launchReq("t1")
	first.Spec.Env, first.Setup = env, operator
	res, err := rig.launchVia(t.Context(), manager, first)
	if err != nil {
		t.Fatalf("the first launch: %v", err)
	}
	box, err := local.Attach(t.Context(), res.SandboxID)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// THE CHECKOUT THE FIRST JOB'S AGENT MADE, and its work in it.
	workspace := filepath.Join(box.Home(), WorkspaceSubdir)
	work := filepath.Join(workspace, "web", "notes.txt")
	if err := os.MkdirAll(filepath.Join(workspace, "web", ".git"), 0o700); err != nil {
		t.Fatalf("the agent's checkout: %v", err)
	}
	if err := os.WriteFile(work, []byte("the agent's uncommitted work\n"), 0o600); err != nil {
		t.Fatalf("the agent's work: %v", err)
	}

	// A ROUND CARRIED ON, which keeps the first job's grant.
	second := withRepos(t, launchReq("t1"), granted("web", webRepoID))
	second.Spec.Env, second.Setup = env, operator
	resumeInItsBox(t, rig)
	again, err := rig.launchVia(t.Context(), manager, second)
	if err != nil {
		t.Fatalf("the second launch: %v", err)
	}
	if !again.Reused || again.SandboxID != res.SandboxID {
		t.Fatalf("the second launch = %+v, want it on the first one's box", again)
	}

	if body, err := os.ReadFile(calls); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			if strings.HasPrefix(line, "clone ") || strings.HasPrefix(line, "fetch ") {
				t.Fatalf("a launch cloned or fetched in the box: %q", line)
			}
		}
	}
	if got, err := os.ReadFile(work); err != nil || string(got) != "the agent's uncommitted work\n" {
		t.Fatalf("the agent's work in the reused checkout is %q (%v), want it as the agent left it", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "provisioned")); err != nil || string(got) != "applied\n" {
		t.Fatalf("the operator's setup left %q (%v), want it applied once, to the fresh box", got, err)
	}
}

// SETUP AND THE RUN SHARE ONE WORKING DIRECTORY: a setup step writes relative
// to where it runs and the harness looks from where it starts, so every backend
// runs a command with no directory of its own in one default (here the box's
// workspace).
//
// Mutation: give ApplySetup's commands a directory of their own (the box's
// .tmp) — the job finds no marker, red.
func TestSetupAndTheRunShareAWorkingDirectory(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	if err := ApplySetup(t.Context(), box, []SetupStep{{
		Name: "mark", Commands: []string{"printf here > marker"},
	}}, nil); err != nil {
		t.Fatalf("ApplySetup: %v", err)
	}
	// AS A HARNESS'S JOB IS STARTED: in no directory of its own
	// (internal/sandbox/executor's Runner.Start).
	if _, err := box.StartBackground(t.Context(), "test -f marker && printf found > found", ExecOptions{}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	found := filepath.Join(box.Home(), WorkspaceSubdir, "found")
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(found)
		return err == nil
	}, "the job did not find what the setup step wrote where it started")
}
