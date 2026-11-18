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

// A CLOSED ROUND'S CALL A TOLD CALL COULD BE TAKEN FOR IS LISTED AGAIN: a
// session that continued through earlier rounds may show their calls
// (runner's closedShown), so they are listed again first, each line noting it
// was made in an earlier round, which says where in the session it may be.

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

// A CLOSED ROUND'S CALL A TOLD CALL COULD BE TAKEN FOR IS LISTED AGAIN. Round
// 2's relaunch continues a cut-off session that may show round 1's post, and
// tells a late post of the same words: the closed post is listed again with
// the earlier-round note. A late post of other words lists nothing.
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

// THE ROUND A SEND-BACK CARRIES IS LISTED AGAIN ONCE. A send-back continues
// the session of the round it collected, which the closed rounds hold last:
// its calls are the round's own, so a held post alike a late one is listed
// once, as the round's, with no earlier-round note.
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

// A CLOSED ROUND A TASK TOLD AGAIN RENDERS IS NEVER LISTED AGAIN. A cut-off
// session may lack its job's brief, so the task is told again, rendering round
// 1 as work done: listing round 1's post too would tell one call twice and
// call an identical line a call of its own. Round 2, closed after that brief,
// is the session's own, and its post alike a late post is listed again.
//
// Mutation: list again the rounds a task told again renders, and round 1's post
// is told twice.
func TestAClosedRoundATaskToldAgainRendersIsNeverListedAgain(t *testing.T) {
	t.Parallel()
	// NO DIGEST: arguments not comparable, so each closed post reads alike every
	// told post, and their words tell the two apart.
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

// A CLOSED SUBMISSION IS NEVER LISTED AGAIN. A closed round's `submit_work`
// was sent back; listed again under words that say nothing of submissions, it
// could read as standing, while taking a told submission for it loses nothing.
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

// A CLOSED HELD CALL AN ANSWER HERE TELLS IS NEVER LISTED AGAIN. Round 1 held
// a `vendor_post` for approval and then made it, both sealed, so each reads
// alike every call of its tool. The answer, told again, tells what became of
// the call; a closed list keeps no held call's id, so neither is listed again
// beside a late `vendor_post`, by the answer's round and tool. Beside an
// answer about another tool, both are.
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
// are condensed past their share, a too-large value left out under a marker
// (execstate.EncodeWithin). Kept whole, a closed write is compared value by
// value; condensed, its content reads alike any content and its path is still
// compared.
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

// THE SUMMARIZED TELLING IS NEVER THE SHORTER WITH CLOSED CALLS LISTED AGAIN.
// The kept path hands a Claude Code relaunch whose session may be summarized
// the telling of every call from the round's start, claiming it is never the
// shorter (keptTellings.led). Closed calls alike a told call are listed again
// in both, so the summarized one's are a superset and its brief never the
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
		// RESTARTED NOWHERE since its brief, and its job's copy counted as its
		// capture is, so the count alone decides which telling is handed.
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

// THE COPY IN ITS BOX IS TOLD ITS CLOSED ROUNDS AS A KEPT SESSION IS. A copy
// carrying round 2 went through round 1, so round 1's post alike a late post
// is listed again with its note, whether its harness ended its own run or it
// was cut off. A task told again renders round 1 and tells the post there
// once. A copy whose thread restarted before its job may hold round 1 only as
// a summary, so it is told the post's tool among the earlier rounds' calls.
//
// Mutation: hand either in-box telling no closed rounds, leave out the rounds a
// task told again renders, or ignore the copy's restart, and a case goes red.
func TestTheCopyInItsBoxIsToldItsClosedRoundsAsAKeptSessionIs(t *testing.T) {
	t.Parallel()
	closedPost := ledger.Call{Name: "slack_post", Args: map[string]any{"channel": "C1", "text": "round one"},
		Result: "done"}
	history := []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{closedPost}}}
	roundOne := `- slack_post({"channel":"C1","text":"round one"}) → success`
	for name, end := range map[string]sandbox.HarnessEnd{
		"its harness ended its own run": sandbox.HarnessEndOwnRun,
		"cut off":                       sandbox.HarnessEndCutOff,
	} {
		brief := inBoxOver(t, history, runner.Resume{HarnessEnd: end,
			Bridged: []runner.BridgedCall{heldIssue, latePost("bye", 2)}}).Continue.Brief
		again, _ := sectionOf(brief, listedAgainHeading)
		if !strings.Contains(again, roundOne+" "+earlierRound) {
			t.Errorf("%s: round 1's post is not listed again with its note:\n%s", name, brief)
		}
	}

	task := sandbox.Lead{Kind: sandbox.LeadKindTask, Job: "job-1", Seq: 1, Round: 2,
		Task: &sandbox.LeadTask{Rounds: 1}}
	brief := inBoxOver(t, history, runner.Resume{Leads: []runner.ResumeLead{toldLead(task)}, Base: baseTurn,
		HarnessEnd: sandbox.HarnessEndCutOff, Bridged: []runner.BridgedCall{heldIssue, latePost("bye", 2)}}).
		Continue.Brief
	if !strings.Contains(leadsSection(brief), roundOne) || strings.Count(brief, roundOne) != 1 {
		t.Errorf("round 1's post is not told once, in the task told again:\n%s", brief)
	}

	restarted := runner.Resume{Bridged: []runner.BridgedCall{heldIssue, latePost("bye", 2)}}
	copyOf(sandbox.HarnessEndOwnRun, &sandbox.ResumedCount{Tokens: 10_000, Restart: new("r-1")})(&restarted)
	brief = inBoxOver(t, []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{post("hi", 1).Call}}},
		restarted).Continue.Brief
	if got, want := otherCallsIn(brief), otherCallsOf(earlierRoundsPart); got != want {
		t.Errorf("the copy restarted before its job is told:\n%s\nwant:\n%s\nin:\n%s", got, want, brief)
	}
}

// inBoxOver is the relaunch that carries round len(history)+1 on, over the
// turn's closed rounds history, in the asking job's own box after a person
// answered it, as resume says the job's record reads.
func inBoxOver(t *testing.T, history []ledger.Iteration, resume runner.Resume) runner.AgentRunRequest {
	t.Helper()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, len(history)+1, "", history)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	resume.State = parked.State
	resume.Asked = &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder", SessionID: "sess-1",
		BoxKept: true}
	launcher := &probingLauncher{basis: inBoxBasis}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: launcher,
		resume: &resume})
	if _, _, err := collector.Resume(t.Context(), history); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := launcher.last(t)
	if again.Continue == nil || again.Continue.SessionID != "sess-1" {
		t.Fatalf("the relaunch continues %+v in place, want sess-1 (kept %+v)", again.Continue, again.Session)
	}
	return again
}
