package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/steer"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A SECOND ADMISSION IS COUNTED WHEN SERVED, AND A NOTE'S POLL SERVES BOUND IT
// ACROSS JOBS. A poll serving a note to a new job spends one of its
// [sandbox.RunNote.MaxServes] serves; an ask for its second admission, made
// only once a compaction folded it, spends one more and answers the job's own
// id ([executor.AgainID]), at no cost made again. A wake is never admitted
// again. At its budget a note is answered spent, and a new job's poll passes
// over it, never stopping there, so it waits for the review while the notes
// after it are served.
//
// Mutation: count the serve at every poll, or at the offer; grant past the
// budget, or a wake; stop a poll at a note at its budget; and this goes red.
func TestASecondAdmissionIsCountedWhenServedAndThePollBudgetSpansJobs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRunOn(t, store, runs, "run-1", executor.OpenCodeName)
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder", ByKind: iam.ActorHuman})
	if err := runs.fileWake(ctx, "run-1", launch, wakeAbout("w-1", "the deploy target is staging")); err != nil {
		t.Fatal(err)
	}
	note := func() sandbox.RunNote {
		t.Helper()
		return noteOn(t, store, "run-1", "n-1")
	}
	first := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, first, 2)
	for range 2 {
		if served, _ := first.Wait(ctx, 0); len(served) != 2 {
			t.Fatalf("the poll answered %+v, want the note and the wake", served)
		}
	}
	if n := note(); n.PollServes != 1 || n.Readmitted != "" {
		t.Fatalf("two polls of one job left the note at %d poll serves (readmitted %q), want one and none",
			n.PollServes, n.Readmitted)
	}
	wake := ""
	for _, n := range mustRow(t, store, "run-1").Steer.Notes {
		if n.IsWake() {
			wake = n.ID
		}
	}
	for range 2 {
		granted, spent, err := first.Again(ctx, []string{"n-1", wake, "planted"})
		if err != nil || len(granted) != 1 || granted["n-1"] != executor.AgainID(executor.OpenCodeName, launch, "n-1") ||
			len(spent) != 0 {
			t.Fatalf("the ask answered %v, %v, %v, want n-1 granted its job's second id alone", granted, spent, err)
		}
	}
	if n := note(); n.PollServes != 2 || n.Readmitted != launch {
		t.Fatalf("the ask, made twice, left the note at %d poll serves (readmitted %q), want two, by the job",
			n.PollServes, n.Readmitted)
	}

	// THE NEXT JOB'S POLL SPENDS THE LAST SERVE; ITS ASK IS SPENT.
	relaunch := func() string {
		t.Helper()
		next, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "run-1", AgentID: sweAgent, AgentHandle: "swe",
			Role: "SWE", Harness: executor.OpenCodeName}, sandbox.Fence{})
		if err != nil {
			t.Fatalf("relaunch: %v", err)
		}
		return next.LaunchID
	}
	second := boundInbox(t, runs, store, "run-1", relaunch())
	viewHolds(t, second, 1)
	if served, _ := second.Wait(ctx, 0); len(served) != 1 || served[0].ID != "n-1" {
		t.Fatalf("the next job's poll answered %+v, want the note", served)
	}
	if granted, spent, err := second.Again(ctx, []string{"n-1"}); err != nil || len(granted) != 0 ||
		!slices.Equal(spent, []string{"n-1"}) || note().PollServes != sandbox.MaxNoteServes {
		t.Fatalf("the ask at the budget answered %v, %v, %v at %d serves, want n-1 spent at %d", granted, spent,
			err, note().PollServes, sandbox.MaxNoteServes)
	}

	// AND A THIRD JOB'S POLL PASSES OVER IT TO THE NOTE AFTER IT.
	third := boundInbox(t, runs, store, "run-1", relaunch())
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-2", Text: "and tag it", By: "founder", ByKind: iam.ActorHuman})
	viewHolds(t, third, 2)
	if served, _ := third.Wait(ctx, 0); len(served) != 1 || served[0].ID != "n-2" {
		t.Fatalf("a job past the note's budget was answered %+v, want n-2 alone", served)
	}
	if n := note(); !n.Waiting() || n.PollServes != sandbox.MaxNoteServes {
		t.Errorf("the note at its budget is %+v, want it waiting for the review, its serves unspent past it", n)
	}
}

// A FOLD IS SAID ONCE A NOTE, BY ITS ID, AND NEVER WITH ITS WORDS: the plugin's
// post of a note it gave up after a second compaction is logged at WARN naming
// the note, its job and its turn and why it waits for the review; a repeat says
// nothing more, an id the job does not hold says nothing at all, and no line
// carries a word of the note.
//
// Mutation: log the note's text, log every repeat, or log an id the job does
// not hold, and this goes red.
func TestAFoldIsSaidOnceANoteByItsIDAndNeverWithItsWords(t *testing.T) {
	logs := captureEngineLogs(t)
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRunOn(t, store, runs, "run-1", executor.OpenCodeName)
	const words = "deploy to the staging cluster first"
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: words, By: "founder", ByKind: iam.ActorHuman})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, inbox, 1)
	inbox.Folded(ctx, []string{"n-1", "planted"})
	inbox.Folded(ctx, []string{"n-1"})
	said := logs.about("steer_note_folded", "note_id", "n-1")
	if len(said) != 1 || said[0]["level"] != "WARN" || said[0]["launch_id"] != launch ||
		said[0]["turn_id"] != "run-1" || !strings.Contains(said[0]["detail"].(string), "compacted twice") ||
		!strings.Contains(said[0]["detail"].(string), "waits for the review") {
		t.Errorf("the fold was said %v, want once at WARN naming the job, the turn and why", said)
	}
	if planted := logs.about("steer_note_folded", "note_id", "planted"); len(planted) != 0 {
		t.Errorf("a fold of a note the job does not hold was said: %v", planted)
	}
	logs.mu.Lock()
	all := logs.buf.String()
	logs.mu.Unlock()
	if strings.Contains(all, words) || strings.Contains(all, "staging cluster") {
		t.Errorf("the log carries the note's words:\n%s", all)
	}
}
