package prompts

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/toolname"
)

// A SESSION IS TOLD WHAT CAME AFTER ITS SAVE WHERE ANYTHING DID. The section a
// continued session reads of the calls its run made after it was saved is
// there wherever the run made any: calls its record lists, and calls its record
// dropped from a long run's middle, which it is told how many of, and then the
// record's tally of every call it dropped. A tally whose calls were all made
// before the save says nothing the session lacks, and is not said; and nothing
// at all after the save is no section.
//
// Mutation: say the section only where calls are listed, say the tally without
// a count of dropped calls after the save, or put the count anywhere but after
// the listed calls and before the tally, and a row goes red.
func TestASessionIsToldWhatCameAfterItsSaveWhereAnythingDid(t *testing.T) {
	t.Parallel()
	const (
		listed = "- jira_create({\"summary\":\"after the save\"}) → success"
		tally  = "- (+150 further call(s) the run made, which its record did not keep: slack_post ×150)"
	)
	dropped := droppedAfter(40, false)
	for name, tc := range map[string]struct {
		after AfterSave
		want  []string
	}{
		"nothing after the save":                     {after: AfterSave{}},
		"a tally of calls made before the save only": {after: AfterSave{Tally: tally}},
		"calls listed after the save":                {after: AfterSave{Calls: listed}, want: []string{listed}},
		"calls dropped after the save": {after: AfterSave{Dropped: 40, Tally: tally},
			want: []string{dropped, tally}},
		"both": {after: AfterSave{Calls: listed, Dropped: 40, Tally: tally},
			want: []string{listed, dropped, tally}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := CalledAfter(tc.after)
			if len(tc.want) == 0 {
				if got != "" {
					t.Fatalf("the session is told:\n%s\nwant nothing", got)
				}
				return
			}
			want := calledAfterHeader + "\n" + strings.Join(tc.want, "\n")
			if got != want {
				t.Fatalf("the session is told:\n%s\nwant:\n%s", got, want)
			}
		})
	}
	if one := droppedAfter(1, false); !strings.HasPrefix(one, "(+1 further call this run made") {
		t.Errorf("one dropped call is told as %q", one)
	}
}

// A CUT-OFF SESSION IS TOLD ITS CALLS AS ONES IT MAY ALREADY SHOW. A session cut
// off is told the calls after the newest point its launch is known to hold,
// not after where it ends (internal/sandbox's savepoint.go), so some of them
// may be in it: its section never says they are not in it, says a call shown
// both there and in the session is one call, made once, and counts the calls a
// long run's record dropped since that point rather than since a save. A
// session saved where it ends keeps the words that say the calls are not in
// it.
//
// Mutation: tell a cut-off session its calls under the words of one saved
// where it ends, or its dropped calls in a save's words, and its case goes
// red; tell every session the cut-off words, and the saved case does.
func TestACutOffSessionIsToldItsCallsAsOnesItMayAlreadyShow(t *testing.T) {
	t.Parallel()
	const (
		listed = "- slack_post({\"text\":\"deployed v2\"}) → success"
		tally  = "- (+150 further call(s) the run made, which its record did not keep: slack_post ×150)"
	)
	cutOff := CalledAfter(AfterSave{Calls: listed, Dropped: 40, Tally: tally, Interrupted: true})
	if want := calledSinceHeader + "\n" + listed + "\n" + droppedAfter(40, true) + "\n" + tally; cutOff != want {
		t.Errorf("a cut-off session is told:\n%s\nwant:\n%s", cutOff, want)
	}
	for _, untrue := range []string{"so they are not in it", "after the session above was saved",
		"before the save "} {
		if strings.Contains(cutOff, untrue) {
			t.Errorf("a cut-off session is told %q, which is not true of calls it may already show:\n%s", untrue,
				cutOff)
		}
	}
	for _, said := range []string{"may show some of these calls", "ONE call, made once",
		"(+40 further calls this run made since that point", "counts calls made before that point"} {
		if !strings.Contains(cutOff, said) {
			t.Errorf("a cut-off session is not told %q:\n%s", said, cutOff)
		}
	}
	saved := CalledAfter(AfterSave{Calls: listed, Dropped: 40, Tally: tally})
	if want := calledAfterHeader + "\n" + listed + "\n" + droppedAfter(40, false) + "\n" + tally; saved != want ||
		strings.Contains(saved, "may show some of these calls") {
		t.Errorf("a session saved where it ends is told:\n%s\nwant:\n%s", saved, want)
	}
}

// A LATE CALL IS TOLD UNDER WORDS OF ITS OWN, WHATEVER THE SESSION'S END. A call
// the record took only after the run that made it was replaced, or one its run
// gave up on before it returned, is told by no point, so neither heading of the
// calls after a save is true of it: the session may show it with no outcome, as
// interrupted or as failed — where it ends, or cut off — and an earlier brief
// may have told it already. So it is said to be one call, made once, whose
// outcome here stands, in a section of its own after the calls since the save,
// under the same words for a session saved where it ends and for one cut off,
// which say its run gave up on it or was cut off or replaced; alone where
// nothing came after the save; and a successful submission among such calls is
// said to be no submission of the work, so the run makes its own.
//
// Mutation: tell the late calls under the after-save heading, drop their
// section where nothing else came after the save, or leave the submission
// line out, and a case goes red.
func TestALateCallIsToldUnderWordsOfItsOwnWhateverTheSessionsEnd(t *testing.T) {
	t.Parallel()
	const (
		listed = "- slack_post({\"text\":\"after the save\"}) → success"
		late   = "- jira_create({\"summary\":\"the late one\"}) → success"
		submit = "- submit_work({\"outcome\":\"delivered\"}) → success"
	)
	for _, interrupted := range []bool{false, true} {
		header := calledAfterHeader
		if interrupted {
			header = calledSinceHeader
		}
		got := CalledAfter(AfterSave{Calls: listed, Late: late, Interrupted: interrupted})
		if want := header + "\n" + listed + "\n\n" + lateCallsHeader + "\n" + late; got != want {
			t.Errorf("a session (cut off: %v) is told:\n%s\nwant:\n%s", interrupted, got, want)
		}
	}
	if got := CalledAfter(AfterSave{Late: late}); got != lateCallsHeader+"\n"+late {
		t.Errorf("a session whose only untold call is late is told:\n%s\nwant its section alone", got)
	}
	for _, said := range []string{"ONE call, made once", "outcome told here stands", "gave up on them",
		"no outcome, as still running, as interrupted or as failed"} {
		if !strings.Contains(lateCallsHeader, said) {
			t.Errorf("a late call's heading does not say %q:\n%s", said, lateCallsHeader)
		}
	}
	if strings.Contains(lateCallsHeader, "not in it") {
		t.Errorf("a late call's heading says it is not in the session, which may show it:\n%s", lateCallsHeader)
	}
	got := CalledAfter(AfterSave{Late: late + "\n" + submit, LateSubmission: true})
	if want := lateCallsHeader + "\n" + late + "\n" + submit + "\n" + lateSubmission; got != want {
		t.Errorf("a late submission is told:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(lateSubmission, "does NOT stand") {
		t.Errorf("the line under a late submission does not say it does not stand: %q", lateSubmission)
	}
}

// AN EARLIER SUBMISSION IS SAID UNDER THE CALLS IT IS AMONG. Every call a
// continuation lists after its save, or since the newest point a session cut
// off is known to hold, was made before the relaunch whose brief lists it, so
// a successful `submit_work` among them is no submission of the work: the line
// saying so, and that the run makes its own, follows the list it speaks of,
// once, before the count of calls dropped after that point and its tally,
// under either heading. A list holding none is told no such line, and nor is
// a section that lists no call, whose count and tally name no call it could
// be among. A late submission beside it keeps its own line, in the late calls'
// section, so each is said once, in words true of it.
//
// Mutation: drop the line, say it where no submission is among the calls or
// where none is listed, put it after the count or the tally, or say it in the
// late calls' section, and a case goes red.
func TestAnEarlierSubmissionIsSaidUnderTheCallsItIsAmong(t *testing.T) {
	t.Parallel()
	const (
		listed = "- submit_work({\"outcome\":\"delivered\"}) → success\n" +
			"- slack_post({\"text\":\"after the save\"}) → success"
		late  = "- submit_work({\"outcome\":\"delivered\"}) → success"
		tally = "- (+150 further call(s) the run made, which its record did not keep: slack_post ×150)"
	)
	for _, interrupted := range []bool{false, true} {
		header := calledAfterHeader
		if interrupted {
			header = calledSinceHeader
		}
		got := CalledAfter(AfterSave{Calls: listed, EarlierSubmission: true, Dropped: 40, Tally: tally,
			Interrupted: interrupted})
		want := header + "\n" + listed + "\n" + earlierSubmission + "\n" + droppedAfter(40, interrupted) + "\n" + tally
		if got != want {
			t.Errorf("a session (cut off: %v) whose calls hold a submission is told:\n%s\nwant:\n%s", interrupted,
				got, want)
		}
	}
	if got := CalledAfter(AfterSave{Calls: listed, Dropped: 40, Tally: tally}); strings.Contains(got,
		earlierSubmission) {
		t.Errorf("a session whose calls hold no successful submission is told one does not stand:\n%s", got)
	}
	if got := CalledAfter(AfterSave{EarlierSubmission: true, Dropped: 40, Tally: tally}); strings.Contains(got,
		earlierSubmission) {
		t.Errorf("a section that lists no call is told a submission among them does not stand:\n%s", got)
	}
	got := CalledAfter(AfterSave{Calls: listed, EarlierSubmission: true, Late: late, LateSubmission: true})
	if want := calledAfterHeader + "\n" + listed + "\n" + earlierSubmission + "\n\n" + lateCallsHeader + "\n" +
		late + "\n" + lateSubmission; got != want {
		t.Errorf("a submission after the save beside a late one is told:\n%s\nwant:\n%s", got, want)
	}
	for _, said := range []string{"does NOT stand as this work's submission", "the work was carried on after it",
		"call `" + toolname.SubmitWork + "` yourself"} {
		if !strings.Contains(earlierSubmission, said) {
			t.Errorf("the line under an earlier submission does not say %q: %q", said, earlierSubmission)
		}
	}
	if strings.Contains(earlierSubmission, "stopped waiting") {
		t.Errorf("the line under an earlier submission says its run stopped waiting for it, which only a late "+
			"one's run did: %q", earlierSubmission)
	}
}

// A HELD SUBMISSION IS SAID FIRST, WHETHER OR NOT A CALL IS LISTED. A session
// saved past its own successful `submit_work`, or the copy in its job's box
// whose harness ended its own run, shows the call answered and lists it
// nowhere, and it stands no more than a listed one: the line saying so, and
// that the run makes its own, comes before every section, in words of the
// session above rather than of a list, once. It is said where no call is
// listed at all, as to the copy of a job that submitted and then asked, whose
// brief otherwise is the answer's lead alone; and beside a listed submission
// and a late one, each keeps its own line under its own list. Without the flag
// nothing is said of it.
//
// Mutation: drop the line, say it only where a call is listed, say it after
// the sections, or say it without the flag, and a case goes red.
func TestAHeldSubmissionIsSaidFirstWhetherOrNotACallIsListed(t *testing.T) {
	t.Parallel()
	const (
		listed = "- submit_work({\"outcome\":\"delivered\"}) → success\n" +
			"- slack_post({\"text\":\"after the save\"}) → success"
		late  = "- submit_work({\"outcome\":\"blocked\"}) → success"
		tally = "- (+150 further call(s) the run made, which its record did not keep: slack_post ×150)"
	)
	if got := CalledAfter(AfterSave{HeldSubmission: true}); got != heldSubmission {
		t.Errorf("a session holding a submission, with no call to tell, is told:\n%s\nwant:\n%s", got,
			heldSubmission)
	}
	if got, want := CalledAfter(AfterSave{HeldSubmission: true, Late: late, LateSubmission: true}),
		heldSubmission+"\n\n"+lateCallsHeader+"\n"+late+"\n"+lateSubmission; got != want {
		t.Errorf("a session holding a submission, told late calls alone, is told:\n%s\nwant:\n%s", got, want)
	}
	for _, interrupted := range []bool{false, true} {
		header := calledAfterHeader
		if interrupted {
			header = calledSinceHeader
		}
		got := CalledAfter(AfterSave{HeldSubmission: true, Calls: listed, EarlierSubmission: true, Dropped: 40,
			Tally: tally, Interrupted: interrupted, Late: late, LateSubmission: true})
		want := heldSubmission + "\n\n" + header + "\n" + listed + "\n" + earlierSubmission + "\n" +
			droppedAfter(40, interrupted) + "\n" + tally + "\n\n" + lateCallsHeader + "\n" + late + "\n" +
			lateSubmission
		if got != want {
			t.Errorf("a session (cut off: %v) holding a submission beside listed and late ones is told:\n%s\n"+
				"want:\n%s", interrupted, got, want)
		}
		if got, want := CalledAfter(AfterSave{HeldSubmission: true, Dropped: 40, Tally: tally,
			Interrupted: interrupted}), heldSubmission+"\n\n"+header+"\n"+droppedAfter(40, interrupted)+"\n"+
			tally; got != want {
			t.Errorf("a session (cut off: %v) holding a submission, told only a count of calls, is told:\n%s\n"+
				"want:\n%s", interrupted, got, want)
		}
	}
	if got := CalledAfter(AfterSave{Calls: listed, EarlierSubmission: true, Dropped: 40, Tally: tally,
		Late: late, LateSubmission: true}); strings.Contains(got, heldSubmission) {
		t.Errorf("a session that holds no submission is told one it shows does not stand:\n%s", got)
	}
	for _, said := range []string{"the session above shows", "does NOT stand as this work's submission",
		"the work was carried on after it", "call `" + toolname.SubmitWork + "` yourself"} {
		if !strings.Contains(heldSubmission, said) {
			t.Errorf("the line on a held submission does not say %q: %q", said, heldSubmission)
		}
	}
	for _, unsaid := range []string{"among them", "stopped waiting"} {
		if strings.Contains(heldSubmission, unsaid) {
			t.Errorf("the line on a held submission says %q, words true only of a listed or a late one: %q", unsaid,
				heldSubmission)
		}
	}
}

// A LATE CALL THE RECORD DROPPED IS COUNTED UNDER THE LATE CALLS' WORDS,
// whatever the session's save point: a dropped call keeps no mark and no
// number, and a count under the words of calls after a save would say the
// session's own run made them after it was saved, which no late call is. So
// the count follows the late calls in their section, after the line on a late
// submission, with the record's tally once; alone in that section where the
// record lists no late call; and beside a count of calls after the save, the
// tally is told there and not again.
//
// Mutation: leave the count out, tell it under the heading of calls after a
// save, or say the tally in both sections, and a case goes red.
func TestALateCallTheRecordDroppedIsCountedUnderTheLateCallsWords(t *testing.T) {
	t.Parallel()
	const (
		late  = "- jira_create({\"summary\":\"the late one\"}) → success"
		tally = "(dropped from the middle: jira_create ×3)"
	)
	count := droppedLate(1)
	if !strings.HasPrefix(count, "(+1 further such call,") || !strings.Contains(count, "check whether its tool") {
		t.Fatalf("one dropped late call is counted as %q", count)
	}
	got := CalledAfter(AfterSave{Late: late, LateSubmission: true, LateDropped: 1, Tally: tally})
	if want := lateCallsHeader + "\n" + late + "\n" + lateSubmission + "\n" + count + "\n" + tally; got != want {
		t.Errorf("a late call listed beside one dropped is told:\n%s\nwant:\n%s", got, want)
	}
	if got := CalledAfter(AfterSave{LateDropped: 1, Tally: tally}); got != lateCallsHeader+"\n"+count+"\n"+tally {
		t.Errorf("a session whose only untold call is a dropped late one is told:\n%s\nwant its count alone, "+
			"under the late calls' heading", got)
	}
	got = CalledAfter(AfterSave{Dropped: 2, Tally: tally, LateDropped: 1})
	if strings.Count(got, tally) != 1 || !strings.Contains(got, lateCallsHeader+"\n"+count) {
		t.Errorf("a count of calls after the save beside one of late calls is told:\n%s\nwant the late count "+
			"under its own heading and the tally once", got)
	}
}
