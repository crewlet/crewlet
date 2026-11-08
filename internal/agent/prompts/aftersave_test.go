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
