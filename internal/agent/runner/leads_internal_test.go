package runner

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY FORM OF AN ANSWER IS TOLD AGAIN IN ITS OWN WORDS. What carried a round
// on may be a person's answer, the seat's withdrawal of its question or of a
// held call, a reply refused for its size, or a held call a person answered:
// made, denied, allowed and not made, or of an outcome not known. Its lead keeps
// which, as the relaunch builds it once the call is made ([makeHeld],
// [Asked.lead]), and a later brief to a session that may lack it tells it again
// in that form ([answerAgain]): never a refusal or a withdrawal as an answer
// (the empty answer ADR-0114 refuses to hand a run, a seat's reason as its
// answerer's words), a call denied as one whose outcome is not known, a call
// made whose result could not be read as one that may never have run, or a
// failed call as one that succeeded.
//
// Mutation: tell a withdrawal, a withdrawn call or a refused reply as an
// answer; keep the lead without its withdrawal or its refusal; keep a denial as
// an outcome not known, or tell one so; tell a call made whose result was
// unread as one whose outcome is not known; or keep a call without whether it
// failed; and a case goes red.
func TestEveryFormOfAnAnswerIsToldAgainInItsOwnWords(t *testing.T) {
	t.Parallel()
	// held is the call to `tr_close_issue` the question was about, answered
	// choice, its mark on the run's row as commit holds it.
	held := func(choice string, commit *ledgerCommit) *HeldCall {
		return &HeldCall{Tool: "tr_close_issue", Server: "tracker", Digest: "sha256:d1",
			Args: map[string]any{"id": "7"}, Choice: choice, Commit: commit}
	}
	const question = "May cto call tr_close_issue on the tracker MCP server?"
	for name, tc := range map[string]struct {
		asked Asked
		// result is what the call returned, where the brief that tells the
		// lead again opened it; nil where it did not.
		result      *sandbox.HeldResult
		says, never []string
	}{
		"an answer": {asked: Asked{Question: "which branch?", Answer: "use main", By: "founder"},
			says: []string{"### An answer to a question this work asked", "A run of this work asked: which branch?",
				"founder answered: use main"}},
		"a question the seat withdrew": {
			asked: Asked{Question: "which branch?", Answer: "the branch is settled", Withdrawn: true},
			says: []string{"### A question this work's seat withdrew", "A run of this work asked: which branch?",
				"The seat withdrew it before anybody answered: the branch is settled"},
			never: []string{"### An answer", "it asked answered"}},
		"a reply refused for its size": {
			asked: Asked{Question: "send me the log?",
				Refused: &RefusedReply{Messages: 2, Bytes: 600 << 10, MaxBytes: 512 << 10}},
			says: []string{"### A reply too large to reach this work", "A run of this work asked: send me the log?",
				"The person it asked replied, but the reply", "2 messages, 600 KiB as the engine records an answer",
				"so the engine did not take it as the answer, and the run was given none of it."},
			never: []string{"### An answer", "answered:"}},
		"a call the seat withdrew": {
			asked: Asked{Question: question, Answer: "the issue was closed by hand", Withdrawn: true,
				Held: held("", &ledgerCommit{})},
			says: []string{"### A call this work's seat withdrew",
				"A run of this work called `tr_close_issue` on the `tracker` MCP server",
				"The seat withdrew it before anybody answered: the issue was closed by hand", "The call was not made."},
			never: []string{"### A call a person answered", "denied it"}},
		"a call denied": {asked: Asked{Question: question, By: "founder", Held: held(sandbox.HeldDeny, &ledgerCommit{})},
			says: []string{"### A call a person answered", "founder denied it.",
				"The call was not made: it was refused (forbidden)."},
			never: []string{"may or may not have taken effect"}},
		"a call allowed and not made": {
			asked: Asked{Question: question, By: "founder", Held: func() *HeldCall {
				h := held(sandbox.HeldOnce, &ledgerCommit{})
				h.Unopened = "its arguments could not be opened"
				return h
			}()},
			says:  []string{"founder allowed it this once.", "The call was not made: its arguments could not be opened."},
			never: []string{"may or may not have taken effect", "ALREADY RAN"}},
		"a call whose outcome is not known": {
			asked: Asked{Question: question, By: "founder", Held: held(sandbox.HeldOnce, &ledgerCommit{marked: true})},
			says:  []string{"founder allowed it this once.", "may or may not have taken effect"},
			never: []string{"ALREADY RAN", "was not made"}},
		"a call made, its result opened again": {
			asked:  Asked{Question: question, By: "founder", Held: held(sandbox.HeldAlways, &ledgerCommit{})},
			result: &sandbox.HeldResult{Output: "closed #7"},
			says: []string{"founder allowed it, and set the tool to run without asking from now on.",
				"The call was made, and returned:", "closed #7"},
			never: []string{"cannot be read", "may or may not have taken effect"}},
		"a call made, its result not opened again": {
			asked: Asked{Question: question, By: "founder", Held: held(sandbox.HeldOnce, &ledgerCommit{})},
			says: []string{"The call was made, and the tool reported success, so it ALREADY RAN",
				"cannot be read on the node carrying this run on"},
			never: []string{"may or may not have taken effect", "reported a failure"}},
		"a call made, its result unread where it was told": {
			asked: Asked{Question: question, By: "founder", Held: held(sandbox.HeldOnce,
				&ledgerCommit{marked: true, unread: true, result: &sandbox.HeldResult{Output: "closed #7"}})},
			says: []string{"The call was made, and the tool reported success, so it ALREADY RAN",
				"cannot be read on the node carrying this run on"},
			never: []string{"may or may not have taken effect", "reported a failure"}},
		"a failed call made, its result unread where it was told": {
			asked: Asked{Question: question, By: "founder", Held: held(sandbox.HeldOnce,
				&ledgerCommit{marked: true, unread: true, result: &sandbox.HeldResult{Failed: true}})},
			says: []string{"The call was made, and the tool reported a failure.",
				"cannot be read on the node carrying this run on"},
			never: []string{"ALREADY RAN", "may or may not have taken effect"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			asked := tc.asked
			// THE CALL MADE FIRST, as the relaunch makes it before it builds
			// the lead every brief of it opens with.
			if asked.Held != nil {
				if err := makeHeld(t.Context(), heldSurface(t, &closeIssue{digest: "sha256:d1"}), asked.Held); err != nil {
					t.Fatalf("makeHeld: %v", err)
				}
			}
			lead := asked.lead()
			if lead == nil || lead.Kind != sandbox.LeadKindAnswer || lead.Answer == nil {
				t.Fatalf("the answer's lead is %+v, want an answer lead", lead)
			}
			told := answerAgain(*lead.Answer, tc.result, provenance.NewBoundary())
			for _, want := range tc.says {
				if !strings.Contains(told, want) {
					t.Errorf("told again, the lead does not say %q:\n%s", want, told)
				}
			}
			for _, never := range tc.never {
				if strings.Contains(told, never) {
					t.Errorf("told again, the lead says %q:\n%s", never, told)
				}
			}
		})
	}
}
