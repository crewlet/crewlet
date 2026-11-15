package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CLOSED ROUND'S CALL A TOLD CALL COULD BE TAKEN FOR IS LISTED AGAIN. A
// session that continued through the turn's earlier rounds may show their
// calls as well as the round's own, and a told call alike one of them could be
// taken for it as surely as for a call of the round (runner's closedShown):
// listed again first, each line saying it was made in an earlier round, which
// says where in the session it may be.

// earlierRound is the note a closed round's call listed again ends its line
// with, in a conversation that is no schedule's.
var earlierRound = prompts.EarlierRoundNote(false)

// latePost is a digested slack_post to C1 saying text, the seq'th call the
// bridge admitted and the log took, taken late.
func latePost(text string, seq int) runner.BridgedCall {
	call := post(text, seq)
	call.Late = true
	return call
}

// heldIssue is the round's first call, a jira_get the session's save point
// holds, which no told call reads alike.
var heldIssue = digested(numbered("jira_get", map[string]any{"key": "OPS-1"}, 1, 1))

// A CLOSED ROUND'S CALL A TOLD CALL COULD BE TAKEN FOR IS LISTED AGAIN. Round 1
// posted "hi" to C1; round 2's relaunch continues a session cut off, which may
// show round 1, and tells a late post of the same words. The closed post is
// listed again, its line ending with the note that it was made in an earlier
// round; a late post of other words reads apart, and nothing is listed.
//
// Mutation: list again only the round's own calls, or drop the note, and the
// first case goes red.
func TestAClosedRoundsCallAToldCallCouldBeTakenForIsListedAgain(t *testing.T) {
	t.Parallel()
	history := []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{post("hi", 1).Call}}}
	brief := lostRelaunchOver(t, cutOffKept(1), history, runner.Resume{Bridged: []runner.BridgedCall{
		heldIssue, latePost("hi", 2)}}).Session.Brief
	again, listed := sectionOf(brief, listedAgainHeading)
	want := listedAgainHeader + "\n- slack_post({\"channel\":\"C1\",\"text\":\"hi\"}) → success " + earlierRound
	if !listed || strings.TrimRight(again, "\n") != want {
		t.Errorf("the closed round's post is not listed again with its note, alone:\n%s\nwant:\n%s", brief, want)
	}

	brief = lostRelaunchOver(t, cutOffKept(1), history, runner.Resume{Bridged: []runner.BridgedCall{
		heldIssue, latePost("bye", 2)}}).Session.Brief
	if strings.Contains(brief, listedAgainHeading) {
		t.Errorf("a closed post a late post reads apart from is listed again:\n%s", brief)
	}
}

// THE ROUND A SEND-BACK CARRIES IS LISTED AGAIN ONCE. A reviewer's send-back
// continues the session of the round it collected, and the turn's closed rounds
// hold that round last: its calls are the round's own, held at the point or
// told by their rules, so a held post a late post of the round reads alike is
// listed once, as a call of the round, never again as a closed round's, and
// with no note, which would say it was made in an earlier round than the one
// just sent back.
//
// Mutation: list again the closed entry that stands for the round, and the
// post is listed twice, with the note; put the note on the round's own calls,
// and its one line carries it.
func TestTheRoundASendBackCarriesIsListedAgainOnce(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	bridged := []runner.BridgedCall{post("hi", 1), latePost("hi", 2)}
	relauncher := &probingLauncher{basis: keptBasis}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		resume: &runner.Resume{State: parked.State, Bridged: bridged, Answer: "posted"}})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	history := []ledger.Iteration{{Iteration: 1, Calls: ledgerOf(bridged), ReviewNotes: "post it to C2"}}
	if _, _, err := collector.Execute(t.Context(), 2, "post it to C2", history); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	again := relauncher.last(t)
	if again.Session == nil {
		t.Fatal("the send-back continues no kept session")
	}
	section, listed := sectionOf(again.Session.Brief, listedAgainHeading)
	if want := listedAgainSection; !listed || strings.TrimRight(section, "\n") != want {
		t.Errorf("the send-back does not list the held post again once, as the round's own:\n%s\nwant:\n%s",
			again.Session.Brief, want)
	}
}

// A CLOSED ROUND A TASK TOLD AGAIN RENDERS IS NEVER LISTED AGAIN. A session cut
// off may lack its job's brief, which continued the session the conversation's
// last turn left and told the turn's task with round 1 as work already done:
// that task is told again, rendering round 1 again, so round 1's post is told
// there and never listed again as well, which would tell one call twice in one
// message and point the words over it at an identical line as a call of its
// own. Round 2, closed after that brief, is the session's own, and its post a
// late post could be taken for is listed again.
//
// Mutation: list again the rounds a task told again renders, and round 1's post
// is told twice.
func TestAClosedRoundATaskToldAgainRendersIsNeverListedAgain(t *testing.T) {
	t.Parallel()
	// NO DIGEST, as a build before one recorded them: arguments not comparable,
	// so each closed post reads alike every told post, and the two are told
	// apart by their words.
	closedPost := func(text string) ledger.Call {
		return ledger.Call{Name: "slack_post", Args: map[string]any{"channel": "C1", "text": text}, Result: "done"}
	}
	history := []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{closedPost("round one")}},
		{Iteration: 2, Calls: []ledger.Call{closedPost("round two")}}}
	task := sandbox.Lead{Kind: sandbox.LeadKindTask, Job: "job-1", Seq: 1, Round: 2,
		Task: &sandbox.LeadTask{Rounds: 1}}
	brief := lostRelaunchOver(t, cutOffKept(1), history, runner.Resume{
		Leads: []runner.ResumeLead{toldLead(task)}, Base: baseTurn,
		Bridged: []runner.BridgedCall{heldIssue, latePost("bye", 2)}}).Session.Brief
	roundOne := `- slack_post({"channel":"C1","text":"round one"}) → success`
	if !strings.Contains(leadsSection(brief), roundOne) || strings.Count(brief, roundOne) != 1 {
		t.Errorf("round 1's post is not told once, in the task told again:\n%s", brief)
	}
	again, _ := sectionOf(brief, listedAgainHeading)
	if want := `- slack_post({"channel":"C1","text":"round two"}) → success ` + earlierRound; !strings.Contains(
		again, want) || strings.Contains(again, "round one") {
		t.Errorf("round 2's post alone is not listed again, with its note:\n%s", brief)
	}
}

// A CLOSED SUBMISSION IS NEVER LISTED AGAIN. A closed round's `submit_work` was
// sent back, and a told submission is said not to stand; taking one for the
// other loses nothing, while listed again under words that say nothing of
// submissions, the closed one could read as standing.
//
// Mutation: list a closed submission again, and its line is in the section.
func TestAClosedSubmissionIsNeverListedAgain(t *testing.T) {
	t.Parallel()
	submission := func(summary string) ledger.Call {
		return ledger.Call{Name: runner.SubmitWorkTool, Args: map[string]any{"summary": summary}, Result: "submitted"}
	}
	late := digested(runner.BridgedCall{Call: submission("the second pass"), Seq: 2, Appended: 2})
	late.Late = true
	brief := lostRelaunchOver(t, cutOffKept(1), []ledger.Iteration{
		{Iteration: 1, Calls: []ledger.Call{submission("the first pass")}}},
		runner.Resume{Bridged: []runner.BridgedCall{heldIssue, late}}).Session.Brief
	if !strings.Contains(brief, `"the second pass"`) {
		t.Fatalf("the late submission is not told, so the case tests nothing:\n%s", brief)
	}
	if strings.Contains(brief, listedAgainHeading) || strings.Contains(brief, "the first pass") {
		t.Errorf("a closed submission is listed again:\n%s", brief)
	}
}

// A CLOSED HELD CALL AN ANSWER HERE TELLS IS NEVER LISTED AGAIN. Round 1's run
// held a `vendor_post` for a person's approval, who allowed it, and the call
// was made: both calls are on round 1's record, their arguments sealed, so
// each reads alike every call of its tool. The answer is told again here, and
// it tells what became of the call; a closed list keeps no held call's id, so
// neither withheld call is listed again beside a late `vendor_post`, by the
// answer's round and tool. Where the answer is about another tool, both are.
//
// Mutation: list again a withheld call of the round and tool an answer of the
// brief tells, and the first case lists both.
func TestAClosedHeldCallAnAnswerHereTellsIsNeverListedAgain(t *testing.T) {
	t.Parallel()
	late := digested(numbered("vendor_post", map[string]any{"body": "again"}, 2, 2))
	late.Late = true
	history := []ledger.Iteration{{Iteration: 1, Calls: roundHeldCalls(true)}}
	relaunch := func(answer sandbox.Lead) string {
		return lostRelaunchOver(t, cutOffKept(1), history, runner.Resume{
			Leads:   []runner.ResumeLead{{Lead: answer, Told: true}},
			Bridged: []runner.BridgedCall{heldIssue, late}}).Session.Brief
	}
	brief := relaunch(heldAnswer())
	if !strings.Contains(leadsSection(brief), "vendor_post") {
		t.Fatalf("the answer is not told again, so the case tests nothing:\n%s", brief)
	}
	if strings.Contains(brief, listedAgainHeading) {
		t.Errorf("a withheld call the answer told again tells of is listed again:\n%s", brief)
	}

	other := heldAnswer()
	other.Answer.Held.Tool = "tr_close_issue"
	again, _ := sectionOf(relaunch(other), listedAgainHeading)
	if n := strings.Count(again, "- vendor_post("); n != 2 {
		t.Errorf("beside an answer about another tool, %d withheld calls are listed again, want both:\n%s", n,
			again)
	}
}

// A CLOSED CALL IS COMPARED AS A PARK KEEPS IT. A parked turn's closed rounds
// ride its row, condensed past their share: a value too large is left out in
// place, its marker saying so (execstate.EncodeWithin). Decoded back, a closed
// write kept whole reads alike a told write of the same values and apart from
// one whose content or path its line shows differs; condensed, its content
// reads alike any content, and its path, kept whole, is still compared.
//
// Mutation: compare a marker as the text it is, and the condensed write reads
// apart from a told write of the same content.
func TestAClosedCallIsComparedAsAParkKeepsIt(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("c", 150)
	write := func(path, content string) map[string]any { return map[string]any{"path": path, "content": content} }
	closed := digested(numbered("write_file", write("notes.md", content), 1, 1)).Call
	state := execstate.State{Iterations: []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{closed}}}}
	full, err := execstate.Encode(state)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	parked := func(budget int) []ledger.Iteration {
		blob, condensed, err := execstate.EncodeWithin(state, budget)
		if err != nil {
			t.Fatalf("EncodeWithin(%d): %v", budget, err)
		}
		back, _, err := execstate.Decode(blob)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if leftOut := kept.IsLeftOut(back.Iterations[0].Calls[0].Args["content"]); leftOut != condensed.Any() {
			t.Fatalf("the park at %d left the content out: %t, condensed: %+v", budget, leftOut, condensed)
		}
		return back.Iterations
	}
	for name, tc := range map[string]struct {
		budget int
		told   map[string]any
		listed bool
	}{
		"kept whole, the same values":       {budget: len(full), told: write("notes.md", content), listed: true},
		"kept whole, another content":       {budget: len(full), told: write("notes.md", "short")},
		"kept whole, another path":          {budget: len(full), told: write("todo.md", content)},
		"content left out, the same values": {budget: len(full) - 1, told: write("notes.md", content), listed: true},
		"content left out, another content": {budget: len(full) - 1, told: write("notes.md", "short"), listed: true},
		"content left out, another path":    {budget: len(full) - 1, told: write("todo.md", content)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			late := digested(numbered("write_file", tc.told, 2, 2))
			late.Late = true
			brief := lostRelaunchOver(t, cutOffKept(1), parked(tc.budget), runner.Resume{
				Bridged: []runner.BridgedCall{late}}).Session.Brief
			again, listed := sectionOf(brief, listedAgainHeading)
			if listed != tc.listed || listed && !strings.Contains(again, `"path":"notes.md"`) {
				t.Errorf("the closed write is listed again: %t, want %t:\n%s", listed, tc.listed, brief)
			}
		})
	}
}

// THE SUMMARIZED TELLING IS NEVER THE SHORTER WITH CLOSED CALLS LISTED AGAIN. A
// Claude Code relaunch whose session its harness may hand its model as a
// summary is told every call of the round from its start; the kept path hands
// that telling wherever the held one may compact, on the claim that it is
// never the shorter (keptTellings.led). Closed calls alike a told call are
// listed again in both, value by value, so the summarized telling's are a
// superset of the held one's, and its brief, for either box, is never the
// shorter.
//
// Mutation: list closed calls again only in the held telling, and the
// summarized brief is the shorter.
func TestTheSummarizedTellingIsNeverTheShorterWithClosedCallsListedAgain(t *testing.T) {
	t.Parallel()
	var calls []ledger.Call
	for _, text := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight"} {
		calls = append(calls, ledger.Call{Name: "slack_post", Result: "done",
			Args: map[string]any{"channel": "C1", "text": "the closed round's " + text + " post, in words of its own"}})
	}
	history := []ledger.Iteration{{Iteration: 1, Calls: calls}}
	relaunch := func(count *sandbox.ResumedCount) *runner.SessionContinuation {
		// RESTARTED NOWHERE since its brief, as its launch handed it, and its
		// job's copy counted as its capture is, so the count alone decides which
		// telling is handed.
		return lostRelaunchOver(t, keptClaude(count, false), history, runner.Resume{BasisRestart: new(""),
			Count:   count,
			Bridged: []runner.BridgedCall{heldIssue, latePost("bye", 2)}}).Session
	}
	held, summed := relaunch(far), relaunch(near)
	if !strings.Contains(held.Brief, earlierRound) || strings.Contains(held.Brief, prompts.MayCompactSession) {
		t.Fatalf("the held telling does not list the closed posts again as one held:\n%s", held.Brief)
	}
	if !strings.Contains(summed.Brief, prompts.MayCompactSession) {
		t.Fatalf("the summarized telling is not handed:\n%s", summed.Brief)
	}
	if len(summed.Brief) < len(held.Brief) || len(summed.Replaced) < len(held.Replaced) {
		t.Errorf("the summarized telling is the shorter: %d and %d bytes against the held one's %d and %d",
			len(summed.Brief), len(summed.Replaced), len(held.Brief), len(held.Replaced))
	}
}
