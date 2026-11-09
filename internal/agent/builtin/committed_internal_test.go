package builtin

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REFUSED OR FAILED WRITE IS NOT MOVED: only a record the write appended
// (applied or pending) counts toward the turn's outcome, never one nobody heard
// back from, and a write outside a turn records nothing.
func TestARefusedOrFailedWriteIsNotMoved(t *testing.T) {
	t.Parallel()
	turn := &turnctx.Turn{Written: &turnctx.Written{}}
	commit(turn, statelog.OutcomeUnknown, types.CommittedItem)
	commit(turn, statelog.OutcomeUnknown, types.CommittedPage)
	commit(turn, "", types.CommittedCode)
	if got := turn.Written.Outcome(); got != types.OutcomeNothing {
		t.Fatalf("outcome %q after writes that appended nothing, want nothing", got)
	}
	commit(turn, statelog.OutcomePending, types.CommittedChat)
	commit(turn, statelog.OutcomeApplied, types.CommittedPage)
	if got := turn.Written.Changes(); !slices.Equal(got,
		[]types.CommittedChange{types.CommittedChat, types.CommittedPage}) {
		t.Fatalf("committed %v, want the chat post and the page save", got)
	}
	commit(nil, statelog.OutcomeApplied, types.CommittedItem)
}

// A WRITE TO A CHANGE'S CONVERSATION IS WORDS WHEN IT COMMENTS AND MOVES THE
// CHANGE WHEN IT REVIEWS; a reviewer request commits nothing of the turn's.
func TestAChangeConversationWriteIsClassedByWhatItWrites(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		w    code.ConversationWrite
		want types.CommittedChange
	}{
		{code.ConversationWrite{Comment: &code.Comment{}}, types.CommittedComment},
		{code.ConversationWrite{Review: &code.Review{}}, types.CommittedCode},
		{code.ConversationWrite{Reviewers: &code.ReviewerRequest{}}, ""},
	} {
		if got := conversed(tc.w); got != tc.want {
			t.Errorf("conversed(%+v) = %q, want %q", tc.w, got, tc.want)
		}
	}
}
