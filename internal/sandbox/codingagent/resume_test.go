package codingagent_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// A RUN THAT CONTINUES A SESSION ASKS ITS CLI FOR THAT SESSION: deep work
// parks on a proposal and its approval continues the same conversation in the
// same box, so the brief is the next message in it rather than a new task.
func TestAClaudeRunContinuesTheSessionItIsGiven(t *testing.T) {
	t.Parallel()
	cmd := scriptOf(t, claude().Command(sandbox.RunRequest{Brief: "approved", Resume: "4f1c-session"},
		codingagent.Paths{}, ""))
	if !strings.Contains(cmd, "--resume '4f1c-session'") {
		t.Fatalf("the session to continue was not passed:\n%s", cmd)
	}
	if fresh := claude().Command(sandbox.RunRequest{Brief: "x"}, codingagent.Paths{}, ""); strings.Contains(fresh, "--resume") {
		t.Fatalf("a fresh run asked to continue a session:\n%s", fresh)
	}
}

// AN OPENCODE RUN CONTINUES THE SESSION IT IS GIVEN, without the `ses_` its
// session report adds: OpenCode continues a session by being handed its id,
// so a continued run that named a fresh one would answer an approval or an
// answer in a conversation that never asked for it.
func TestAnOpenCodeRunContinuesTheSessionItIsGiven(t *testing.T) {
	t.Parallel()
	const session = "0123456789abcdef0123456789abcdef"
	cmd := codingagent.OpenCode{}.Command(sandbox.RunRequest{Brief: "approved", Resume: "ses_" + session},
		codingagent.Paths{}, "")
	if !strings.Contains(cmd, session) {
		t.Fatalf("the session to continue was not passed:\n%s", cmd)
	}
	if !(codingagent.OpenCode{}).Resumes() {
		t.Fatal("OpenCode says it cannot continue a session, so the runner refuses every continued run")
	}
}

// noResume is a CLI that cannot continue a session: Claude Code's runner with
// its Resumes hidden, as any CLI added without one would be.
type noResume struct{ codingagent.CLI }

// A CLI THAT CANNOT CONTINUE A SESSION REFUSES A RUN THAT ASKS IT TO, rather
// than start a new conversation the caller believes is the old one.
//
// Mutation: drop the resumer gate from Runner.Start, and the run starts.
func TestARunnerThatCannotResumeRefusesToPretend(t *testing.T) {
	t.Parallel()
	runner := codingagent.New(noResume{codingagent.ClaudeCode{}})
	b := box(t, runner)
	if _, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "x", Resume: "ses_1"}); err == nil {
		t.Fatal("a CLI with no way to continue a session started a run it was asked to continue one for")
	}
}
