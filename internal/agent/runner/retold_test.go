package runner_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A note a take recorded read and never handed over, as the briefs of every
// later launch tell it again, and as the launch names it for the write that
// records the telling.

// steerRead is a note the turn has read, as the run's record carries it.
func steerRead(id, text string, iteration, order int, boundary types.SteerBoundary, launch string,
	unanswered bool) steer.Read {
	return steer.Read{Note: steer.Note{ID: id, Text: text, By: "founder"},
		Reading: steer.Reading{Phase: types.PhaseExecute, Iteration: iteration, Round: 1, Boundary: boundary,
			Launch: launch, Order: order, Unanswered: unanswered}}
}

// A LAUNCH NAMES EVERY MARKED NOTE ITS BRIEFS TELL AGAIN. A read the run's record
// marks unanswered is a note its take's harness never had, and the brief of
// every launch after it tells the note again, whichever session the launch
// takes up: the fresh brief tells every note the turn has read, and a continued
// session, kept or the job's own copy in its box, every note from the first
// such read on. So the launch names every read the turn carries marked as the
// brief was built (runner.AgentRunRequest.Retold), for its write to record the
// telling, a mark the brief's own read of the record found included; a send-back
// and a person's answer alike. None of them is among the notes the brief is the
// first to read, and a turn carrying no mark names none.
//
// Mutation: name no read, only the reads the segment resumed with marked, or
// every read; or tell a marked note in no continued session, and a case goes
// red.
func TestALaunchNamesEveryMarkedNoteItsBriefsTellAgain(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	steered := []steer.Read{
		steerRead("brief", "the note the job's brief carried", 1, 1, types.SteerBoundaryBrief, "job-1", false),
		steerRead("lost", "the note whose take's answer was lost", 1, 2, types.SteerBoundaryToolBatch, "job-1",
			true),
		steerRead("after", "the note an answered take read after it", 1, 3, types.SteerBoundaryToolBatch,
			"job-1", false),
		steerRead("late", "the note whose take was marked after the segment resumed", 1, 4,
			types.SteerBoundaryToolBatch, "job-1", false),
	}
	marked := []steer.Read{steered[1], steered[3]}
	msg := func(r steer.Read) string { return prompts.SteerMessage(r.By, r.Text) }
	basis := keptBasis
	basis.NoteCursor = 4
	// briefs is every brief a relaunch hands, by what takes it up.
	briefs := func(t *testing.T, again runner.AgentRunRequest, inPlace bool) map[string]string {
		t.Helper()
		if again.Session == nil {
			t.Fatal("the relaunch continues no kept session")
		}
		out := map[string]string{"the fresh brief": again.Brief, "the kept session": again.Session.Brief}
		if inPlace {
			if again.Continue == nil || again.Continue.Brief == "" {
				t.Fatal("the relaunch continues no copy in its job's box")
			}
			out["the job's own copy"] = again.Continue.Brief
		}
		return out
	}

	t.Run("a send-back", func(t *testing.T) {
		t.Parallel()
		launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
		bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"first pass"}`)}})
		parked, _ := launched.Suspended()
		relauncher := &probingLauncher{basis: basis}
		collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
			steer: markingBox{Box: steer.New(), unanswered: []string{"lost", "late"}},
			resume: &runner.Resume{State: parked.State, Bridged: bridged, Answer: "first pass done",
				Steered: steered}})
		if _, _, err := collector.Resume(t.Context(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		history := []ledger.Iteration{{Iteration: 1, Calls: ledgerOf(bridged)}}
		if _, _, err := collector.Execute(t.Context(), 2, "the test still fails on CI", history); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		again := relauncher.last(t)
		if !slices.Equal(again.Retold, []string{"lost", "late"}) {
			t.Errorf("the send-back names %v told again, want every read marked as its brief was built",
				again.Retold)
		}
		for name, brief := range briefs(t, again, false) {
			for _, r := range marked {
				if strings.Count(brief, msg(r)) != 1 {
					t.Errorf("%s does not tell %q once:\n%s", name, r.ID, brief)
				}
			}
		}
	})
	for name, tc := range map[string]struct {
		end    sandbox.HarnessEnd
		source func() runner.SteerSource
		marks  []steer.Read
		want   []string
	}{
		"a person's answer, its job's harness ending its own run": {end: sandbox.HarnessEndOwnRun,
			source: func() runner.SteerSource {
				return markingBox{Box: steer.New(), unanswered: []string{"lost", "late"}}
			}, marks: marked, want: []string{"lost", "late"}},
		"a person's answer, its job's harness cut off": {end: sandbox.HarnessEndCutOff,
			source: func() runner.SteerSource {
				return markingBox{Box: steer.New(), unanswered: []string{"lost", "late"}}
			}, marks: marked, want: []string{"lost", "late"}},
		"a person's answer, the turn carrying no mark": {end: sandbox.HarnessEndOwnRun,
			source: func() runner.SteerSource { return steer.New() }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
			parked, _ := launched.Suspended()
			parked.State.ActiveTools = req.Surface.Active()
			carriedReads := steered
			if tc.marks == nil {
				carriedReads = slices.Clone(steered)
				carriedReads[1].Unanswered = false
			}
			relauncher := &probingLauncher{basis: basis}
			resume := &runner.Resume{State: parked.State, Steered: carriedReads, HarnessEnd: tc.end,
				Run: runner.RunRecord{LaunchID: "job-1"},
				Bridged: []runner.BridgedCall{
					numbered("jira_create", map[string]any{"summary": "the job's issue"}, 1, 1)},
				Asked: &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
					SessionID: "s-1", BoxKept: true}}
			collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
				steer: tc.source(), resume: resume})
			if _, _, err := collector.Resume(t.Context(), nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			again := relauncher.last(t)
			if !slices.Equal(again.Retold, tc.want) {
				t.Errorf("the relaunch names %v told again, want %v", again.Retold, tc.want)
			}
			for _, n := range again.Notes {
				if slices.Contains(again.Retold, n.ID) {
					t.Errorf("note %s is named told again and among the notes the brief is the first to read",
						n.ID)
				}
			}
			for name, brief := range briefs(t, again, true) {
				for _, r := range tc.marks {
					if strings.Count(brief, msg(r)) != 1 {
						t.Errorf("%s does not tell %q once:\n%s", name, r.ID, brief)
					}
				}
			}
		})
	}
}

// A CONTINUATION OF A JOB CUT OFF AFTER TELLING A MARKED NOTE AGAIN IS STILL
// TOLD IT. Job 1's take of a note was marked unanswered; job 2's brief told it
// again, and its launch's write recorded it read by job 2's brief, unmarked, at
// the take's own place among the reads. Then job 2 was cut off: its session is
// not known to hold its brief, so the launch that carries its round on tells
// every note the turn has read, as notes it may already show, in its kept
// session and in its own copy in its box, and the fresh brief tells every
// note as it always does: the note is told once in each, at its place, ahead
// of the note job 2's take read after it. Nothing is marked any more, so the
// launch names no note told again.
//
// Mutation: tell a cut-off session only the notes past its cursor, or the copy
// in its box only the notes from a read marked unanswered, or place the note
// told again where the launch's write was made, and a case goes red.
func TestAContinuationOfAJobCutOffAfterTellingAMarkedNoteAgainIsStillToldIt(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	retold := steerRead("lost", "the note job 1's take never handed over", 2, 1, types.SteerBoundaryBrief,
		"job-2", false)
	later := steerRead("later", "the note job 2's take read after it", 2, 2, types.SteerBoundaryToolBatch,
		"job-2", false)
	msg := func(r steer.Read) string { return prompts.SteerMessage(r.By, r.Text) }
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	basis := keptBasis
	basis.Interrupted, basis.NoteCursor = true, 0
	relauncher := &probingLauncher{basis: basis}
	resume := &runner.Resume{State: parked.State, Steered: []steer.Read{retold, later},
		HarnessEnd: sandbox.HarnessEndCutOff, Run: runner.RunRecord{LaunchID: "job-2"},
		Bridged: []runner.BridgedCall{numbered("jira_create", map[string]any{"summary": "the job's issue"}, 1, 1)},
		Asked: &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder", SessionID: "s-1",
			BoxKept: true}}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		steer: steer.New(), resume: resume})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if again.Session == nil || again.Continue == nil || again.Continue.Brief == "" {
		t.Fatalf("the relaunch continues kept %+v and in place %+v, want both offered", again.Session,
			again.Continue)
	}
	if len(again.Retold) != 0 {
		t.Errorf("the relaunch names %v told again, want none: nothing is marked any more", again.Retold)
	}
	for name, brief := range map[string]string{"the fresh brief": again.Brief,
		"the kept session": again.Session.Brief, "the job's own copy": again.Continue.Brief} {
		first, second := strings.Index(brief, msg(retold)), strings.Index(brief, msg(later))
		if strings.Count(brief, msg(retold)) != 1 || first < 0 || second < first {
			t.Errorf("%s does not tell the note told again once, ahead of the later read:\n%s", name, brief)
		}
	}
}
