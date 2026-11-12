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

// WHAT A CONTINUED SESSION IS TOLD OF THE CALLS AFTER ITS SAVE, read against
// its save point: how many of the round's calls the run's log had taken at the
// read the capture was placed by, its own or, where its harness may not have
// written every call it made, an earlier capture's
// ([runner.SessionBasis.Through]), every call the log took after it
// ([runner.BridgedCall.Appended] past it) being one the session may not have
// been answered.

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
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	relauncher := &probingLauncher{basis: basis}
	round.State, round.Lost = parked.State, &runner.Lost{Why: "its host was drained", Losses: 1, Limit: 2}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		resume: &round})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
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
// made a slow call fifth and three hundred more while it ran, so by the time
// the capture read the log the bound had dropped the hundred-odd calls between
// the log's first and last hundred, and the call answered only after the read.
// Read by place, the dropped places are the highest the log leaves unlisted —
// sixty-four of them filled every slot an older build's stamp names below its
// highest call — and the slow call, below them all, read as answered. Read by
// the order the log took it, it came after the save, and the continuation is
// told it, and nothing it holds.
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

// A CONTINUATION COUNTS THE DROPPED CALLS MADE AFTER ITS SAVE. A round of three
// hundred calls whose log dropped the hundred between its first and last
// hundred tells a session saved part way through every call the log lists
// after the save, and then how many of the calls after it the log dropped,
// exactly — the round's calls past the save point less those it lists — and
// the round's tally of every call it dropped, the line every other reader of a
// long round is given, verbatim. A session saved after the dropped middle is
// told neither, since nothing it lacks was dropped, and one saved past every
// call the round's log took is told nothing at all. A call an older build
// appended after the save, in a fleet mid-upgrade, has no number: it is told,
// and counted among the dropped calls too, its place unknown — told twice,
// never left out. And the sentence's bound is the log's.
//
// Mutation: count every dropped call, or none, in place of those after the
// save; render the tally where nothing after the save was dropped; drop the
// tally beside a count; take a call with no number off the count; or raise
// sandbox.MaxBridgeCalls under the sentence's "two hundred", and a case goes
// red.
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

// THE CALLS AFTER A SAVE ARE HELD TO THE LEDGER'S BUDGETS. A session saved early
// in a long round has a hundred calls after it, each kept up to 16 KiB, so the
// continuation lists them as the prior-work ledger lists a round: an
// argument value or a failed call's error past its budget rewritten by the
// seat's auxiliary model, or, where none can be had, omitted by its size and
// digest, never cut and never whole; and the reads past the ledger's cap
// counted rather than listed. Every write is listed.
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
// FIRST. The calls after a session's save and its late calls are one round's,
// told in one brief under two headings, so the ledger's cap counts the reads of
// both together: nine late reads and nine after the save are twelve read lines
// in all, every late read and the first three after the save, whose heading
// counts the six it leaves out. Only what the brief shows is rewritten: a read
// after the save past the cap costs no rewrite of its long argument, and a late
// write's long body costs one.
//
// Mutation: render the two lists with a cap each, and eighteen reads are told;
// spend the cap on the calls after the save first, and three late reads are;
// name the payloads list by list, and the read the brief never shows is
// rewritten all the same.
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
		budget: meter, compact: compact.New(chargingModels{meter: meter, tokens: rewrite}, nil).
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

// A CARRIED-ON RELAUNCH LAUNCHES NOTHING WHEN THE CALLS AFTER ITS SAVE FILL THE
// WINDOW. Telling a continued session the calls after its save rewrites a
// payload past its budget with the seat's auxiliary model, charged to the
// turn's counters, and that rewrite can take the window past its ceiling. The
// relaunch that carries a lost job's round on asks the meter once every
// rewrite its briefs make is spent — before anything is launched, and before
// the call a person approved would be made — so a run the rewrite left no room
// for is never launched and paid for whole past the ceiling.
//
// Mutation: build the continued session's brief after the relaunch asks the
// meter, as it was built beside its lead, and the run is launched.
func TestACarriedOnRelaunchLaunchesNothingWhenTheCallsAfterItsSaveFillTheWindow(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	// ONE WRITE AFTER THE SAVE, its body past the ledger's value budget.
	bridged := []runner.BridgedCall{numbered("jira_create",
		map[string]any{"summary": "after the save", "body": strings.Repeat("b", 2000)}, 2, 2)}
	meter := &ceilingMeter{ceiling: 100}
	relauncher := &probingLauncher{basis: keptBasis}
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

// A GUARD ARMS ONLY THE LOADS THROUGH ITS SAVE POINT. A relaunch that continues
// a kept session arms its guard with the loads its job made that the session
// holds: those the log took before the read the save point is, whatever their
// place among the calls the job made — a load made before a call still in
// flight is the session's, and one made early and answered only after the
// read is not — by the same test the continuation is told the calls after the
// save by. A load an older build took, with no number, is after every save
// point, so its skill is refused until loaded again: the safe direction.
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

// A KEPT SESSION IS TOLD AN EARLIER SUBMIT_WORK DOES NOT STAND. A continuation
// of a kept capture tells the calls the log took after its save point, and
// every one was made before the relaunch it opens, so a successful
// `submit_work` among them is no submission of the work that relaunch carries
// on: a lost round whose calls since its last answer hold one is collected on
// it rather than carried on, so the one a loss's continuation tells was made
// before an earlier answer, by the job that then asked; and a send-back's
// continuation can tell the submission its review sent back, as it does where
// the capture was saved before it. Each is told, right after the calls and
// once, that the submission does not stand and that the run makes its own.
//
// AND OF ONE THE SESSION HOLDS, FIRST, IN WORDS OF THE SESSION RATHER THAN OF A
// LIST: a capture saved once the asking job's submission was answered holds it,
// and so does a capture of the collected job saved past its submission, which
// a send-back continues. Neither lists it, and it stands no more than a listed
// one, so each is told, once and before the calls after its save, that the
// submission the session shows does not stand; and no line says one among
// those calls does not stand, which hold none. Said under them, it would tell
// the run a call it reads nowhere in that list is there.
//
// Mutation: never say a successful submission among the calls does not stand,
// and the first two cases go red; never say one the session holds does not
// stand, or say it of the calls after the save, and the last two do.
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
// turn's job may continue the session its conversation's last turn left, which
// holds that turn's `submit_work`, and then the copy in the job's box holds it
// too, and so does a capture of the job. It is no call of this round, and only
// the round's own calls say that a submission the session shows does not
// stand, so a session whose round holds no submission is told nothing of one:
// the lead that began this turn said where the earlier turn ended, and a run
// told that a submission it shows does not stand would look for one of this
// round's that was never made.
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
