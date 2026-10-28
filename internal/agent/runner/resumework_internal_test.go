package runner

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/tools"
)

// A RESUMED PHASE'S RECORD IS WHOLE, AND SAYS WHICH OF IT IS THIS ATTEMPT'S.
// The parked round's calls lead the record, because the delivery check and the
// ledger read the whole phase, and are marked as an earlier attempt's, because
// whether a retry would repeat a write is a question about this attempt alone
// ([turn.Work.Inherited]).
//
// Mutation: leave Inherited at zero, and the parked post reads as this
// attempt's.
func TestAResumedRecordMarksTheParkedRoundsCallsAsInherited(t *testing.T) {
	t.Parallel()
	state := execstate.State{ToolExecutions: []map[string]any{
		{"name": "slack_post", "arguments": "{}", "success": true},
		{"name": "run_sandbox", "arguments": "{}", "success": true},
	}}
	work := resumedWork(turn.Work{Text: "x"}, &tools.Surface{}, state)
	if len(work.Calls) != 2 || work.Inherited != 2 || work.Text != "x" {
		t.Fatalf("resumed work = %+v, want both parked calls, both inherited, the text kept", work)
	}
}
