package runner

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY FORM OF AN ANSWER IS TOLD AGAIN IN ITS OWN WORDS: a person's answer,
// the seat's withdrawal of its question or held call, a reply refused for its
// size, or a held call made, denied, allowed and not made, or of unknown
// outcome. The lead keeps which, built once the call is made ([makeHeld],
// [Asked.lead]), and [answerAgain] tells it so: never a refusal or withdrawal as
// an answer (ADR-0114's empty answer; a seat's reason as its answerer's words),
// a denial as unknown, a made call with an unread result as maybe never run, or
// a failed call as a success.
//
// Mutation: tell a withdrawal, withdrawn call or refused reply as an answer;
// drop the withdrawal or refusal from the lead; keep or tell a denial as
// unknown; tell an unread made call as unknown; or drop whether it failed: red.
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
			told := answerAgain(lead.Kind, *lead.Answer, tc.result, answerAt{}, provenance.NewBoundary())
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

// AN ANSWER POINTS ONLY TO A ROUND ITS BRIEF LISTS CALL BY CALL ([heldIn]): one
// kept whole, listing a sealed call of the tool, with nothing that could count
// the call instead. Never a span a park dropped (ledger.Iteration.Through), even
// of one round, nor a round whose tally counts its tool or no tool, or of which
// the tool is a read: its answer tells the outcome as one of that entry's
// calls, and one whose round holds no call of the tool tells it plainly.
//
// Mutation: take a span for a round; leave a tally of the tool or of no tool,
// or the tool's being a read, out of what may count the call; point to a round
// with no call of the tool; read only a span's first round; count every tally
// whatever its tools; or tell the outcome without saying it is one of the
// entry's calls, or name another entry, and a case goes red.
func TestAnAnswerPointsOnlyToARoundItsBriefListsCallByCall(t *testing.T) {
	t.Parallel()
	answer := ResumeLead{Lead: sandbox.Lead{Kind: sandbox.LeadKindAnswer, Job: "job-2", Seq: 2, Round: 2,
		Answer: &sandbox.LeadAnswer{Question: "May swe call vendor_post?", By: "founder",
			Held: &sandbox.LeadHeld{Tool: "vendor_post", Server: "vendor", HeldID: "held-1", Choice: sandbox.HeldOnce,
				Outcome: sandbox.LeadHeldMade}}},
		Result: &sandbox.HeldResult{Output: "posted to #eng"}}
	// approved is round 2's call a person let be made, its arguments sealed on
	// every record, as the round's list draws it.
	approved := []ledger.Call{{Name: "read_page", Result: "the page"},
		{Name: "vendor_post", Result: "posted to #eng", Withheld: true}}
	spanned := []ledger.Elided{{Name: "vendor_post", Calls: 2}}
	for name, tc := range map[string]struct {
		listed []ledger.Iteration
		// pointed is the answer pointing to round 2's list; counted, the
		// entry it is said to be one of, "" for none.
		pointed bool
		counted string
	}{
		"a round kept whole": {listed: []ledger.Iteration{{Iteration: 1}, {Iteration: 2, Calls: approved}},
			pointed: true},
		"a round whose record dropped only other tools' calls": {listed: []ledger.Iteration{{Iteration: 1},
			{Iteration: 2, Calls: approved, Elided: []ledger.Elided{{Name: "read_page", Calls: 90}}}}, pointed: true},
		"a span of rounds a park dropped, the answer's its first": {
			listed:  []ledger.Iteration{{Iteration: 1}, {Iteration: 2, Through: 4, Elided: spanned}},
			counted: prompts.HeldCallCounted(2, 2, 4)},
		"a round inside a span": {listed: []ledger.Iteration{{Iteration: 1, Through: 4, Elided: spanned}},
			counted: prompts.HeldCallCounted(2, 1, 4)},
		"a span of the answer's round alone": {
			listed:  []ledger.Iteration{{Iteration: 1}, {Iteration: 2, Through: 2, Elided: spanned}},
			counted: prompts.HeldCallCounted(2, 2, 2)},
		"a span whose tally counts no call of its tool": {listed: []ledger.Iteration{{Iteration: 1},
			{Iteration: 2, Through: 3, Elided: []ledger.Elided{{Name: "read_page", Calls: 3}}}},
			counted: prompts.HeldCallCounted(2, 2, 3)},
		"a round whose calls a park folded into its tally": {listed: []ledger.Iteration{{Iteration: 1},
			{Iteration: 2, Elided: []ledger.Elided{{Name: "vendor_post", Calls: 3, Failed: 1}}}},
			counted: prompts.HeldCallCounted(2, 2, 0)},
		"a round whose record dropped calls of its tool": {listed: []ledger.Iteration{{Iteration: 1},
			{Iteration: 2, Calls: approved, Elided: []ledger.Elided{{Name: "vendor_post", Calls: 90}}}},
			counted: prompts.HeldCallCounted(2, 2, 0)},
		"a round whose tally names no tool": {listed: []ledger.Iteration{{Iteration: 1},
			{Iteration: 2, Calls: approved, Elided: []ledger.Elided{{Calls: 4}}}},
			counted: prompts.HeldCallCounted(2, 2, 0)},
		"a round of which its tool is a read": {listed: []ledger.Iteration{{Iteration: 1},
			{Iteration: 2, Calls: approved, Reads: []string{"read_page", "vendor_post"}}},
			counted: prompts.HeldCallCounted(2, 2, 0)},
		"a round that lists no call of its tool": {listed: []ledger.Iteration{{Iteration: 1},
			{Iteration: 2, Calls: approved[:1]}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			section := earlierAnswers([]ResumeLead{answer}, 5, tc.listed, 0, true, provenance.NewBoundary()).section.Text
			if pointed := strings.Contains(section, prompts.HeldCallListed(2)); pointed != tc.pointed {
				t.Errorf("the answer points to round 2's list: %v, want %v:\n%s", pointed, tc.pointed, section)
			}
			if told := strings.Contains(section, "The call was made, and returned:") &&
				strings.Contains(section, "posted to #eng"); told == tc.pointed {
				t.Errorf("the answer tells what its call returned: %v, want %v:\n%s", told, !tc.pointed, section)
			}
			counted := strings.Contains(section, "may count rather than list")
			if counted != (tc.counted != "") || (counted && !strings.Contains(section, tc.counted)) {
				t.Errorf("the answer says its call is one of an entry's calls: %v, want %q:\n%s", counted,
					tc.counted, section)
			}
		})
	}
}

// A TASK LEAD KEEPS THE LAST ROUND ITS BRIEF RENDERED, BY ITS NUMBER: a park
// folds closed rounds into spans (ledger.Iteration.Through), so entries can be
// fewer than rounds; the number survives a later park's folds, and the task
// told again renders every entry up to it (roundsThrough).
//
// Mutation: keep how many entries the brief rendered, or a span's first round
// for its last, and a case goes red.
func TestATaskLeadKeepsTheLastRoundItsBriefRenderedByItsNumber(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		history []ledger.Iteration
		rounds  int
	}{
		"no closed round":   {},
		"rounds kept whole": {history: []ledger.Iteration{{Iteration: 1}, {Iteration: 2}}, rounds: 2},
		"rounds a park folded": {history: []ledger.Iteration{{Iteration: 1}, {Iteration: 2, Through: 5}, {Iteration: 6}},
			rounds: 6},
		"every round in one span": {history: []ledger.Iteration{{Iteration: 1, Through: 6}}, rounds: 6},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			leads := taskLead(len(tc.history)+1, tc.history, "")
			if len(leads) != 1 || leads[0].Task == nil {
				t.Fatalf("the brief keeps %+v, want one task lead", leads)
			}
			if got := leads[0].Task.Rounds; got != tc.rounds {
				t.Errorf("the task lead keeps round %d as the last its brief rendered, want %d", got, tc.rounds)
			}
		})
	}
}
