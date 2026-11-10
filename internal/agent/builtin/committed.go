package builtin

import (
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// commit records on the turn's write record the change a write committed: a
// record appended (applied or pending), never one nobody heard back from.
func commit(turn *turnctx.Turn, outcome statelog.Outcome, change types.CommittedChange) {
	if turn == nil {
		return
	}
	switch outcome {
	case statelog.OutcomeApplied, statelog.OutcomePending:
		turn.Written.Commit(change)
	}
}

// conversed is the change a write to a change's conversation is: a comment is
// words, a review moves the change, and the rest commit nothing of the turn's.
func conversed(w code.ConversationWrite) types.CommittedChange {
	switch {
	case w.Review != nil:
		return types.CommittedCode
	case w.Comment != nil:
		return types.CommittedComment
	}
	return ""
}
