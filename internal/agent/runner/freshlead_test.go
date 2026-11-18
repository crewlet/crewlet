package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A SESSION THE RUN STARTED OF ITS OWN IS TOLD ITS TASK AGAIN where it may be a
// summary: every brief that may open such a session hands its task as a lead,
// and a continuation that may be a summary tells it, once.

// freshHeading opens a fresh brief's task told again.
var freshHeading = strings.SplitN(prompts.FreshTaskAgain, "\n", 2)[0]

// freshOf is the fresh lead job's brief opened with, the seq'th launch of the
// turn, its brief having rendered no closed round.
func freshOf(job string, seq int64) sandbox.Lead {
	return sandbox.Lead{Kind: sandbox.LeadKindFresh, Job: job, Seq: seq, Round: 1, Task: &sandbox.LeadTask{}}
}

// summarizedWith is the kept capture a send-back continues told as a session
// that may be a summary (its count near Claude Code's window), the run keeping
// leads beside its record, none marked told by the job's brief.
func summarizedWith(t *testing.T, leads ...sandbox.Lead) string {
	t.Helper()
	kept := guarded{basis: keptClaude(near, false), prep: func(r *runner.Resume) {
		copyOf(sandbox.HarnessEndOwnRun, near)(r)
		for _, lead := range leads {
			r.Leads = append(r.Leads, runner.ResumeLead{Lead: lead})
		}
	}}.relaunch(t)
	if kept.Session == nil || !strings.Contains(kept.Session.Replaced, prompts.MayCompactSession) {
		t.Fatalf("the send-back hands %+v, want the kept capture told as one that may be a summary", kept.Session)
	}
	return kept.Session.Replaced
}

// EVERY BRIEF THAT MAY START A SESSION HANDS ITS TASK. A turn's first launch, a
// send-back, and a relaunch carrying a round on each hand the launch a fresh
// brief its run starts a session of its own on, wherever it does not take up the
// session the launch continues: each hands the task that brief opened with as a
// fresh lead, with the closed rounds it rendered and the review it carried, and
// a relaunch beside what carries its round on.
//
// Mutation: hand no fresh lead at the first launch or the send-back, or none
// in a relaunch's fresh brief, and a case goes red.
func TestEveryBriefThatMayStartASessionHandsItsTask(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	fresh := func(own []sandbox.Lead, round int, task sandbox.LeadTask) bool {
		return len(own) > 0 && own[0].Kind == sandbox.LeadKindFresh && own[0].Round == round &&
			own[0].Task != nil && *own[0].Task == task && own[0].Answer == nil
	}
	if _, first := launch(t, entries, buildOpts{}, 1, "", nil); !fresh(first.FreshOwn, 1, sandbox.LeadTask{}) {
		t.Errorf("a first launch hands %+v, want its fresh brief's task", first.FreshOwn)
	}
	sent := guarded{basis: keptClaude(far, false), history: []ledger.Iteration{{Iteration: 1,
		ReviewNotes: "thread the reply"}}}.relaunch(t)
	if !fresh(sent.FreshOwn, 2, sandbox.LeadTask{Rounds: 1, Reviewed: 1}) {
		t.Errorf("a send-back hands %+v, want its fresh brief's task through round 1, its review carried",
			sent.FreshOwn)
	}
	carried := relaunchedInBox(t, inBoxCarryNamed(t, "a replaced sign-in"), sandbox.HarnessEndCutOff, nil, nil)
	if own := carried.Continue.Fresh.Own; !fresh(own, 1, sandbox.LeadTask{}) || carried.FreshOwn != nil {
		t.Errorf("a relaunch carrying its round on hands %+v in its fresh brief and %+v beside, want the fresh "+
			"brief's task first in it", own, carried.FreshOwn)
	}
}

// A FRESH TASK IS TOLD AGAIN WHERE THE SESSION MAY BE A SUMMARY. A compaction
// at the continuation's first request summarizes the message a session of the
// run's own opened with, its task; the run keeps that task, and a continuation
// that may be a summary is told every lead the run keeps, so it is told the
// task again, under words that say no turn began there and nothing of a box,
// with the answers its brief told nested under it, each once.
//
// Mutation: tell the fresh lead as the turn's boundary, leave it untold, or
// count no fresh lead among the tasks that carry answers, and a case goes red.
func TestAFreshTaskIsToldAgainWhereTheSessionMayBeASummary(t *testing.T) {
	t.Parallel()
	brief := summarizedWith(t, earlierAnswer("job-1", "use the release branch"), freshOf("job-2", 2))
	section := leadsSection(brief)
	at := strings.Index(section, freshHeading)
	if at < 0 || !strings.Contains(section[at:], "post the summary") {
		t.Fatalf("the session that may be a summary is not told its fresh brief's task again:\n%s", brief)
	}
	for _, never := range []string{"began here", "new box", "FRESH checkout"} {
		if strings.Contains(section, never) {
			t.Errorf("the fresh task told again says %q:\n%s", never, section)
		}
	}
	if n := strings.Count(brief, "use the release branch"); n != 1 ||
		!strings.Contains(section[at:], "use the release branch") {
		t.Errorf("the answer the fresh brief told is told %d times, want once, under the task:\n%s", n, brief)
	}
}

// A CUT-OFF FRESH SESSION IS NOT TOLD ITS TASK. A session the run started of
// its own holds its opening message wherever it can be continued, cut off or
// not: only one that may be a summary may lack it. So a copy cut off, its count
// far from its window, is told no fresh task its job's record did not mark,
// while the same copy near its window is.
//
// Mutation: tell a fresh lead whatever the session may be, and this goes red.
func TestACutOffFreshSessionIsNotToldItsTask(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		count *sandbox.ResumedCount
		told  bool
	}{"far from its window": {count: far}, "near it": {count: near, told: true}} {
		brief := guarded{basis: inBoxBasis, carry: func(r *runner.Resume) {
			r.Rotated = &runner.Rotated{SessionID: "sess-1", BoxKept: true}
			copyOf(sandbox.HarnessEndCutOff, tc.count)(r)
			r.Leads = []runner.ResumeLead{{Lead: freshOf("job-1", 1)}}
		}}.relaunch(t).Continue.Brief
		if strings.Contains(brief, freshHeading) != tc.told {
			t.Errorf("%s: the cut-off copy is told its fresh task again: %v, want %v:\n%s", name, !tc.told,
				tc.told, brief)
		}
	}
}

// THE TURN'S ASK IS TOLD ONCE WHERE SEVERAL LEADS KEEP IT. Every launch keeps a
// fresh lead, and a base continuation a task lead too, each rendering the turn's
// ask and its closed rounds: a summary told each would be handed them as many
// times as the turn launched. So a section tells one: the task lead where one
// opens here, with where the turn began; else the newest fresh lead that opens
// here.
//
// Mutation: tell every fresh lead, a fresh lead beside a task, or the newest
// whether it opens or not, and a case goes red.
func TestTheTurnsAskIsToldOnceWhereSeveralLeadsKeepIt(t *testing.T) {
	t.Parallel()
	unread := freshOf("job-3", 3)
	unread.Task, unread.Unread = nil, "sealed under a key this node lacks"
	task := sandbox.Lead{Kind: sandbox.LeadKindTask, Job: "job-1", Seq: 1, Round: 1, Task: &sandbox.LeadTask{}}
	for name, tc := range map[string]struct {
		leads           []sandbox.Lead
		fresh, unreadOK bool
		turn            bool
	}{
		"two fresh leads":                 {leads: []sandbox.Lead{freshOf("job-1", 1), freshOf("job-2", 2)}, fresh: true},
		"a task lead and a fresh one":     {leads: []sandbox.Lead{task, freshOf("job-2", 2)}, turn: true},
		"the newest fresh lead unread":    {leads: []sandbox.Lead{freshOf("job-2", 2), unread}, fresh: true},
		"the only fresh lead unread here": {leads: []sandbox.Lead{unread}, unreadOK: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			section := leadsSection(summarizedWith(t, tc.leads...))
			want := map[bool]int{true: 1, false: 0}[tc.fresh]
			if n := strings.Count(section, freshHeading); n != want {
				t.Errorf("the fresh task is told %d times, want %d:\n%s", n, want, section)
			}
			if strings.Contains(section, "began here") != tc.turn {
				t.Errorf("the turn's boundary told is %v, want %v:\n%s", !tc.turn, tc.turn, section)
			}
			if strings.Contains(section, prompts.LeadUnread) != tc.unreadOK {
				t.Errorf("a lead that cannot be read here is said: %v, want %v:\n%s", !tc.unreadOK, tc.unreadOK,
					section)
			}
		})
	}
}
