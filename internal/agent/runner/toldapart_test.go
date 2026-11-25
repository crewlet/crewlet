package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A TOLD CALL ANOTHER CALL OF ITS BRIEF COULD BE TAKEN FOR IS MARKED. The
// words over the late calls, and over calls the session may already show, say
// a call shown twice is ONE call; where the same message tells another call a
// model could take for the told one (a line of its other list, a held call a
// lead tells, rendered work already done), one could be lost. Listing again
// is for a call only the session shows; this one is already in front of the
// model, so its line ends with a note (prompts.ToldApartNote).

// toldApart is the note a marked line ends with, after the space the ledger
// puts before a note.
const toldApart = " (a call of its own: another call this message tells could be taken for it)"

// TWO TOLD CALLS THAT COULD BE TAKEN FOR EACH OTHER ARE EACH MARKED. A cut-off
// capture saved at the round's first call: one job's post landed late, another
// post of the same words came after the save, so the brief tells one under the
// late calls' words and one under those of calls the session may show. Each
// line is marked. A post of other words reads apart, and two alike late calls
// are lines of one list, so neither is marked; a read is marked beside an alike
// read the cap shows, never one the cap left out, whose line no reader sees.
//
// Mutation: mark nothing, mark within one list, or read a partner the cap left
// out, and a case goes red.
func TestTwoToldCallsThatCouldBeTakenForEachOtherAreEachMarked(t *testing.T) {
	t.Parallel()
	lateRead := func(channel string, seq int) runner.BridgedCall {
		call := digested(numbered("slack_history", map[string]any{"channel": channel}, seq, seq))
		call.Late = true
		return call
	}
	read := func(channel string, seq int) runner.BridgedCall {
		return digested(numbered("slack_history", map[string]any{"channel": channel}, seq, seq))
	}
	var pastTheCap []runner.BridgedCall
	for n := 2; n <= 13; n++ {
		pastTheCap = append(pastTheCap, digested(readOf(n)))
	}
	pastTheCap = append(pastTheCap, read("C99", 14), lateRead("C99", 15))
	const (
		hi   = `- slack_post({"channel":"C1","text":"hi"}) → success`
		c99  = `- slack_history({"channel":"C99"}) → success (read)`
		none = 0
	)
	for name, tc := range map[string]struct {
		bridged []runner.BridgedCall
		// line is the line marked, and marked how many times it is.
		line   string
		marked int
	}{
		"a late call and an alike call after the save": {bridged: []runner.BridgedCall{
			heldIssue, post("hi", 2), latePost("hi", 3)}, line: hi, marked: 2},
		"a late call and a call after the save that reads apart": {bridged: []runner.BridgedCall{
			heldIssue, post("bye", 2), latePost("hi", 3)}, line: hi, marked: none},
		"two alike late calls": {bridged: []runner.BridgedCall{
			heldIssue, latePost("hi", 2), latePost("hi", 3)}, line: hi, marked: none},
		"a late read and an alike read the cap shows": {bridged: []runner.BridgedCall{
			heldIssue, read("C99", 2), lateRead("C99", 3)}, line: c99, marked: 2},
		"a late read and an alike read the cap left out": {bridged: append([]runner.BridgedCall{heldIssue},
			pastTheCap...), line: c99, marked: none},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			brief := lostRelaunch(t, cutOffKept(1), runner.Resume{Bridged: tc.bridged}).Session.Brief
			if !strings.Contains(brief, tc.line) {
				t.Fatalf("the brief tells no %s, so the case tests nothing:\n%s", tc.line, brief)
			}
			if got := strings.Count(brief, tc.line+toldApart); got != tc.marked ||
				strings.Count(brief, prompts.ToldApartNote) != tc.marked {
				t.Errorf("%d lines are marked, want %d:\n%s", got, tc.marked, brief)
			}
		})
	}
}

// A TOLD CALL LIKE A HELD CALL A LEAD TELLS IS MARKED. A later attempt's lead
// tells the `tr_close_issue` a person allowed once, which no list shows; a late
// `tr_close_issue` could be taken for it, so the late line is marked. Never the
// lead's own held call where nothing was approved: a fresh run carrying on a
// round whose held call was denied tells it among the round's calls, and the
// lead tells only that it was not made. Beside a late call of its tool both
// are marked, the late call being another call the message tells. A lead
// whose held call names no id still tells a call of its tool.
//
// Mutation: drop the lead partners, mark the lead's own held call, or count a
// lead only by its held call's id, and a case goes red.
func TestAToldCallLikeAHeldCallALeadTellsIsMarked(t *testing.T) {
	t.Parallel()
	lateClose := digested(numbered("tr_close_issue", map[string]any{"id": "9"}, 3, 3))
	lateClose.Late = true
	brief := laterAttemptBrief(t, cutOffKept(1), sandbox.HarnessEndCutOff, []runner.BridgedCall{
		numbered("slack_post", map[string]any{"text": "posted before the save"}, 1, 1),
		approvedFor("tr_close_issue", "held-1", 2, 2), lateClose}, nil)
	const approvedLine = "- tr_close_issue(arguments sealed for a person's approval, kept on no record) → success"
	if !strings.Contains(brief, "closed #7") || strings.Contains(brief, approvedLine) {
		t.Fatalf("the lead does not tell the approved call, or a list does:\n%s", brief)
	}
	if want := `- tr_close_issue({"id":"9"}) → success` + toldApart; !strings.Contains(brief, want) {
		t.Errorf("the late call the lead's approved call could be taken for is not marked:\n%s\nwant a line:\n%s",
			brief, want)
	}

	const heldLine = "- tr_close_issue(arguments sealed for a person's approval, kept on no record) → error: " +
		"held for a person's approval"
	denied := func(heldID string, bridged ...runner.BridgedCall) string {
		return freshRelaunch(t, runner.Resume{Bridged: bridged,
			Asked: &runner.Asked{Answer: "not now", By: "founder", SessionID: "sess-ask",
				Held: &runner.HeldCall{Tool: "tr_close_issue", Server: "tracker", Digest: "sha256:close",
					Choice: sandbox.HeldDeny, HeldID: heldID}}}, buildOpts{}).Brief
	}
	asked := numbered("slack_post", map[string]any{"text": "the asking job's post"}, 1, 1)
	brief = denied("held-1", asked, heldFor("tr_close_issue", 2, 2))
	if !strings.Contains(brief, heldLine) {
		t.Fatalf("the fresh run is not told the held call among the round's calls:\n%s", brief)
	}
	if strings.Contains(brief, prompts.ToldApartNote) {
		t.Errorf("the lead's own held call, denied, is marked as another call's like:\n%s", brief)
	}
	lateAsk := digested(numbered("tr_close_issue", map[string]any{"id": "9"}, 3, 3))
	lateAsk.Late = true
	// A FRESH RUN'S LINE OF A WRITE ENDS WITH WHAT IT RETURNED (ledger's
	// FormatOptions.Answers), and the mark after it.
	lateLine := `- tr_close_issue({"id":"9"}) → success: ` + lateAsk.Result + toldApart
	brief = denied("held-1", asked, heldFor("tr_close_issue", 2, 2), lateAsk)
	if !strings.Contains(brief, heldLine+toldApart) || !strings.Contains(brief, lateLine) {
		t.Errorf("the held call and a late call of its tool are not both marked:\n%s", brief)
	}
	// A LEAD WHOSE HELD CALL NAMES NO ID still tells a call of its tool.
	brief = denied("", asked, lateAsk)
	if !strings.Contains(brief, lateLine) {
		t.Errorf("a late call like the held call a lead naming no id tells is not marked:\n%s", brief)
	}
}

// A HELD CALL BESIDE AN EARLIER ROUND'S ANSWER OF ITS TOOL IS MARKED. A lead
// spares held calls of its own round and tool, whose fate it tells, but an
// earlier round's lead tells of another call, approved and made, that no line
// shows: a fresh run carrying round 2 on, told round 1 allowed `vendor_post`
// once, could take round 2's held `vendor_post` for it and lose it.
//
// Mutation: spare a held call from every lead of its tool, whatever its round,
// and the case goes red.
func TestAHeldCallBesideAnEarlierRoundsAnswerOfItsToolIsMarked(t *testing.T) {
	t.Parallel()
	brief := freshRelaunchOver(t, []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{post("x", 1).Call}}},
		runner.Resume{Lost: lostWithItsBox(),
			Leads: []runner.ResumeLead{{Lead: heldAnswer(), Told: true,
				Result: &sandbox.HeldResult{Output: "posted to #eng"}}},
			Bridged: []runner.BridgedCall{heldFor("vendor_post", 1, 1)}}).Brief
	const line = "- vendor_post(arguments sealed for a person's approval, kept on no record) → error: held for a" +
		" person's approval"
	if !strings.Contains(brief, line) {
		t.Fatalf("the round's held call is not told, so the case tests nothing:\n%s", brief)
	}
	if !strings.Contains(brief, line+toldApart) {
		t.Errorf("the round's held call beside an earlier round's answer of its tool is not marked:\n%s", brief)
	}
}

// A LATE CALL LIKE WORK ALREADY DONE IS MARKED. A fresh run carrying round 2
// on is told round 1 as prior work (a `→ success` ALREADY RAN) and round 2's
// late post as ONE call, made once; where round 1 posted the same words,
// either could be read as the other, so the late line is marked, not the
// prior-work line, whose render the reviewer shares. A post of other words is
// not. A round folded into a tally keeps no arguments, so a tally of the
// post's tool, or of no tool, marks it; one of another tool does not. A task
// told again renders its rounds the same way, and a late post alike one is
// marked.
//
// Mutation: drop the rendered rounds' lines, or their tallies, as partners,
// and a case goes red.
func TestALateCallLikeAWorkAlreadyDoneIsMarked(t *testing.T) {
	t.Parallel()
	lateFailed := func(text string) runner.BridgedCall {
		call := latePost(text, 1)
		call.Failed, call.Result = true, "boom"
		return call
	}
	posted := []ledger.Call{post("hi", 1).Call}
	tally := func(name string) []ledger.Elided {
		return []ledger.Elided{{Name: name, Digest: turnctx.ArgsDigest(map[string]any{"text": "folded"}),
			Calls: 2}}
	}
	for name, tc := range map[string]struct {
		round  ledger.Iteration
		late   string
		marked bool
	}{
		"a post of the same words": {round: ledger.Iteration{Iteration: 1, Calls: posted}, late: "hi",
			marked: true},
		"a post of other words": {round: ledger.Iteration{Iteration: 1, Calls: posted}, late: "bye"},
		"a tally of the post's tool": {round: ledger.Iteration{Iteration: 1, Elided: tally("slack_post")},
			late: "bye", marked: true},
		"a tally of no tool it names": {round: ledger.Iteration{Iteration: 1, Elided: tally("")}, late: "bye",
			marked: true},
		"a tally of another tool": {round: ledger.Iteration{Iteration: 1, Elided: tally("jira_get")},
			late: "bye"},
		"a span counting the post's tool": {round: ledger.Iteration{Iteration: 1, Through: 1,
			Elided: tally("slack_post")}, late: "bye", marked: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			brief := freshRelaunchOver(t, []ledger.Iteration{tc.round}, runner.Resume{Lost: lostWithItsBox(),
				Bridged: []runner.BridgedCall{lateFailed(tc.late)}}).Brief
			line := `- slack_post({"channel":"C1","text":"` + tc.late + `"}) → error: boom`
			if !strings.Contains(brief, line) {
				t.Fatalf("the fresh run is not told its late post:\n%s", brief)
			}
			if marked := strings.Contains(brief, line+toldApart); marked != tc.marked ||
				strings.Count(brief, prompts.ToldApartNote) != boolCount(tc.marked) {
				t.Errorf("the late post is marked: %t, want %t, and nothing else:\n%s", marked, tc.marked, brief)
			}
		})
	}

	// A TASK TOLD AGAIN renders round 1 as work already done, on a cut-off
	// capture of a base continuation's lineage that may lack its job's brief.
	task := sandbox.Lead{Kind: sandbox.LeadKindTask, Job: "job-1", Seq: 1, Round: 2,
		Task: &sandbox.LeadTask{Rounds: 1}}
	brief := lostRelaunchOver(t, cutOffKept(1), []ledger.Iteration{{Iteration: 1, Calls: posted}}, runner.Resume{
		Leads: []runner.ResumeLead{toldLead(task)}, Base: baseTurn,
		Bridged: []runner.BridgedCall{heldIssue, latePost("hi", 2)}}).Session.Brief
	if !strings.Contains(leadsSection(brief), `- slack_post({"channel":"C1","text":"hi"}) → success`) {
		t.Fatalf("the task told again does not render round 1's post, so the case tests nothing:\n%s", brief)
	}
	if strings.Count(brief, `- slack_post({"channel":"C1","text":"hi"}) → success`+toldApart) != 1 ||
		strings.Contains(brief, listedAgainHeading) {
		t.Errorf("the late post alike round 1's, which the task told again renders, is not marked once, or "+
			"round 1's post is listed again:\n%s", brief)
	}
}

// boolCount is 1 for true and 0 for false.
func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}
