package builtin

import (
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// commit records on the turn's write record the change a write committed and
// the records it created or wrote, each named as a wake about it names it
// ([types.WrittenRecord]), which a running run's router reads (ADR-0167): a
// record appended (applied or pending), never one nobody heard back from.
func commit(turn *turnctx.Turn, outcome statelog.Outcome, change types.CommittedChange,
	wrote ...types.WrittenRecord) {

	if turn == nil {
		return
	}
	switch outcome {
	case statelog.OutcomeApplied, statelog.OutcomePending:
		turn.Written.Commit(change)
		turn.Written.Wrote(wrote...)
	}
}

// changeRecord is the record a write to change id wrote, named as every wake
// about the change is stamped ([gitlet.ChangeConversation]).
func changeRecord(id string) types.WrittenRecord {
	return types.ConversationWritten(gitlet.ChangeConversation(id))
}

// conversed is the change a write to a change's conversation is: a comment is
// words, a review moves the change, and the rest commit nothing of the turn's.
func conversed(w gitlet.ConversationWrite) types.CommittedChange {
	switch {
	case w.Review != nil:
		return types.CommittedCode
	case w.Comment != nil:
		return types.CommittedComment
	}
	return ""
}
