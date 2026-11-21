package builtin

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
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
		w    gitlet.ConversationWrite
		want types.CommittedChange
	}{
		{gitlet.ConversationWrite{Comment: &gitlet.Comment{}}, types.CommittedComment},
		{gitlet.ConversationWrite{Review: &gitlet.Review{}}, types.CommittedCode},
		{gitlet.ConversationWrite{Reviewers: &gitlet.ReviewerRequest{}}, ""},
	} {
		if got := conversed(tc.w); got != tc.want {
			t.Errorf("conversed(%+v) = %q, want %q", tc.w, got, tc.want)
		}
	}
}

// A WRITE RECORDS WHAT IT WROTE ONLY ONCE IT LANDED, NAMED AS A WAKE ABOUT IT IS
// STAMPED (ADR-0167): a change by its conversation, a post by its thread's, a
// page by its own; a write nobody heard back from records nothing, and a write
// whose change is none of the turn's outcome kinds (an ask for review) still
// records the change. A revert records the change it opened, never the one it
// reverts; an answer naming no change or room records nothing.
//
// Mutation: record on an unknown outcome, or name a change apart from
// gitlet.ChangeConversation, and this goes red.
func TestAWriteRecordsWhatItWroteOnceItLanded(t *testing.T) {
	t.Parallel()
	turn := &turnctx.Turn{Written: &turnctx.Written{}}
	commit(turn, statelog.OutcomeUnknown, types.CommittedCode, changeRecord("c-0"))
	commit(turn, "", types.CommittedChat, changeRecord("c-0"))
	if got := turn.Written.Records(); len(got) != 0 {
		t.Fatalf("writes that appended nothing recorded %v", got)
	}
	posted := chatlet.Written{Channel: chatlet.Channel{ID: "room-1", Kind: chatlet.KindPublic},
		Message: chatlet.Message{ID: "msg-5", ThreadRoot: "msg-0"}}
	commit(turn, statelog.OutcomeApplied, "", changeRecord("c-1"))
	commit(turn, statelog.OutcomePending, types.CommittedChat, chatWritten(posted))
	commit(turn, statelog.OutcomeApplied, types.CommittedPage, pageWritten("p-1"))
	commit(turn, statelog.OutcomeApplied, types.CommittedCode,
		revertWritten(gitlet.EngineWriteResult{Change: &gitlet.ChangeAnswer{ChangeID: "c-2"}}),
		revertWritten(gitlet.EngineWriteResult{}), chatWritten(chatlet.Written{}), conversing{}.wrote())
	want := []types.WrittenRecord{
		types.ConversationWritten(gitlet.ChangeConversation("c-1")),
		types.ConversationWritten(chatlet.MessageConversation(posted.Channel, posted.Message)),
		types.ConversationWritten(doclet.PageConversation("p-1")),
		types.ConversationWritten(gitlet.ChangeConversation("c-2")),
	}
	if got := turn.Written.Records(); !slices.Equal(got, want) {
		t.Errorf("recorded %v, want %v", got, want)
	}
	if got := turn.Written.Changes(); !slices.Equal(got,
		[]types.CommittedChange{types.CommittedChat, types.CommittedPage, types.CommittedCode}) {
		t.Errorf("committed %v: an ask for review's record moved the outcome", got)
	}
}
