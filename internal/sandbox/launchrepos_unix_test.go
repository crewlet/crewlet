//go:build unix

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// A REUSED BOX IS GIVEN ONLY THE REPOSITORIES IT LACKS. A job that continues
// its turn's run reattaches to the box the job before it left, whose checkout
// holds the agent's work: each clone step's own guard skips a repository
// already checked out, so web is not cloned again and the agent's file in it
// survives, api — which the first job was not given — is cloned, and the
// operator's setup is not applied twice. On the direct cell, where the steps run for real, against
// a stand-in git that clones by making the directory.
//
// Mutation: guard the clone step on a directory the clone does not make (or
// drop the guard) — web is cloned a second time over the agent's work, red.
func TestAReusedBoxIsGivenOnlyTheRepositoriesItLacks(t *testing.T) {
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
	// A CLONE MAKES ITS DIRECTORY'S .git, as git's does; every other
	// command succeeds having done nothing, which is all the recipe asks of
	// it here.
	stub := gittest.Fake(t, "git version 2.56.0", `printf '%s\n' "$*" >> "$GIT_CALLS"
if [ "$1" = clone ]; then for dir in "$@"; do :; done; mkdir -p "$dir/.git"; fi
exit 0`)
	env := map[string]string{"GIT_CALLS": calls, "PATH": filepath.Dir(stub) + ":" + os.Getenv("PATH")}
	operator := []SetupStep{{Name: "operator", Commands: []string{"echo applied >> provisioned"}}}

	first := withRepos(t, launchReq("t1"), granted("web", webRepoID))
	first.Spec.Env, first.Setup = env, operator
	res, err := rig.launchVia(t.Context(), manager, first)
	if err != nil {
		t.Fatalf("the first launch: %v", err)
	}
	box, err := local.Attach(t.Context(), res.SandboxID)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	workspace := filepath.Join(box.Home(), WorkspaceSubdir)
	work := filepath.Join(workspace, "web", "notes.txt")
	if err := os.WriteFile(work, []byte("the agent's uncommitted work\n"), 0o600); err != nil {
		t.Fatalf("the agent's work: %v", err)
	}

	second := withRepos(t, launchReq("t1"), granted("web", webRepoID), granted("api", apiRepoID))
	second.Spec.Env, second.Setup = env, operator
	resumeInItsBox(t, rig)
	again, err := rig.launchVia(t.Context(), manager, second)
	if err != nil {
		t.Fatalf("the second launch: %v", err)
	}
	if !again.Reused || again.SandboxID != res.SandboxID {
		t.Fatalf("the second launch = %+v, want it on the first one's box", again)
	}

	body, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("the stand-in's calls: %v", err)
	}
	var clones []string
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if strings.HasPrefix(line, "clone ") {
			clones = append(clones, line[strings.LastIndexByte(line, ' ')+1:])
		}
	}
	if strings.Join(clones, ",") != "web,api" {
		t.Fatalf("the box cloned %q, want web on the first launch and api alone on the second", clones)
	}
	if got, err := os.ReadFile(work); err != nil || string(got) != "the agent's uncommitted work\n" {
		t.Fatalf("the agent's work in the reused checkout is %q (%v), want it as the agent left it", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "provisioned")); err != nil || string(got) != "applied\n" {
		t.Fatalf("the operator's setup left %q (%v), want it applied once, to the fresh box", got, err)
	}
	for _, repo := range []string{"web", "api"} {
		hook, err := os.Stat(filepath.Join(workspace, repo, ".git", "hooks", "prepare-commit-msg"))
		if err != nil || hook.Mode().Perm() != 0o755 {
			t.Errorf("%s's checkout has no executable commit-trailer hook: %v", repo, err)
		}
	}
}

// SETUP AND THE RUN SHARE ONE WORKING DIRECTORY. The clone step checks a
// repository out at a path relative to where it runs, and the coding agent is
// told to find it at that path from where IT starts, so the two must start in
// the same directory: every backend runs a command with no directory of its
// own in one default (the box's workspace here). A file a setup step writes
// is the job's to find.
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
	// AS A CODING AGENT'S JOB IS STARTED: in no directory of its own
	// (internal/sandbox/codingagent's Runner.Start).
	if _, err := box.StartBackground(t.Context(), "test -f marker && printf found > found", ExecOptions{}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	found := filepath.Join(box.Home(), WorkspaceSubdir, "found")
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(found)
		return err == nil
	}, "the job did not find what the setup step wrote where it started")
}
