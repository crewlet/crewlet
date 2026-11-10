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
