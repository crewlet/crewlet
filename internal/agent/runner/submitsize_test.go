package runner_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// A SUBMISSION A BYTE PAST WHAT THE LOG KEEPS IS REFUSED NAMING A SIZE PAST THE
// BOUND. The refusal states what the submission weighs as the run's record
// holds it beside the bound, in kept.Size's units, which round whole KiB down:
// a submission a byte past 32 KiB stated in the same units read "this
// submission is 32 KiB ..., and the record keeps a submission whole up to
// 32 KiB", which tells the run nothing was wrong with it. Rounded up
// (kept.SizeUp), it reads past.
//
// Mutation: state the submission's size in kept.Size, and it reads as the
// bound itself.
func TestASubmissionAByteTooLargeIsRefusedNamingASizePastItsBound(t *testing.T) {
	t.Parallel()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{})
	reopened, err := r.ReopenAgentRun(t.Context(), execstate.State{Version: execstate.Version, Round: 1},
		nil, nil, nil, nil, runner.Bridged{}, false)
	if err != nil {
		t.Fatalf("ReopenAgentRun: %v", err)
	}
	// AS THE TOOL WEIGHS IT: its arguments as decoded off the wire, as the
	// engine records them, as a JSON string.
	argsOf := func(submission map[string]any) (map[string]any, int) {
		t.Helper()
		raw, err := json.Marshal(submission)
		if err != nil {
			t.Fatal(err)
		}
		var args map[string]any
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatal(err)
		}
		text, err := tools.RecordArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		return args, kept.Quoted(text)
	}
	_, empty := argsOf(submissionOf(0))
	args, weighs := argsOf(submissionOf(sandbox.MaxSubmissionBytes + 1 - empty))
	if weighs != sandbox.MaxSubmissionBytes+1 {
		t.Fatalf("the premise: a submission a byte past the bound, not %d bytes", weighs)
	}
	res, err := reopened.Surface.Execute(t.Context(), llm.ToolCall{Name: runner.SubmitWorkTool, Arguments: args})
	if err != nil {
		t.Fatalf("submit_work: %v", err)
	}
	want := "this submission is 33 KiB as the run's record holds it, and the record keeps a submission " +
		"whole up to 32 KiB"
	if !res.Failed || !strings.Contains(res.Output, want) {
		t.Errorf("a submission a byte past the bound answered %+v, want it refused saying %q", res, want)
	}
}
