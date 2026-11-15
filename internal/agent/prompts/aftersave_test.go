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
	for _, said := range []string{"ONE call, made once", "outcome told here stands", "gave up on it",
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

// A CALL WHOSE OUTCOME IS UNKNOWN IS NEVER TOLD IT FINISHED. A call still in
// flight as its round was read is told among the late calls, failed, its line
// saying its outcome is unknown and that what it did may have taken effect
// (internal/agent/runner's UnfinishedNote). The section's words are true of it
// as of the others: the heading, and the words under it, name a call whose
// outcome is unknown beside one its run stopped waiting for; they say a call
// finished only as what it had not done when its run stopped waiting for it;
// and they say a call marked `→ error` did not take effect only with the
// exception, in the line's own terms, of one that says its outcome is unknown,
// which may have, and is to be checked before it is made again. Under words
// that said the calls finished after their run stopped waiting for them and
// that every error did not take effect, a run that trusted them made such a
// call again: the duplicate the section exists to prevent. So are the words a
// run that starts a session of its own reads them under, carrying the round on
// ([lateBeforeHeader]).
//
// Mutation: restore those words, drop the exception, or name only the calls
// their run stopped waiting for, in the heading or under it, of either
// section, and its case goes red.
func TestACallWhoseOutcomeIsUnknownIsNeverToldItFinished(t *testing.T) {
	t.Parallel()
	const excepted = "did not take effect, unless it says its outcome is unknown: that one may have, so check" +
		" whether it did before making it again."
	for reader, header := range map[string]string{
		"a run continuing a session":      lateCallsHeader,
		"a run of its own carrying it on": lateBeforeHeader,
	} {
		heading, words, _ := strings.Cut(header, "\n")
		for part, said := range map[string]string{"heading": heading, "words": words} {
			if !strings.Contains(said, "whose outcome is unknown") {
				t.Errorf("the late calls' %s %s reads name no call whose outcome is unknown:\n%s", part, reader,
					said)
			}
		}
		notDone := strings.NewReplacer("before it finished", "", "before they finished", "").Replace(header)
		if strings.Contains(notDone, "finished") {
			t.Errorf("the late calls' words %s reads say a call finished:\n%s", reader, header)
		}
		if !strings.Contains(words, excepted) ||
			strings.Count(words, "did not take effect") != strings.Count(words, excepted) {
			t.Errorf("the late calls' words %s reads say a failed call did not take effect without excepting "+
				"one whose outcome is unknown:\n%s", reader, header)
		}
	}
}

// A CALL WHOSE OUTCOME IS UNKNOWN IS NEVER TOLD IT DID NOT TAKE EFFECT, BY ANY
// RULE IT IS READ BY. Such a call is on its round's record failed, its line
// saying its outcome is unknown and that what it did may have taken effect
// (internal/agent/runner's UnfinishedNote), and every reader of that record
// reads the line under a rule for a call marked `→ error`: the round's reviewer
// ([ReviewHeader]), every later round's fresh brief over the prior-work ledger
// ([PriorWorkHeader]), a send-back continuing the session that made it
// ([ContinuedRoundRules]), and a run carrying the round on, among the late
// calls ([lateCallsHeader]). Each rule says a failed call did not take effect
// only with the exception, in the line's own terms, of one that says its
// outcome is unknown, which may have; and each the executor reads says to
// check whether it did before making it again, which is the executor's to do,
// so the reviewer's says only that it may have. Told that every failed call
// did not take effect, and to retry it, a reviewer would send the round back
// to make such a call again, and a run would make it again: a second post or
// delegation. A run that starts a session of its own carrying the round on
// reads such a call among the late calls too, under words of its own
// ([lateBeforeHeader]). The rules for the calls after a save, for every call
// of the round a run of its own is told, and for the reads an earlier message
// counted without listing them keep no exception ([calledAfterHeader],
// [calledSinceHeader], [calledBeforeHeader], [owedHeader]), and are true of
// them: internal/agent/runner's calledAfter tells no call whose outcome is
// unknown among them.
//
// Mutation: drop the exception from any of the rules, rule a failed call
// retried before excepting it, or leave out of an executor's rule that the
// call is checked before it is made again, and a row goes red.
func TestACallWhoseOutcomeIsUnknownIsNeverToldItDidNotTakeEffect(t *testing.T) {
	t.Parallel()
	const (
		excepted = "did not take effect, unless it says its outcome is unknown: that one may have"
		checked  = excepted + ", so check whether it did before making it again."
	)
	for reader, tc := range map[string]struct{ rules, exception string }{
		"the round's reviewer":               {ReviewHeader, excepted + "."},
		"a later round's fresh brief":        {PriorWorkHeader, checked},
		"a send-back continuing its session": {ContinuedRoundRules, checked},
		"a run carrying the round on":        {lateCallsHeader, checked},
		"a run of its own carrying it on":    {lateBeforeHeader, checked},
	} {
		said := strings.Count(tc.rules, "did not take effect")
		if said == 0 {
			t.Errorf("%s is told no rule that a failed call did not take effect, so this case reads no "+
				"exception to one: hold the rule's new words to it:\n%s", reader, tc.rules)
			continue
		}
		if said != strings.Count(tc.rules, tc.exception) {
			t.Errorf("%s is told a failed call did not take effect without excepting, as %q, one whose "+
				"outcome is unknown:\n%s", reader, tc.exception, tc.rules)
		}
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
// tally is told once, after the late count, the last: the count of calls
// after the save says the tally is below it.
//
// Mutation: leave the count out, tell it under the heading of calls after a
// save, say the tally in both sections, or say it beside the count of calls
// after the save, and a case goes red.
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
	if want := calledAfterHeader + "\n" + droppedAfter(2, false) + "\n\n" + lateCallsHeader + "\n" + count +
		"\n" + tally; got != want {
		t.Errorf("a count of calls after the save beside one of late calls is told:\n%s\nwant the late count "+
			"under its own heading and the tally once, after it:\n%s", got, want)
	}
}

// A FRESH RUN IS TOLD THE ROUND'S CALLS IN ITS OWN WORDS. A run that starts a
// session of its own carrying a round on remembers none of the round's calls
// and has no session above to look for them in, so it is told every one of
// them as calls the runs before it made, in the order they made them, under
// words that never speak of a session above, a save or a point: the calls its
// record lists, a successful submission among them said not to stand right
// after them, then how many more the record kept only in its tally; and, in a
// section of their own, the late calls and the ones whose outcome is unknown, a
// late submission said not to stand, and how many such the record dropped; the
// tally once, beside the last count, which each count says is below it. What
// only a continued session is told (that it holds a submission, the reads an
// earlier message counted, that it was cut off) is never said to a run of its
// own.
//
// Mutation: tell a run of its own its calls in a continued session's words
// (CalledBefore delegating to CalledAfter), put the submission line after the
// count, say the tally twice or beside a count before the last, or drop either
// section where it alone has something to tell, and a case goes red.
func TestAFreshRunIsToldTheRoundsCallsInItsOwnWords(t *testing.T) {
	t.Parallel()
	const (
		listed = "- submit_work({\"outcome\":\"delivered\"}) → success\n" +
			"- slack_post({\"text\":\"an earlier job's post\"}) → success"
		late  = "- jira_create({\"summary\":\"the late one\"}) → success\n- submit_work({}) → success"
		tally = "- (+30 further call(s) the run made, which its record did not keep: slack_post ×30)"
	)
	every := AfterSave{Calls: listed, EarlierSubmission: true, Dropped: 30, Tally: tally, Late: late,
		LateSubmission: true, LateDropped: 2}
	got := CalledBefore(every)
	want := calledBeforeHeader + "\n" + listed + "\n" + earlierSubmission + "\n" + droppedBefore(30) + "\n\n" +
		lateBeforeHeader + "\n" + late + "\n" + lateSubmission + "\n" + droppedLate(2) + "\n" + tally
	if got != want {
		t.Errorf("a run of its own is told:\n%s\nwant:\n%s", got, want)
	}
	for _, untrue := range []string{"session above", "saved", "since that point", "may not show", "this run made"} {
		if strings.Contains(got, untrue) {
			t.Errorf("a run of its own is told %q, which speaks of a session it does not have:\n%s", untrue, got)
		}
	}
	for name, tc := range map[string]struct {
		told AfterSave
		want string
	}{
		"no call at all":   {told: AfterSave{Tally: tally}},
		"late calls alone": {told: AfterSave{Late: late}, want: lateBeforeHeader + "\n" + late},
		"a dropped late call alone": {told: AfterSave{LateDropped: 2, Tally: tally},
			want: lateBeforeHeader + "\n" + droppedLate(2) + "\n" + tally},
		"calls dropped alone": {told: AfterSave{Dropped: 30, Tally: tally},
			want: calledBeforeHeader + "\n" + droppedBefore(30) + "\n" + tally},
		// WHAT ONLY A CONTINUED SESSION IS TOLD, never said to a run of its own.
		"a continued session's words": {told: AfterSave{HeldSubmission: true, Owed: "- slack_history() → success " +
			"(read)", OwedDropped: 3, Interrupted: true, Calls: listed},
			want: calledBeforeHeader + "\n" + listed},
	} {
		if got := CalledBefore(tc.told); got != tc.want {
			t.Errorf("%s: a run of its own is told:\n%s\nwant:\n%s", name, got, tc.want)
		}
	}
	if one := droppedBefore(1); !strings.HasPrefix(one, "(+1 further call the runs before you made,") {
		t.Errorf("one dropped call is told as %q", one)
	}
	for _, said := range []string{"The runs before you on this task made these calls", "ALREADY RAN",
		"in the order they made them"} {
		if !strings.Contains(calledBeforeHeader, said) {
			t.Errorf("the calls' heading does not say %q:\n%s", said, calledBeforeHeader)
		}
	}
}

// THE READS AN EARLIER MESSAGE COUNTED ARE TOLD AS SUCH. A message to a session
// that told its round's calls under the ledger's read cap counted the reads past
// it without listing them, so the session holds those reads only as a count; the
// next message to it lists them, first, as the oldest calls it tells, under
// words that say an earlier message counted them and that a read is run again
// for its data, never as calls made after the session was saved, which they
// are not. A successful submission the session holds is said before them; the
// reads the record has since dropped are counted under the same words; and the
// record's tally is said once, beside the last count that needs it, which each
// count says is below it.
//
// Mutation: tell the owed reads under the heading of calls after a save, after
// that section, or say the tally beside an earlier count instead or as well,
// and a case goes red.
func TestReadsAnEarlierMessageCountedAreToldAsSuch(t *testing.T) {
	t.Parallel()
	const (
		owed   = "- slack_history({\"channel\":\"C13\"}) → success (read)"
		listed = "- slack_post({\"text\":\"after the save\"}) → success"
		late   = "- jira_create({\"summary\":\"the late one\"}) → success"
		tally  = "- (+150 further call(s) the run made, which its record did not keep: slack_history ×150)"
	)
	got := CalledAfter(AfterSave{HeldSubmission: true, Owed: owed, Calls: listed, Late: late})
	want := heldSubmission + "\n\n" + owedHeader + "\n" + owed + "\n\n" + calledAfterHeader + "\n" + listed +
		"\n\n" + lateCallsHeader + "\n" + late
	if got != want {
		t.Errorf("a session owed reads beside its calls after the save is told:\n%s\nwant:\n%s", got, want)
	}
	got = CalledAfter(AfterSave{Owed: owed, OwedDropped: 2, Dropped: 3, LateDropped: 1, Tally: tally})
	want = owedHeader + "\n" + owed + "\n" + owedDropped(2) + "\n\n" + calledAfterHeader + "\n" +
		droppedAfter(3, false) + "\n\n" + lateCallsHeader + "\n" + droppedLate(1) + "\n" + tally
	if got != want {
		t.Errorf("a session owed reads the record has since dropped is told:\n%s\nwant:\n%s", got, want)
	}
	if got, want := CalledAfter(AfterSave{OwedDropped: 1, Tally: tally}),
		owedHeader+"\n"+owedDropped(1)+"\n"+tally; got != want {
		t.Errorf("a session owed one read the record dropped is told:\n%s\nwant:\n%s", got, want)
	}
	if one := owedDropped(1); !strings.HasPrefix(one, "(+1 further such read,") || !strings.Contains(one,
		"Re-run a read if you need its data now") {
		t.Errorf("one dropped owed read is told as %q", one)
	}
	if owedDropped(0) != "" {
		t.Errorf("no dropped owed read is told as %q", owedDropped(0))
	}
	for _, said := range []string{"An earlier message to the session above counted these reads",
		"without listing them", "re-run it if you need its data now, never invent it"} {
		if !strings.Contains(owedHeader, said) {
			t.Errorf("the owed reads' heading does not say %q:\n%s", said, owedHeader)
		}
	}
	if strings.Contains(owedHeader, "after the point") || strings.Contains(owedHeader, "so they are not in it") {
		t.Errorf("the owed reads' heading tells them as calls after a save:\n%s", owedHeader)
	}
}

// EVERY COUNT THAT SAYS THE TALLY IS BELOW HAS IT BELOW. A brief counts the calls
// the record dropped from its middle in as many as three places, in the order its
// sections come (the reads an earlier message counted, the calls after the save
// and the late calls; to a run of its own, the calls the runs before it made and
// the late ones), and every one of those counts says the record's tally is below
// it. So the tally is said once, after the last count, whichever counts a brief
// gives: said beside an earlier one, a later count sends the run looking below it
// for what a dropped call was, and it finds nothing there.
//
// Mutation: say the tally beside the first count that needs it, beside every
// count, or beside none, and a case goes red.
func TestEveryCountThatSaysTheTallyIsBelowHasItBelow(t *testing.T) {
	t.Parallel()
	const tally = "- (+9 further call(s) the run made, which its record did not keep: slack_post ×9)"
	for _, owedGone := range []int{0, 2} {
		for _, dropped := range []int{0, 3} {
			for _, lateDropped := range []int{0, 1} {
				told := AfterSave{OwedDropped: owedGone, Dropped: dropped, LateDropped: lateDropped, Tally: tally}
				for brief, text := range map[string]string{
					"a continued session": CalledAfter(told),
					"a run of its own":    CalledBefore(told),
				} {
					below := max(strings.LastIndex(text, "the tally below"), strings.LastIndex(text, "The tally below"))
					if below < 0 {
						if strings.Contains(text, tally) {
							t.Errorf("%s, owed %d, after %d and late %d dropped, is told the tally beside no "+
								"count:\n%s", brief, owedGone, dropped, lateDropped, text)
						}
						continue
					}
					if strings.Count(text, tally) != 1 || strings.Index(text, tally) < below ||
						!strings.HasSuffix(text, "\n"+tally) {
						t.Errorf("%s, owed %d, after %d and late %d dropped, is not told the tally once, after "+
							"the last count that says it is below:\n%s", brief, owedGone, dropped, lateDropped, text)
					}
				}
			}
		}
	}
}

// THE CALLS LISTED AGAIN ARE ONES A TOLD CALL COULD BE TAKEN FOR. A call the
// session holds that a told call could be taken for is listed again under
// words that say the session may show it, never that it shows it, since a
// save point's claim is not what a resumed session reads back, and never that
// the session lacks it; that say the told call could be taken for it, the one
// reason true wherever runner lists it, never that their arguments read alike,
// which is false of a told call in flight or a sealed one; and that the told
// call is one of its own, made as well. After the calls since the save and
// before the late calls, the owed reads and the held submission's line before
// them all; and no section at all where none is listed again.
//
// Mutation: drop the section, place it anywhere else, word it as calls the
// session may lack, say the session shows each, or say the arguments read
// alike, and this goes red.
func TestTheCallsListedAgainAreOnesAToldCallCouldBeTakenFor(t *testing.T) {
	t.Parallel()
	const (
		owed   = "- slack_history({\"channel\":\"C0\"}) → success (read)"
		listed = "- slack_post({\"text\":\"hi\"}) → success"
		shown  = "- slack_post({\"text\":\"hi\"}) → success"
		late   = "- jira_create({\"summary\":\"the late one\"}) → success"
	)
	got := CalledAfter(AfterSave{HeldSubmission: true, Owed: owed, Calls: listed, Shown: shown, Late: late,
		Interrupted: true})
	want := strings.Join([]string{heldSubmission, owedHeader + "\n" + owed, calledSinceHeader + "\n" + listed,
		shownAgainHeader + "\n" + shown, lateCallsHeader + "\n" + late}, "\n\n")
	if got != want {
		t.Errorf("a session with a call listed again is told:\n%s\nwant:\n%s", got, want)
	}
	if got := CalledAfter(AfterSave{Late: late}); strings.Contains(got, shownAgainHeader) {
		t.Errorf("a session with no call listed again is told the section:\n%s", got)
	}
	for _, said := range []string{"## Calls the session above may show, listed again",
		"The session above may show each of these calls", "names the same tool and could be taken for it",
		"a call of its own, made as well as this one", "never this one shown twice"} {
		if !strings.Contains(shownAgainHeader, said) {
			t.Errorf("the heading of the calls listed again does not say %q:\n%s", said, shownAgainHeader)
		}
	}
	for _, unsaid := range []string{"shows each", "session above shows", "read alike", "not in it",
		"may not show", "\u2014"} {
		if strings.Contains(shownAgainHeader, unsaid) {
			t.Errorf("the heading of the calls listed again says %q:\n%s", unsaid, shownAgainHeader)
		}
	}
}

// AN EARLIER ROUND'S CALL IS SAID TO BE OF AN EARLIER ROUND OF THE TURN, OR OF
// THE FIRE. A call of the turn's closed rounds that a brief lists again ends its
// line with a note that says where in the session above it may be: in an
// earlier round of this turn, or, in a schedule's conversation, whose turns are
// its fires, of this fire. In parentheses, as the ledger's own marks on a line
// are, and with no em dash.
//
// Mutation: say "turn" of a schedule's fire, or "fire" of a conversation's turn,
// and a row goes red.
func TestAnEarlierRoundsCallIsSaidToBeOfAnEarlierRoundOfTheTurnOrFire(t *testing.T) {
	t.Parallel()
	for fire, want := range map[bool]string{
		false: "(made in an earlier round of this turn)",
		true:  "(made in an earlier round of this fire)",
	} {
		if got := EarlierRoundNote(fire); got != want || strings.Contains(got, "\u2014") {
			t.Errorf("EarlierRoundNote(%t) = %q, want %q", fire, got, want)
		}
	}
}

// OTHER CALLS ARE SAID TO BE NONE OF THE LISTED ONES. Calls the session may show
// that no list of the brief names (an earlier turn's, an earlier round's in a
// summary's words, ones the record no longer lists one by one, a worker's) are
// told in a section of their own, each part only where it holds, by the tools
// it names: before every list, right after the held submission, so "below" is
// every list, and closing with words that say none of them is a listed call.
// A schedule's conversation says it in a fire's words, and nothing to tell is
// no section.
//
// Mutation: drop a part, say a turn's words of a fire, or tell the section
// after the owed reads, and a row goes red.
func TestOtherCallsAreSaidToBeNoneOfTheListedOnes(t *testing.T) {
	t.Parallel()
	const (
		head    = "## Other calls the session above may show\nThe session above may also show calls no list below holds one by one:\n"
		closing = "\nHowever alike one of them reads to a call listed below, it is a call of its own, never that one" +
			" shown twice."
		turns   = "- calls of this conversation's earlier turns, made before this turn began"
		rounds  = "- calls of this turn's earlier rounds, in full or only in a summary's words, made to `slack_post`"
		dropped = "- calls of this turn the record no longer lists one by one, made to `slack_post`, `jira_get` and" +
			" `tr_close_issue` and to tools it does not name"
		workers = "- calls of workers this turn delegated to, in their reports' words, made to tools the record" +
			" does not name"
	)
	post := ToolsNamed{Tools: []string{"slack_post"}}
	three := ToolsNamed{Tools: []string{"slack_post", "jira_get", "tr_close_issue"}, Unnamed: true}
	unnamed := ToolsNamed{Unnamed: true}
	for name, tc := range map[string]struct {
		other OtherCalls
		want  string
	}{
		"none":                       {},
		"the earlier turns alone":    {other: OtherCalls{EarlierTurns: true}, want: head + turns + closing},
		"the earlier rounds alone":   {other: OtherCalls{EarlierRounds: post}, want: head + rounds + closing},
		"the calls no longer listed": {other: OtherCalls{Unlisted: three}, want: head + dropped + closing},
		"the workers' alone":         {other: OtherCalls{Workers: unnamed}, want: head + workers + closing},
		"every part, in their order": {other: OtherCalls{EarlierTurns: true, EarlierRounds: post, Unlisted: three,
			Workers: unnamed}, want: head + turns + "\n" + rounds + "\n" + dropped + "\n" + workers + closing},
		"a part naming no tool at all": {other: OtherCalls{Unlisted: ToolsNamed{}}},
		"a schedule's fire": {other: OtherCalls{Fire: true, EarlierTurns: true, EarlierRounds: post, Unlisted: post,
			Workers: ToolsNamed{Tools: []string{"jira_get"}}},
			want: head + "- calls of this schedule's earlier fires, made before this fire began\n" +
				"- calls of this fire's earlier rounds, in full or only in a summary's words, made to `slack_post`\n" +
				"- calls of this fire the record no longer lists one by one, made to `slack_post`\n" +
				"- calls of workers this fire delegated to, in their reports' words, made to `jira_get`" + closing},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			other := tc.other
			if got := otherCalls(other); got != tc.want {
				t.Errorf("the section is:\n%s\nwant:\n%s", got, tc.want)
			}
			if strings.Contains(otherCalls(other), "\u2014") || strings.Contains(otherCalls(other), "count") {
				t.Errorf("the section carries an em dash or speaks of a count:\n%s", otherCalls(other))
			}
		})
	}

	brief := CalledAfter(AfterSave{HeldSubmission: true, Other: OtherCalls{EarlierTurns: true},
		Owed: "- jira_get({\"key\":\"OPS-1\"}) → success (read)", Late: "- slack_post({}) → success"})
	held, other, owed := strings.Index(brief, heldSubmission), strings.Index(brief, otherCallsHeader),
		strings.Index(brief, owedHeader)
	if held < 0 || other < held || owed < other {
		t.Errorf("the other calls are not told after the held submission and before the owed reads:\n%s", brief)
	}
	if brief := CalledAfter(AfterSave{Late: "- slack_post({}) → success"}); strings.Contains(brief, otherCallsHeader) {
		t.Errorf("nothing to tell of other calls is told:\n%s", brief)
	}
}
