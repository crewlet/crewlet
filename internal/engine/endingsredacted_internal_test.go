package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ENDINGS BLOCK TELLS A ROW THROUGH ITS ONE REDACTION. How a conversation's
// earlier turns ended is read off their rows and handed to the next turn's
// model, a reader like any other: what each turn was asked and every account
// of its ending are shown as Session.Redacted shows them — a credential a
// person pasted into the ask, or one a box printed into a failure or an unkept
// capture's reason, is its marker there — the head's and another ended
// turn's alike.
//
// Mutation: hand the block the row's words as the row holds them, and a
// credential reaches the next turn's brief.
func TestTheEndingsBlockTellsARowThroughItsOneRedaction(t *testing.T) {
	t.Parallel()
	const token = "ghp_Q7wErTy9UiOp3AsDfGh5JkLzXcVbNm1PqRsT"
	at := time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)
	row := func(turn string, seq uint64) executordomain.Session {
		return executordomain.Session{Handle: "swe", Turn: turn, ConversationKey: "chat:D1",
			AskExcerpt: "push the fix with " + token, EndedAt: at,
			EndedAtPosition: statelog.Position{Generation: 1, Seq: seq}, Ending: &executorkeep.Ending{
				Outcome: executorkeep.OutcomeFailed, CompletedWork: "pushed with " + token,
				BlockedOn: "the token " + token + " was refused", StopReason: "stopped over " + token,
				Failure: "git said " + token,
				Unkept:  &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 1, Reason: "the box printed " + token}}}
	}
	head, other := row("t-head", 2), row("t-other", 1)
	block, told := conversationEndings(executordomain.Head{Session: &head,
		Endings: []executordomain.Session{other}}, "swe", false, provenance.NewBoundary())
	if len(told) != 2 {
		t.Fatalf("the block told %v, want both turns", told)
	}
	if strings.Contains(block, "Q7wErTy9") || !strings.Contains(block, "[REDACTED:github-token]") {
		t.Errorf("the endings block hands the next turn a credential its rows quote:\n%s", block)
	}
}
