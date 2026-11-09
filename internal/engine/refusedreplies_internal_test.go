package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE RESUMED EXECUTOR'S RECORD CARRIES ITS ROUND'S REFUSED REPLIES (ADR-0114).
// The row keeps the replies the round's questions got that were refused as too
// large to hand the run (sandbox.PendingRun.RoundRefusals), figures alone; the
// collection hands them on to the round's reviewer through the run's record,
// since none of them is in a call of the round's. A row with none hands on
// none.
//
// Mutation: leave RefusedReplies off runRecordOf, and a reviewer judging a
// round whose question was asked again after its reply was refused reads the
// two questions as one asked twice.
func TestAResumedRecordCarriesItsRoundsRefusedReplies(t *testing.T) {
	t.Parallel()
	run := pushedRun()
	run.RoundRefusals = []sandbox.RoundRefusal{
		{Job: "launch-1", Messages: 3, Bytes: 600 << 10, MaxBytes: 512 << 10},
		{Job: "launch-2", Messages: 1, Bytes: 3 << 20, MaxBytes: 512 << 10},
	}
	got := runRecordOf(run, accountOf(sandbox.ResumeRequest{Run: run}))
	want := []turn.RefusedReply{{Messages: 3, Bytes: 600 << 10, MaxBytes: 512 << 10},
		{Messages: 1, Bytes: 3 << 20, MaxBytes: 512 << 10}}
	if !slices.Equal(got.RefusedReplies, want) {
		t.Errorf("the resumed record carries %+v, want the round's refusals %+v", got.RefusedReplies, want)
	}
	if none := runRecordOf(pushedRun(), accountOf(sandbox.ResumeRequest{Run: pushedRun()})); none.RefusedReplies != nil {
		t.Errorf("a round with no refusal carries %+v", none.RefusedReplies)
	}
}
