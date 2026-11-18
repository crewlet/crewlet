package runner_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
)

// A TELLING ([runner.Resume.Tellings]): a late call the row records a job's own
// session took, at the place among the turn's reads of the write that recorded
// it. A continuation of a session that holds it is not told the call again;
// every other continuation is, as before any telling existed.

// tookLate is a round whose log took one call of its job's and then, late, a
// `jira_create` of the job before's at place 2, which a job's session took: its
// telling stands at place 5 among the turn's reads.
func tookLate() runner.Resume {
	late := numbered("jira_create", map[string]any{"summary": "the late one"}, 2, 2)
	late.Late = true
	return runner.Resume{Bridged: []runner.BridgedCall{
		numbered("slack_post", map[string]any{"text": "before it"}, 1, 1), late,
	}, Tellings: []runner.Telling{{Appended: 2, Place: 5}}}
}

// keptAt is the turn's kept session, saved past the round's two calls, its note
// cursor stamped at cursor.
func keptAt(cursor int) runner.SessionBasis {
	basis := keptBasis
	basis.Through, basis.NoteCursor = 2, cursor
	return basis
}

// A SESSION THAT TOOK A LATE CALL IS NOT TOLD IT AGAIN. The kept session's
// capture was read once the write that recorded its session took the late call
// had landed (its cursor at the telling's place), so it holds the call's
// outcome: the brief continuing it tells nothing of the call, as of a call at
// its save point.
//
// Mutation: ignore the tellings (tellings-ignored), and the call is told again.
func TestASessionThatTookALateCallIsNotToldItAgain(t *testing.T) {
	t.Parallel()
	brief := lostRelaunch(t, keptAt(5), tookLate()).Session.Brief
	if strings.Contains(brief, "jira_create") {
		t.Errorf("the session that took the late call is told it again:\n%s", brief)
	}
}

// A CAPTURE READ BEFORE ITS TELLING IS TOLD THE CALL. A capture whose cursor
// was read off the row before the write that recorded the telling (its cursor
// below the telling's place) may be of a session that never took the call, or
// took it after the capture: it is told the call, under the late calls' words.
//
// Mutation: credit a telling whatever its place (telling-place-unread), and the
// call is told to nobody.
func TestACaptureReadBeforeItsTellingIsToldTheCall(t *testing.T) {
	t.Parallel()
	brief := lostRelaunch(t, keptAt(4), tookLate()).Session.Brief
	if !strings.Contains(brief, "the late one") {
		t.Errorf("a capture read before the telling is not told the late call:\n%s", brief)
	}
}

// A SESSION THAT MAY LACK ITS BRIEF CREDITS NO TELLING. A session the record
// says was cut off may lack what its job was handed, the late call's item
// among it, whatever its cursor says: it is told the call.
//
// Mutation: credit a telling where the session lacks its brief
// (telling-when-lacking), and the call is told to nobody.
func TestASessionThatMayLackItsBriefCreditsNoTelling(t *testing.T) {
	t.Parallel()
	basis := keptAt(5)
	basis.Interrupted = true
	brief := lostRelaunch(t, basis, tookLate()).Session.Brief
	if !strings.Contains(brief, "the late one") {
		t.Errorf("a session cut off is not told the late call its job's session took:\n%s", brief)
	}
}

// A FRESH RUN IS TOLD EVERY LATE CALL. A run that starts a session of its own
// holds nothing any session took, so its brief tells the late call whatever the
// row records of another session.
//
// Mutation: credit tellings on a fresh run (telling-on-fresh), and the fresh
// run is never told the call.
func TestAFreshRunIsToldEveryLateCall(t *testing.T) {
	t.Parallel()
	again := lostRelaunch(t, keptAt(5), tookLate())
	if !strings.Contains(again.Brief, "the late one") {
		t.Errorf("the fresh brief is not told the late call:\n%s", again.Brief)
	}
}

// A JOB'S OWN COPY CREDITS EVERY TELLING OF THE ROW. The copy in the job's box,
// where its harness ended its own run, is the session of the round's newest
// job, which holds every telling on the row: its own, and those its brief
// credited or told. So it is not told a late call the row records taken; a copy
// cut off is.
//
// Mutation: credit no telling in place (telling-not-in-place), and the copy is
// told the call again.
func TestAJobsOwnCopyCreditsEveryTellingOfTheRow(t *testing.T) {
	t.Parallel()
	asked := numbered(toolname.AskAndWait, map[string]any{"question": "which branch?"}, 3, 3)
	for end, want := range map[sandbox.HarnessEnd]bool{
		sandbox.HarnessEndOwnRun: false,
		sandbox.HarnessEndCutOff: true,
	} {
		again := inPlaceRelaunch(t, inBoxCarryNamed(t, "a person's answer"), 2, func(r *runner.Resume) {
			took := tookLate()
			took.Tellings[0].Place = 1 << 20
			r.Bridged, r.Tellings, r.HarnessEnd = append(took.Bridged, asked), took.Tellings, end
		})
		if told := strings.Contains(again.Continue.Brief, "the late one"); told != want {
			t.Errorf("the copy whose harness's end is %q is told the late call its session took: %v, want %v:\n%s",
				end, told, want, again.Continue.Brief)
		}
	}
}

// A LATE CALL A BRIEF LEFT OUT IS RECORDED APART, AND NEVER CREDITED. A brief
// whose late reads pass the read cap counts the last of them without listing
// them: the launch records those apart from the reads it owes
// (sandbox.BriefTold.LateOmitted). And a session whose brief counted a late call
// so holds only that count of it, so a telling of the call is not credited to
// it however its cursor stands: it is told the call.
//
// Mutation: answer the late reads left out as owed (late-left-out-owed), or
// none at all (late-left-out-dropped); or credit a telling of a call the
// session holds as a count (telling-over-owed); and a check goes red.
func TestALateCallABriefLeftOutIsRecordedApartAndNeverCredited(t *testing.T) {
	t.Parallel()
	t.Run("recorded apart", func(t *testing.T) {
		t.Parallel()
		reads := readsOf(1, ledger.MaxReadCalls+2)
		for i := range reads {
			reads[i].Late = true
		}
		basis := keptBasis
		basis.Through = len(reads)
		again := lostRelaunch(t, basis, runner.Resume{Bridged: reads})
		want := []int{ledger.MaxReadCalls + 1, ledger.MaxReadCalls + 2}
		for name, told := range map[string]sandbox.BriefTold{"the kept session's": again.Session.Told,
			"the fresh one's": again.Continue.Fresh} {
			if !slices.Equal(told.LateOmitted, want) || len(told.Omitted) != 0 {
				t.Errorf("%s brief records late calls %v and reads %v left out, want the late %v and no read",
					name, told.LateOmitted, told.Omitted, want)
			}
		}
	})
	t.Run("never credited", func(t *testing.T) {
		t.Parallel()
		basis := keptAt(5)
		basis.LateOwed = []int{2}
		brief := lostRelaunch(t, basis, tookLate()).Session.Brief
		if !strings.Contains(brief, "the late one") {
			t.Errorf("a session holding the late call only as its brief's count is not told it:\n%s", brief)
		}
	})
}

// A TELLING NEVER COUNTS A LOAD. A late call that loaded a skill is told to no
// session that took it, but the skill's body reached that session as the item's
// account, never as the tool's answer it guards: its guard holds no load of it,
// and the covered tool is refused until the skill is loaded again.
//
// Mutation: count a load the session holds by a telling (telling-counts-load),
// and the covered tool is let through.
func TestATellingNeverCountsALoad(t *testing.T) {
	t.Parallel()
	late := loadSkill(1)
	late.Late = true
	// THE SURFACE THE JOB BEFORE WAS ARMED WITH, so slack_post is a tool of it
	// and only the guard refuses it.
	active := guarded{basis: keptBasis}.relaunch(t).Surface.Active()
	req := guarded{basis: keptBasis, state: execstate.State{ActiveTools: active},
		bridged: []runner.BridgedCall{late}, carry: func(r *runner.Resume) {
			r.Lost = &runner.Lost{Why: "its host was drained", Losses: 1, Limit: 2}
			r.Tellings = []runner.Telling{{Appended: 1, Place: keptBasis.NoteCursor}}
		}}.relaunch(t)
	if req.Session == nil {
		t.Fatal("the premise: the lost job's round continues no kept session")
	}
	bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
	if !bridged[0].Failed || !strings.Contains(bridged[0].Result, "chat-conventions") {
		t.Errorf("slack_post was answered %q, not refused by the guard, on a skill a late call loaded",
			bridged[0].Result)
	}
}

// A HOOK'S TELLING IS TOLD AGAIN IN A BOX WHOSE RELEASE THE RESUME WAS NOT
// READ IN. A late call a Claude Code hook took is held as the job's session
// showed it, which only a release whose resume this build read rebuilds: the
// briefs for a box of that release do not tell it, and the ones for any other
// box (runner.SessionContinuation.Unheld) do, recording what they left out of
// their own telling. A telling no hook recorded, an OpenCode confirmation's,
// is credited to every box, and no such briefs are built for it.
//
// Mutation: credit a hook's telling in the briefs for such a box
// (unheld-credits-hooks), or record what the other briefs left out on them
// (unheld-told-of-held), and a check goes red.
func TestAHooksTellingIsToldAgainInABoxWhoseReleaseTheResumeWasNotReadIn(t *testing.T) {
	t.Parallel()
	t.Run("told again", func(t *testing.T) {
		t.Parallel()
		took := tookLate()
		took.Tellings[0].Hook = true
		session := lostRelaunch(t, keptAt(5), took).Session
		if strings.Contains(session.Brief, "jira_create") {
			t.Errorf("the brief for a box whose release was read tells the late call its hook took:\n%s",
				session.Brief)
		}
		if session.Unheld == nil {
			t.Fatal("the continuation carries no brief for a box whose release the resume was not read in")
		}
		for _, brief := range []string{session.Unheld.Brief, session.Unheld.Replaced} {
			if !strings.Contains(brief, "the late one") {
				t.Errorf("the brief for a box whose release was not read is not told the late call:\n%s", brief)
			}
		}
	})
	t.Run("what it left out", func(t *testing.T) {
		t.Parallel()
		reads := readsOf(1, ledger.MaxReadCalls+2)
		took := runner.Resume{Bridged: reads}
		for i := range reads {
			reads[i].Late = true
			took.Tellings = append(took.Tellings, runner.Telling{Appended: i + 1, Place: 5, Hook: true})
		}
		basis := keptAt(5)
		basis.Through = len(reads)
		session := lostRelaunch(t, basis, took).Session
		if len(session.Told.LateOmitted) != 0 || session.Unheld == nil {
			t.Fatalf("the premise: the brief left out late calls %v, and carries unheld briefs %v",
				session.Told.LateOmitted, session.Unheld != nil)
		}
		if got, want := session.Unheld.Told.LateOmitted, []int{ledger.MaxReadCalls + 1,
			ledger.MaxReadCalls + 2}; !slices.Equal(got, want) {
			t.Errorf("the unheld briefs record late calls %v left out, want %v", got, want)
		}
	})
	t.Run("a confirmation's", func(t *testing.T) {
		t.Parallel()
		if session := lostRelaunch(t, keptAt(5), tookLate()).Session; session.Unheld != nil {
			t.Errorf("a continuation crediting no hook's telling carries unheld briefs:\n%s", session.Unheld.Brief)
		}
	})
}
