package codingagent_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// The ask signal is the one part of the protocol that runs INSIDE the box as a
// script the engine wrote, so it needs a real shell to prove anything.
func TestTheAskShimRecordsAQuestionFromInsideARealBox(t *testing.T) {
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
	p := codingagent.PathsFor(box)

	// Exactly as the launch wrapper does it: the shim directory prepended
	// to PATH for the agent's own command, inherited by its children.
	res, err := box.Exec(t.Context(),
		`PATH='`+p.BinDir()+`':"$PATH" sh -c 'crewlet-ask "which branch?" --to requester'`,
		sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("the shim exited %d: %s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	blob, err := box.ReadFile(t.Context(), p.Ask())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(blob), `"question":"which branch?"`) {
		t.Fatalf("ask.json = %q", blob)
	}
	if !strings.Contains(string(blob), `"to":"requester"`) {
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
