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
	dropped := droppedAfter(40)
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
	if one := droppedAfter(1); !strings.HasPrefix(one, "(+1 further call this run made") {
		t.Errorf("one dropped call is told as %q", one)
	}
}
