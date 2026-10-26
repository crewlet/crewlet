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
	cmd := claude().Command(sandbox.RunRequest{Brief: "approved", Resume: "4f1c-session"},
		codingagent.Paths{}, "")
	if !strings.Contains(cmd, "--resume '4f1c-session'") {
		t.Fatalf("the session to continue was not passed:\n%s", cmd)
	}
	if fresh := claude().Command(sandbox.RunRequest{Brief: "x"}, codingagent.Paths{}, ""); strings.Contains(fresh, "--resume") {
		t.Fatalf("a fresh run asked to continue a session:\n%s", fresh)
	}
}

// A CLI THAT CANNOT CONTINUE A SESSION REFUSES A RUN THAT ASKS IT TO, rather
// than start a new conversation the caller believes is the old one.
func TestARunnerThatCannotResumeRefusesToPretend(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	if _, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "x", Resume: "ses_1"}); err == nil {
		t.Fatal("OpenCode started a run it was asked to continue a session for")
	}
}
