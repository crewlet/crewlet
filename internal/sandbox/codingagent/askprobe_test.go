package codingagent_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// realBox is a local box on the engine host with the runner's plumbing — the
// ask shim among it — installed, as a launch leaves one.
func realBox(t *testing.T) (*codingagent.Runner, sandbox.Sandbox) {
	t.Helper()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{
		Placement: sandbox.Direct, StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })

	runner := codingagent.NewClaudeCode()
	if err = runner.Install(t.Context(), box); err != nil {
		t.Fatalf("Install: %v", err)
	}
	return runner, box
}

// inBox runs cmd in the box's checkout, as the coding agent's own shell
// would: the shim directory on PATH, exactly as the launch wrapper puts it
// there for the agent's command and its children.
func inBox(t *testing.T, box sandbox.Sandbox, cmd string) {
	t.Helper()
	res, err := box.Exec(t.Context(),
		`PATH='`+codingagent.PathsFor(box).BinDir()+`':"$PATH"; `+cmd, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec %q: %v", cmd, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("%q exited %d: %s%s", cmd, res.ExitCode, res.Stdout, res.Stderr)
	}
}

// askFile is what the shim recorded.
func askFile(t *testing.T, box sandbox.Sandbox) string {
	t.Helper()
	blob, err := box.ReadFile(t.Context(), codingagent.PathsFor(box).Ask())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(blob)
}

// The ask signal is the one part of the protocol that runs INSIDE the box as a
// script the engine wrote, so it needs a real shell to prove anything.
func TestTheAskShimRecordsAQuestionFromInsideARealBox(t *testing.T) {
	t.Parallel()
	runner, box := realBox(t)
	inBox(t, box, `sh -c 'crewlet-ask "which branch?" --to requester'`)

	blob := askFile(t, box)
	if !strings.Contains(blob, `"question":"which branch?"`) {
		t.Fatalf("ask.json = %q", blob)
	}
	if !strings.Contains(blob, `"to":"requester"`) {
		t.Fatalf("ask.json = %q", blob)
	}

	// And the runner reads it back as a parked run.
	result, err := runner.Collect(t.Context(), box, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !result.NeedsInput || result.Question != "which branch?" {
		t.Fatalf("Collect = %+v, want the run parked on its question", result)
	}
}

// THE BRANCH THE RUN PUSHED IS RECORDED WITH ITS QUESTION, because it is what
// a run re-seeded on a fresh machine starts from — and the only thing that
// knows it is the agent, at the moment it asks. Named with --branch, it needs
// no git in the box at all.
func TestTheAskShimRecordsTheBranchItIsGiven(t *testing.T) {
	t.Parallel()
	for name, flag := range map[string]string{
		"as two words": "--branch wip/named",
		"as one":       "--branch=wip/named",
		"before --to":  "--branch wip/named --to requester",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := realBox(t)
			inBox(t, box, `crewlet-ask "which branch?" `+flag)

			if blob := askFile(t, box); !strings.Contains(blob, `"branch":"wip/named"`) {
				t.Fatalf("ask.json = %q", blob)
			}
			result, err := runner.Collect(t.Context(), box, sandbox.RunHandle{})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if !result.NeedsInput || result.Question != "which branch?" || result.WIPBranch != "wip/named" {
				t.Fatalf("Collect = question %q, branch %q; want the question and the branch it named",
					result.Question, result.WIPBranch)
			}
		})
	}
}

// AND WHERE THE AGENT NAMES NONE, the shim asks git, from where the agent
// asked: the branch its upstream names on the remote — the name it was pushed
// under — or the branch it is on, or nothing on a detached HEAD or outside a
// repository, which leaves the re-seed to say so in general terms rather than
// name something that is not a branch. A name the agent gives wins.
//
// NEEDS GIT ON THE ENGINE HOST: the shim runs in the box, and a direct box is
// the host. The shim's whole point is that git is optional in the box, which
// TestTheAskShimRecordsTheBranchItIsGiven proves with no git involved.
func TestTheAskShimRecordsTheBranchItWasRunOn(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH: the shim's own reading of the branch needs a repository to read")
	}
	const repo = `git init -q && git symbolic-ref HEAD refs/heads/wip/t1 && `
	for name, tc := range map[string]struct {
		setup, ask, want string
	}{
		"the branch it is on": {
			setup: repo + `true`,
			ask:   `crewlet-ask "which branch?"`, want: "wip/t1",
		},
		"the name its upstream has on the remote": {
			setup: repo + `git config branch.wip/t1.remote origin && ` +
				`git config branch.wip/t1.merge refs/heads/wip/pushed`,
			ask: `crewlet-ask "which branch?"`, want: "wip/pushed",
		},
		"not a local upstream": {
			setup: repo + `git config branch.wip/t1.remote . && ` +
				`git config branch.wip/t1.merge refs/heads/main`,
			ask: `crewlet-ask "which branch?"`, want: "wip/t1",
		},
		"a detached HEAD": {
			setup: repo + `git -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m t && ` +
				`git checkout -q --detach`,
			ask: `crewlet-ask "which branch?"`, want: "",
		},
		"outside a repository": {
			setup: `true`,
			ask:   `crewlet-ask "which branch?"`, want: "",
		},
		"a name the agent gives": {
			setup: repo + `true`,
			ask:   `crewlet-ask "which branch?" --branch wip/given`, want: "wip/given",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := realBox(t)
			inBox(t, box, tc.setup)
			inBox(t, box, tc.ask)

			if blob := askFile(t, box); !strings.Contains(blob, `"branch":"`+tc.want+`"`) {
				t.Fatalf("ask.json = %q, want the branch %q", blob, tc.want)
			}
			result, err := runner.Collect(t.Context(), box, sandbox.RunHandle{})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if !result.NeedsInput || result.WIPBranch != tc.want {
				t.Fatalf("Collect = needs input %v, branch %q; want %q",
					result.NeedsInput, result.WIPBranch, tc.want)
			}
		})
	}
}
