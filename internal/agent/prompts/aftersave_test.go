package prompts

import (
	"strings"
	"testing"
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
		"no outcome, as interrupted or as failed"} {
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
