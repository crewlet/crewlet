package runner_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT A CONTINUED SESSION IS TOLD OF THE CALLS AFTER ITS SAVE is read against
// its save point ([runner.SessionBasis.Through]): how many of the round's calls
// the log had taken at the read the capture was placed by (its own, or an
// earlier capture's where its harness may not have written every call). Every
// call the log took after it ([runner.BridgedCall.Appended] past it) may be one
// the session was not answered.

// afterSaveBrief is the brief a lost job's round is carried on with,
// continuing the job's kept session saved at through, where the round's log
// lists bridged and counts elided without listing them.
func afterSaveBrief(t *testing.T, through int, bridged []runner.BridgedCall, elided []ledger.Elided) string {
	t.Helper()
	basis := keptBasis
	basis.Through = through
	return lostRelaunch(t, basis, runner.Resume{Bridged: bridged, Elided: elided}).Session.Brief
}

// lostRelaunch is the request a lost job's round is carried on with, continuing
// the kept session basis names, where the round's log is as round says of it —
// its parked state and its loss the case's own.
func lostRelaunch(t *testing.T, basis runner.SessionBasis, round runner.Resume) runner.AgentRunRequest {
	t.Helper()
	return lostRelaunchOver(t, basis, nil, round)
}

// lostRelaunchOver is [lostRelaunch] of round len(history)+1, its run launched
// over the turn's closed rounds history, as [freshRelaunchOver]'s is.
func lostRelaunchOver(t *testing.T, basis runner.SessionBasis, history []ledger.Iteration,
	round runner.Resume) runner.AgentRunRequest {

	t.Helper()
	return lostRelaunchIn(t, "", basis, history, round)
}

// lostRelaunchIn is [lostRelaunchOver] in the conversation conversation names
// ([runner.Turn.ConversationKey]), "" for the fixture's own.
func lostRelaunchIn(t *testing.T, conversation string, basis runner.SessionBasis, history []ledger.Iteration,
	round runner.Resume) runner.AgentRunRequest {

	t.Helper()
	return lostRelaunchWith(t, buildOpts{conversation: conversation}, basis, history, round)
}

// lostRelaunchWith is [lostRelaunchOver] in the conversation opts names, its
// relaunch's fences of the turn context under the token opts pins
// ([buildOpts.boundary]), "" for one each fence mints.
func lostRelaunchWith(t *testing.T, opts buildOpts, basis runner.SessionBasis, history []ledger.Iteration,
	round runner.Resume) runner.AgentRunRequest {

	t.Helper()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	notes := ""
	if len(history) > 0 {
		notes = history[len(history)-1].ReviewNotes
	}
	launched, req := launch(t, entries, buildOpts{conversation: opts.conversation}, len(history)+1, notes,
		history)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	relauncher := &probingLauncher{basis: basis}
	var asked runner.AgentLauncher = relauncher
	if opts.reads != nil {
		asked = readingLauncher{probingLauncher: relauncher, reads: opts.reads}
	}
	round.State, round.Lost = parked.State, &runner.Lost{Why: "its host was drained", Losses: 1, Limit: 2}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: asked,
		resume: &round, conversation: opts.conversation, boundary: opts.boundary})
	if _, _, err := collector.Resume(t.Context(), history); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if again.Session == nil {
		t.Fatal("the lost job's round continues no kept session")
	}
	return again
}

// numbered is a call the run made, the seq'th the bridge admitted and the
// appended'th its round's log took.
func numbered(name string, args map[string]any, seq, appended int) runner.BridgedCall {
	return runner.BridgedCall{Call: ledger.Call{Name: name, Args: args, Result: "done"}, Seq: seq,
		Appended: appended}
}

// A CALL IN FLIGHT AT THE READ IS TOLD, WHATEVER THE DROPPED MIDDLE. The run
// made a slow call fifth and three hundred more while it ran; the capture read
// the log after its bound dropped the middle and before the slow call was
// answered. Read by place, the slow call sits below every dropped place and
// reads as answered; read by the order the log took it, it came after the
// save, and the continuation is told it and nothing it holds.
//
// Mutation: read the save point against the place a call was made at (Seq)
// rather than the place the log took it at (Appended), and the call in flight
// goes untold.
func TestACallInFlightAtTheReadIsToldWhateverTheDroppedMiddle(t *testing.T) {
	t.Parallel()
	const atRead = 304 // the calls the log had taken when the capture read it
	var bridged []runner.BridgedCall
	listed := func(appended, seq int) {
		bridged = append(bridged, numbered("slack_post", map[string]any{"text": fmt.Sprintf("post %d", appended)},
			seq, appended))
	}
	// THE LOG'S FIRST HUNDRED: the slow call was made fifth, so every call
	// after it was made a place later than the log took it.
	for n := 1; n <= 100; n++ {
		seq := n
		if n >= 5 {
			seq = n + 1
		}
		listed(n, seq)
	}
	// AND ITS LAST, the 105 between them dropped and tallied.
	for n := 206; n <= atRead; n++ {
		listed(n, n+1)
	}
	bridged = append(bridged, numbered("jira_create", map[string]any{"summary": "the slow one"}, 5, atRead+1))
	brief := afterSaveBrief(t, atRead, bridged, []ledger.Elided{{Name: "slack_post", Calls: 105}})

	for _, want := range []string{"## Calls made after the session above was saved", "jira_create",
		"the slow one"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the continuation is not told %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "slack_post") || strings.Contains(brief, "further call") {
		t.Errorf("the continuation is told a call its session holds, or calls dropped after its save, where "+
			"none were:\n%s", brief)
	}
}

// A CONTINUATION COUNTS THE DROPPED CALLS MADE AFTER ITS SAVE. A session saved
// before or inside a round's dropped middle is told the listed calls after its
// save, then exactly how many dropped calls followed it (the calls past the
// save point less those listed), then the round's tally verbatim. One saved
// after the middle is told neither, one saved past every call nothing. A call
// an older build appended after the save has no number: told, and counted as
// dropped too, never left out. The sentence's "two hundred" is the log's bound.
//
// Mutation: count every dropped call, or none, for those after the save;
// render the tally with nothing dropped after the save, or drop it beside a
// count; take a call with no number off the count; or raise
// sandbox.MaxBridgeCalls past "two hundred": a case goes red.
func TestAContinuationCountsTheDroppedCallsMadeAfterItsSave(t *testing.T) {
	t.Parallel()
	if sandbox.MaxBridgeCalls != 200 {
		t.Fatalf("a continued session is told a run's record keeps its first and last past two hundred "+
			"calls, and it keeps %d", sandbox.MaxBridgeCalls)
	}
	var bridged []runner.BridgedCall
	for n := 1; n <= 300; n++ {
		if n > 100 && n <= 200 {
			continue
		}
		bridged = append(bridged, numbered("jira_create", map[string]any{"summary": fmt.Sprintf("issue %d", n)},
			n, n))
	}
	elided := []ledger.Elided{{Name: "slack_post", Calls: 60, Failed: 2}, {Name: "jira_create", Calls: 40}}
	tally := ledger.FormatElided(elided, nil)
	// issue is how the brief quotes the n'th call's argument, whole, so one
	// number is never read inside another.
	issue := func(n int) string { return fmt.Sprintf(`"issue %d"`, n) }
	// AND ONE MORE CALL, which an older build appended after the save with
	// no number of its place.
	unnumbered := append(slices.Clone(bridged), numbered("jira_create",
		map[string]any{"summary": "issue 301"}, 301, 0))
	for name, tc := range map[string]struct {
		bridged []runner.BridgedCall
		through int
		dropped string
		told    []string
		holds   string
	}{
		"saved part way through the dropped middle": {through: 150,
			dropped: "(+50 further calls this run made after the session above was saved",
			told:    []string{issue(201), issue(300)}, holds: issue(100)},
		"saved before the last dropped call": {through: 199,
			dropped: "(+1 further call this run made after the session above was saved",
			told:    []string{issue(201)}, holds: issue(100)},
		"saved before any call": {through: 0,
			dropped: "(+100 further calls this run made after the session above was saved",
			told:    []string{issue(1), issue(300)}},
		"saved after the dropped middle":             {through: 250, told: []string{issue(251)}, holds: issue(250)},
		"saved past every call the round's log took": {through: 400, holds: issue(300)},
		"a call an older build appended after the save": {bridged: unnumbered, through: 250,
			dropped: "(+1 further call this run made after the session above was saved",
			told:    []string{issue(251), issue(301)}, holds: issue(250)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			calls := bridged
			if tc.bridged != nil {
				calls = tc.bridged
			}
			brief := afterSaveBrief(t, tc.through, calls, elided)
			for _, want := range tc.told {
				if !strings.Contains(brief, want) {
					t.Errorf("the continuation is not told %s:\n%s", want, brief)
				}
			}
			if tc.holds != "" && strings.Contains(brief, tc.holds) {
				t.Errorf("the continuation is told %s, which its session holds:\n%s", tc.holds, brief)
			}
			if len(tc.told) == 0 && strings.Contains(brief, "## Calls made after the session above was saved") {
				t.Errorf("a session saved past every call its round made is told of calls after it:\n%s", brief)
			}
			if tc.dropped == "" {
				if strings.Contains(brief, "this run made after the session above was saved") ||
					strings.Contains(brief, tally) {
					t.Errorf("a session saved after the dropped middle is told of dropped calls:\n%s", brief)
				}
				return
			}
			at, tallied := strings.Index(brief, tc.dropped), strings.Index(brief, tally)
			if at < 0 || tallied < at {
				t.Errorf("the continuation is not told %q followed by the round's tally %q:\n%s", tc.dropped,
					tally, brief)
			}
		})
	}
}

// THE CALLS AFTER A SAVE ARE HELD TO THE LEDGER'S BUDGETS. Each is kept up to
// 16 KiB, so the continuation lists them as the prior-work ledger lists a
// round: an argument value or a failed call's error past its budget rewritten
// by the seat's auxiliary model or, with none, omitted by size and digest,
// never cut and never whole; reads past the ledger's cap counted, not listed;
// every write listed.
//
// Mutation: render the list verbatim, with no budget, and every read and both
// payloads are told whole.
func TestTheCallsAfterASaveAreHeldToTheLedgersBudgets(t *testing.T) {
	t.Parallel()
	body, refusal := strings.Repeat("b", 2000), strings.Repeat("e", 2000)
	var bridged []runner.BridgedCall
	for n := 1; n <= 15; n++ {
		bridged = append(bridged, numbered("slack_history", map[string]any{"channel": fmt.Sprintf("C%d", n)},
			n, n))
	}
	bridged = append(bridged, numbered("jira_create", map[string]any{"summary": "a write", "body": body}, 16, 16))
	failed := numbered("slack_post", map[string]any{"text": "a refused post"}, 17, 17)
	failed.Failed, failed.Result = true, refusal
	bridged = append(bridged, failed)
	brief := afterSaveBrief(t, 0, bridged, nil)

	if reads := strings.Count(brief, "- slack_history("); reads != ledger.MaxReadCalls {
		t.Errorf("the continuation lists %d reads, want the ledger's cap of %d:\n%s", reads, ledger.MaxReadCalls,
			brief)
	}
	for _, want := range []string{"(+3 further read call(s) omitted)", "a write", "a refused post", "not shown"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the continuation is not told %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, body[:300]) || strings.Contains(brief, refusal[:300]) {
		t.Errorf("the continuation carries a payload past its budget:\n%s", brief)
	}
}

// A CONTINUATION'S TOLD CALLS SHARE ONE READ CAP, SPENT ON THE LATE CALLS
// FIRST. The calls after the save and the late calls are one round's, told in
// one brief under two headings: nine late reads and nine after the save are
// twelve read lines, every late read and the first three after, whose heading
// counts the six left out. Only what is shown is rewritten (a read past the cap
// costs none, a late write's long body one), and on a compactor that remembers
// the turn's rewrites, as the engine's does, the fresh brief rewrites none again.
//
// Mutation: a cap per list tells eighteen reads; spending the cap after the
// save first tells three late reads; naming the payloads list by list rewrites
// the read the brief never shows.
func TestAContinuationsToldCallsShareOneReadCap(t *testing.T) {
	t.Parallel()
	read := func(n int, channel string) runner.BridgedCall {
		return numbered("slack_history", map[string]any{"channel": channel}, n, n)
	}
	var bridged []runner.BridgedCall
	for n := 1; n <= 8; n++ {
		bridged = append(bridged, read(n, fmt.Sprintf("C%d", n)))
	}
	bridged = append(bridged, read(9, strings.Repeat("u", 2000)))
	for n := 10; n <= 18; n++ {
		late := read(n, fmt.Sprintf("C%d", n))
		late.Late = true
		bridged = append(bridged, late)
	}
	write := numbered("jira_create", map[string]any{"summary": "a late write", "body": strings.Repeat("b", 2000)},
		19, 19)
	write.Late = true
	bridged = append(bridged, write)

	const rewrite = 37
	meter := &countingMeter{}
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	basis := keptBasis
	basis.Through = 0
	relauncher := &probingLauncher{basis: basis}
	lost := runner.Lost{Why: "its host was drained", Losses: 1, Limit: 2}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		budget: meter, compact: compact.New(chargingModels{meter: meter, tokens: rewrite}, compact.NewCache()).
			For(nil, auxspend.Use{Stage: types.AuxStageTurn}),
		resume: &runner.Resume{State: parked.State, Bridged: bridged, Lost: &lost}})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if again.Session == nil {
		t.Fatal("the lost job's round continues no kept session")
	}
	brief := again.Session.Brief

	if reads := strings.Count(brief, "- slack_history("); reads != ledger.MaxReadCalls {
		t.Errorf("the continuation lists %d reads, want the one cap of %d:\n%s", reads, ledger.MaxReadCalls, brief)
	}
	afterAt := strings.Index(brief, "## Calls made after the session above was saved")
	lateAt := strings.Index(brief, "## Calls the run that made them stopped waiting for, or whose outcome is unknown")
	if afterAt < 0 || lateAt < afterAt {
		t.Fatalf("the continuation is not told the calls after its save, then its late calls:\n%s", brief)
	}
	after, late := brief[afterAt:lateAt], brief[lateAt:]
	if n := strings.Count(late, "- slack_history("); n != 9 || strings.Contains(late, "omitted") {
		t.Errorf("the late calls list %d reads, want all nine and none omitted:\n%s", n, late)
	}
	afterReads := strings.Count(after, "- slack_history(")
	if afterReads != 3 || !strings.Contains(after, "(+6 further read call(s) omitted)") {
		t.Errorf("the calls after the save list %d reads, want the three the cap leaves and the six past it "+
			"counted:\n%s", afterReads, after)
	}
	if !strings.Contains(late, "a late write") || strings.Contains(late, strings.Repeat("b", 300)) {
		t.Errorf("the late write is not told within its budget:\n%s", late)
	}
	rewrites := 0
	for _, charge := range meter.charges() {
		if charge == rewrite {
			rewrites++
		}
	}
	if rewrites != 1 {
		t.Errorf("%d rewrites, want the late write's body alone: the read past the cap is never shown", rewrites)
	}
}

// THE CALLS LISTED AGAIN TAKE THE READ CAP LAST, AND ARE NEVER OWED. A cut-off
// capture holds its round's first four reads, and the brief tells the reads
// after its save (the first four alike the held ones) as calls it may already
// show. The held reads are listed again under the one cap, spent last: where
// the others leave two reads of it, two are listed and two counted; where they
// take it all, the section is not rendered, since a heading over a bare count
// names no call. The session holds them, so the launch records none left out.
//
// Mutation: spend the cap on the calls listed again first, and calls after the
// save are counted; record the reads left out as owed, and the launch records
// them; render the listing with no call, and the second case carries a count.
func TestTheCallsListedAgainTakeTheReadCapLastAndAreNeverOwed(t *testing.T) {
	t.Parallel()
	read := func(channel string, seq int) runner.BridgedCall {
		call := numbered("slack_history", map[string]any{"channel": channel}, seq, seq)
		call.Digest = turnctx.ArgsDigest(call.Args)
		return call
	}
	basis := keptBasis
	basis.Through, basis.Interrupted = 4, true
	for _, tc := range []struct {
		// after is how many reads the brief tells after the save; listed and
		// counted, how many of the four held reads it lists again and counts.
		after, listed, counted int
	}{
		{after: ledger.MaxReadCalls - 2, listed: 2, counted: 2},
		{after: ledger.MaxReadCalls, listed: 0, counted: 4},
	} {
		var bridged []runner.BridgedCall
		for n := 1; n <= 4; n++ {
			bridged = append(bridged, read(fmt.Sprintf("C%d", n), n))
		}
		for n := 1; n <= tc.after; n++ {
			bridged = append(bridged, read(fmt.Sprintf("C%d", n), 4+n))
		}
		s := lostRelaunch(t, basis, runner.Resume{Bridged: bridged}).Session

		at := strings.Index(s.Brief, cutOffHeader)
		if at < 0 {
			t.Fatalf("%d after: the reads after the save are not told as calls the session may already show:\n%s",
				tc.after, s.Brief)
		}
		since, again, listed := strings.Cut(s.Brief[at:], listedAgainHeading)
		if reads := strings.Count(since, "- slack_history("); reads != tc.after || strings.Contains(since, "omitted") {
			t.Errorf("%d after: the calls after the save list %d reads, want all %d and none omitted:\n%s",
				tc.after, reads, tc.after, s.Brief)
		}
		if listed != (tc.listed > 0) {
			t.Fatalf("%d after: the held reads are listed again %v, want %v:\n%s", tc.after, listed,
				tc.listed > 0, s.Brief)
		}
		if listed && (strings.Count(again, "- slack_history(") != tc.listed ||
			!strings.Contains(again, fmt.Sprintf("(+%d further read call(s) omitted)", tc.counted))) {
			t.Errorf("%d after: the calls listed again are not the cap's last, %d listed and %d counted:\n%s",
				tc.after, tc.listed, tc.counted, s.Brief)
		}
		if len(s.Told.Omitted) != 0 {
			t.Errorf("%d after: the launch records %v left out, want none: the session holds the reads listed "+
				"again", tc.after, s.Told.Omitted)
		}
	}
}

// A CARRIED-ON RELAUNCH LAUNCHES NOTHING WHEN THE CALLS AFTER ITS SAVE FILL THE
// WINDOW. The continuation's rewrites are charged to the turn's counters and
// can take the window past its ceiling, so the relaunch asks the meter once
// every brief's rewrite is spent, before anything is launched or an approved
// call made: a run with no room left is never launched and paid for whole.
// The read after the save is one the fresh brief counts past its cap rather
// than lists, so its rewrite is the continuation's alone.
//
// Mutation: build the continued session's brief after the relaunch asks the
// meter, and the run is launched; rewrite a read the fresh brief counts
// without listing, and it is charged twice.
func TestACarriedOnRelaunchLaunchesNothingWhenTheCallsAfterItsSaveFillTheWindow(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	// TWELVE READS THE SESSION HOLDS, and ONE AFTER THE SAVE, its argument past
	// the ledger's value budget.
	var bridged []runner.BridgedCall
	for n := 1; n <= ledger.MaxReadCalls; n++ {
		bridged = append(bridged, numbered("slack_history", map[string]any{"channel": fmt.Sprintf("C%d", n)}, n,
			n))
	}
	bridged = append(bridged, numbered("slack_history", map[string]any{"channel": strings.Repeat("b", 2000)},
		ledger.MaxReadCalls+1, ledger.MaxReadCalls+1))
	meter := &ceilingMeter{ceiling: 100}
	saved := keptBasis
	saved.Through = ledger.MaxReadCalls
	relauncher := &probingLauncher{basis: saved}
	lost := runner.Lost{Why: "its host was drained", Losses: 1, Limit: 2}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		budget: meter, compact: compact.New(chargingModels{meter: meter, tokens: 150}, nil).
			For(nil, auxspend.Use{Stage: types.AuxStageTurn}),
		resume: &runner.Resume{State: parked.State, Bridged: bridged, Lost: &lost}})

	_, _, err := collector.Resume(t.Context(), nil)
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("Resume = %v, want the refusal the brief's own rewrite made certain", err)
	}
	if got := meter.charges(); len(got) != 1 || got[0] != 150 {
		t.Fatalf("charges = %v, want the one rewrite of the call after the save", got)
	}
	if relauncher.launched() != 0 {
		t.Errorf("relaunched %d runs on a window the brief had already filled", relauncher.launched())
	}
}

// A GUARD ARMS ONLY THE LOADS THROUGH ITS SAVE POINT: those the log took
// before the read the save point is, whatever their place among the job's
// calls, by the same test that tells the continuation the calls after its
// save. A load made before a call still in flight is the session's; one made
// early and answered after the read is not. A load an older build took, with
// no number, is after every save point, so its skill is refused until loaded
// again: the safe direction.
//
// Mutation: read a load's place (Seq) against the save point rather than the
// order the log took it, or read a load with no number as before it, and a
// row goes red.
func TestAGuardArmsOnlyTheLoadsThroughItsSavePoint(t *testing.T) {
	t.Parallel()
	saved := func(through int) runner.SessionBasis {
		basis := keptBasis
		basis.Through = through
		return basis
	}
	load := func(seq, appended int) runner.BridgedCall {
		c := loadSkill(seq)
		c.Appended = appended
		return c
	}
	read := func(seq, appended int) runner.BridgedCall {
		return numbered("slack_history", map[string]any{"channel": "C1"}, seq, appended)
	}
	for name, tc := range map[string]struct {
		g       guarded
		refused bool
	}{
		"a load made first and taken after the save": {g: guarded{basis: saved(1),
			bridged: []runner.BridgedCall{load(1, 2), read(2, 1)}}, refused: true},
		"a load made after a call then in flight, and taken before the save": {g: guarded{basis: saved(1),
			bridged: []runner.BridgedCall{read(1, 2), load(2, 1)}}},
		"a load an older build took, with no number": {g: guarded{basis: saved(5),
			bridged: []runner.BridgedCall{load(1, 0)}}, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := tc.g.relaunch(t)
			bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
			if bridged[0].Failed != tc.refused {
				t.Fatalf("slack_post failed %v (%q), want refused %v", bridged[0].Failed, bridged[0].Result,
					tc.refused)
			}
		})
	}
}

// A KEPT SESSION IS TOLD AN EARLIER SUBMIT_WORK DOES NOT STAND. Every call a
// continuation tells after its save was made before the relaunch, so a
// successful `submit_work` among them is not this work's: a loss's is the
// asking job's, from before an earlier answer (a round holding one since its
// last answer is collected, not carried on); a send-back's is the one its
// review sent back. Each is told once, after the calls, that it does not stand
// and the run makes its own. One the session holds is told once, before the
// calls after the save, in words of the session, never as one among them,
// which would send the run looking for a call that list lacks.
//
// Mutation: drop the line after the calls, and the first two cases go red;
// drop the held one, or say it of the calls after the save, and the last two do.
func TestAKeptSessionIsToldAnEarlierSubmitWorkDoesNotStand(t *testing.T) {
	t.Parallel()
	const (
		notStanding  = "does NOT stand as this work's submission: the work was carried on after it."
		afterHeading = "## Calls made after the session above was saved"
	)
	// asking is the asking job's submission, the first call its round's log
	// took, made before its question.
	asking := func() runner.BridgedCall {
		return runner.BridgedCall{Call: ledger.Call{Name: runner.SubmitWorkTool,
			Args:   map[string]any{"outcome": "delivered", "summary": "the asking job's submission"},
			Result: "submitted"}, Seq: 1, Appended: 1}
	}
	// lostAfterAnswer is the round a loss carries on after an answer: the
	// asking job's submission and question, then the post the job the answer
	// opened made before it was lost.
	lostAfterAnswer := func() runner.Resume {
		return runner.Resume{Answered: 2, Bridged: []runner.BridgedCall{asking(),
			numbered("ask_and_wait", map[string]any{"question": "the asking job's question"}, 2, 2),
			numbered("slack_post", map[string]any{"text": "a post after the answer"}, 3, 3),
		}}
	}
	told := func(t *testing.T, brief, submission, last string) {
		t.Helper()
		listed := strings.Index(brief, submission)
		if listed < 0 || strings.Index(brief, last) < listed {
			t.Fatalf("the session is not told the submission, then the calls after it:\n%s", brief)
		}
		said := strings.Index(brief, notStanding)
		if said < strings.Index(brief, last) || strings.Count(brief, notStanding) != 1 {
			t.Errorf("the session is not told once, after the calls, that the submission among them does not "+
				"stand:\n%s", brief)
		}
	}
	// heldFirst fails t unless brief, continuing a session that holds
	// submission, lists it nowhere, tells the calls after the save down to
	// last, and says once, before them, that the submission the session shows
	// does not stand, and never that one among them does not.
	heldFirst := func(t *testing.T, brief, submission, last string) {
		t.Helper()
		after := strings.Index(brief, afterHeading)
		if after < 0 || !strings.Contains(brief[after:], last) || strings.Contains(brief, submission) {
			t.Fatalf("the session is not told the calls after its save alone:\n%s", brief)
		}
		said := strings.Index(brief, heldSubmissionLine)
		if said < 0 || said > after || strings.Count(brief, heldSubmissionLine) != 1 {
			t.Errorf("the session is not told once, before the calls after its save, that the submission it "+
				"shows does not stand:\n%s", brief)
		}
		if strings.Contains(brief, "among them "+notStanding) {
			t.Errorf("the calls after the save, which hold no submission, are told one among them does not "+
				"stand:\n%s", brief)
		}
	}
	// sentBack is the brief a send-back continues the collected job's capture,
	// saved as basis says, with: the job posted, submitted, then filed an issue.
	sentBack := func(t *testing.T, basis runner.SessionBasis) string {
		t.Helper()
		entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
		launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
		bridged := play(t, req, run{calls: []llm.ToolCall{
			call(t, "slack_post", `{"text":"the first pass"}`),
			call(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"the reviewed submission","deliveries":["slack_post"]}`),
			call(t, "jira_create", `{"summary":"a call after the submission"}`),
		}})
		parked, _ := launched.Suspended()
		relauncher := &probingLauncher{basis: basis}
		collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
			resume: &runner.Resume{State: parked.State, Bridged: bridged, Answer: "the first pass is done"}})
		if _, _, err := collector.Resume(t.Context(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		history := []ledger.Iteration{{Iteration: 1, Calls: ledgerOf(bridged)}}
		if _, _, err := collector.Execute(t.Context(), 2, "the summary leaves the release out", history); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		again := relauncher.last(t)
		if again.Session == nil {
			t.Fatal("the send-back continues no kept session")
		}
		return again.Session.Brief
	}
	t.Run("a loss after an earlier answer", func(t *testing.T) {
		t.Parallel()
		// THE JOB AN ANSWER OPENED, captured cut off at the round's start, and
		// lost: the job before it submitted and then asked.
		basis := keptBasis
		basis.Through, basis.Interrupted = 0, true
		brief := lostRelaunch(t, basis, lostAfterAnswer()).Session.Brief
		told(t, brief, "the asking job's submission", "a post after the answer")
	})
	t.Run("a send-back", func(t *testing.T) {
		t.Parallel()
		// THE COLLECTED JOB, captured cut off once its first call was
		// answered: its submission came after the save.
		basis := keptBasis
		basis.Interrupted = true
		told(t, sentBack(t, basis), "the reviewed submission", "a call after the submission")
	})
	t.Run("a loss whose session holds the submission", func(t *testing.T) {
		t.Parallel()
		// THE ASKING JOB, captured once its submission was answered
		// (keptBasis, at the log's first call), and the job the answer opened
		// lost without a capture of its own.
		brief := lostRelaunch(t, keptBasis, lostAfterAnswer()).Session.Brief
		heldFirst(t, brief, "the asking job's submission", "a post after the answer")
	})
	t.Run("a send-back whose session holds the submission", func(t *testing.T) {
		t.Parallel()
		// THE COLLECTED JOB, captured once its submission was answered: the
		// issue it filed after it came after the save.
		basis := keptBasis
		basis.Through = 2
		heldFirst(t, sentBack(t, basis), "the reviewed submission", "a call after the submission")
	})
}

// A SESSION HOLDING AN EARLIER TURN'S SUBMIT_WORK IS TOLD NOTHING OF IT. A
// job may continue the session its conversation's last turn left, holding
// that turn's `submit_work`, as do the in-box copy and any capture of the job.
// Only the round's own calls say a submission the session shows does not
// stand: the lead already said where the earlier turn ended, and a run told
// otherwise would look for one of this round's that was never made.
//
// Mutation: say it wherever the copy in the box holds every call of the round,
// or of any successful call the session holds, and a case goes red.
func TestASessionHoldingAnEarlierTurnsSubmitWorkIsToldNothingOfIt(t *testing.T) {
	t.Parallel()
	posted := numbered("slack_post", map[string]any{"text": "the job's own post"}, 1, 1)
	t.Run("the copy in its job's box, its harness having ended its own run", func(t *testing.T) {
		t.Parallel()
		brief := inPlaceBrief(t, inBoxCarryNamed(t, "a person's answer"), true, 0, nil, func(r *runner.Resume) {
			r.Bridged = []runner.BridgedCall{posted,
				numbered("ask_and_wait", map[string]any{"question": "the job's own question"}, 2, 2)}
			r.HarnessEnd = sandbox.HarnessEndOwnRun
		})
		if strings.Contains(brief, "does NOT stand") {
			t.Errorf("a copy whose round holds no submission is told one does not stand:\n%s", brief)
		}
	})
	t.Run("a capture of the job, lost", func(t *testing.T) {
		t.Parallel()
		brief := lostRelaunch(t, keptBasis, runner.Resume{Bridged: []runner.BridgedCall{posted,
			numbered("jira_create", map[string]any{"summary": "an issue after the save"}, 2, 2)}}).Session.Brief
		if !strings.Contains(brief, "an issue after the save") {
			t.Fatalf("the session is not told the call after its save:\n%s", brief)
		}
		if strings.Contains(brief, "does NOT stand") {
			t.Errorf("a session whose round holds no submission is told one does not stand:\n%s", brief)
		}
	})
}
