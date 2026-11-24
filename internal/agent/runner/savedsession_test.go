package runner_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
)

// savedAt is when the kept capture of [savedBasis] was taken.
var savedAt = time.Date(2026, 11, 8, 9, 20, 0, 0, time.UTC)

// savedBasis is a turn's kept session taken while its job still worked, placed
// at the round's start: cut off, as every such capture is, and gone on past.
var savedBasis = runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1", Home: "/boxes/4f2a/home",
	Interrupted: true, WentOn: true, CapturedAt: savedAt, ContextTokens: 40000}

// savedHeader opens [prompts.SavedSession]'s words, whatever instant they name.
const savedHeader = "## The session above was saved while its run still worked"

// A SESSION SAVED WHILE ITS HARNESS WORKED IS SAID SO: a relaunch continuing a
// capture taken while its job still worked tells the session it was saved
// then, when, and that its run went on, in place of the cut-off words alone,
// which would have it take the session's end for where its work stopped. A
// final capture cut off is told it was cut off, never that it went on; a
// relaunch continuing no kept capture is told neither.
//
// Mutation: tell a save only that it was cut off, or tell a final capture cut
// off that its run went on (switch on Interrupted), and a case goes red.
func TestASessionSavedWhileItsHarnessWorkedIsSaidSo(t *testing.T) {
	t.Parallel()
	final := keptBasis
	final.Interrupted = true
	for _, c := range []struct {
		name  string
		basis runner.SessionBasis
		// saved and cut say whether the kept continuation is told
		// SavedSession at savedAt and InterruptedSession; kept whether
		// there is one.
		kept, saved, cut bool
	}{
		{name: "a capture its job went on past", basis: savedBasis, kept: true, saved: true},
		{name: "a final capture cut off", basis: final, kept: true, cut: true},
		{name: "no kept capture continued", basis: runner.SessionBasis{KeptTurn: "t1"}},
		{name: "the session the conversation's last turn left", basis: runner.SessionBasis{SessionID: "s-base",
			Harness: "claude-code", Base: &runner.BaseBasis{Turn: "t-0", Home: "/boxes/old/home",
				EndedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			resume := runner.Resume{}
			answered("use main")(&resume)
			again := relaunchOf(t, resume, &probingLauncher{basis: c.basis}, buildOpts{})
			briefs := []string{again.Brief}
			if again.Continue != nil {
				briefs = append(briefs, again.Continue.Brief)
			}
			if again.Session != nil {
				briefs = append(briefs, again.Session.Brief, again.Session.Replaced)
			}
			if c.kept != (again.Session != nil && again.Session.Basis.Continues()) {
				t.Fatalf("the relaunch continues %+v, want a kept capture continued: %v", again.Session, c.kept)
			}
			if c.kept {
				for _, brief := range []string{again.Session.Brief, again.Session.Replaced} {
					if got := strings.Contains(brief, prompts.SavedSession(savedAt)); got != c.saved {
						t.Errorf("the kept continuation is told it was saved while its run worked, at %v: %v, "+
							"want %v:\n%s", savedAt, got, c.saved, brief)
					}
					if got := strings.Contains(brief, prompts.InterruptedSession); got != c.cut {
						t.Errorf("the kept continuation is told only that it was cut off: %v, want %v:\n%s", got,
							c.cut, brief)
					}
				}
				return
			}
			for _, brief := range briefs {
				if strings.Contains(brief, savedHeader) || strings.Contains(brief, prompts.InterruptedSession) {
					t.Errorf("a relaunch continuing no kept capture is told of one:\n%s", brief)
				}
			}
		})
	}
}

// A LOST JOB CONTINUED FROM A SAVE IS TOLD IT WENT ON PAST IT. The relaunch
// carrying a lost job's round on in the newest capture that job kept, taken
// while it still worked, tells the session first what cut its run off and why
// (runner.Lost.Continuation), then that the session was saved while its run
// worked, when, and that the run went on after it, then the calls the run made
// after the save, each once and in that order; never only that it was cut off.
// Continued from a final capture, cut off as its job was lost, it is told that
// it was cut off, as before.
//
// Mutation: tell a lost job's save only that it was cut off, or tell its final
// capture that its run went on, and a case goes red.
func TestALostJobContinuedFromASaveIsToldItWentOnPastIt(t *testing.T) {
	t.Parallel()
	lost := lostWithItsBox()
	lead := lost.Continuation()
	created := numbered("jira_create", map[string]any{"summary": "after the save"}, 1, 1)
	carriedOn := func(t *testing.T, basis runner.SessionBasis) string {
		t.Helper()
		again := relaunchOf(t, runner.Resume{Lost: lostWithItsBox(), Bridged: []runner.BridgedCall{created}},
			&probingLauncher{basis: basis}, buildOpts{})
		if again.Session == nil || !again.Session.Basis.Continues() {
			t.Fatalf("the lost job's round continues %+v, want the kept capture", again.Session)
		}
		return again.Session.Brief
	}

	t.Run("from a save", func(t *testing.T) {
		t.Parallel()
		brief := carriedOn(t, savedBasis)
		saved := prompts.SavedSession(savedAt)
		parts := []string{lead, saved, "## Calls this run made that the session above may not show"}
		at := -1
		for _, part := range parts {
			if n := strings.Count(brief, part); n != 1 {
				t.Fatalf("the continuation tells %q %d times, want once:\n%s", part, n, brief)
			}
			next := strings.Index(brief, part)
			if next < at {
				t.Errorf("the continuation tells %q before what comes ahead of it:\n%s", part, brief)
			}
			at = next
		}
		if !strings.Contains(brief[at:], "jira_create") {
			t.Errorf("the continuation does not tell the call made after its save:\n%s", brief)
		}
		if strings.Contains(brief, prompts.InterruptedSession) {
			t.Errorf("a lost job's save is told only that it was cut off:\n%s", brief)
		}
	})

	t.Run("from a final capture", func(t *testing.T) {
		t.Parallel()
		final := keptBasis
		final.Interrupted, final.Through = true, 0
		brief := carriedOn(t, final)
		if !strings.Contains(brief, lead) || !strings.Contains(brief, prompts.InterruptedSession) ||
			strings.Contains(brief, savedHeader) {
			t.Errorf("a lost job's final capture is not told it was cut off, or is told its run went on:\n%s",
				brief)
		}
	})
}
