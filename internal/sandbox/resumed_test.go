package sandbox

import (
	"slices"
	"testing"
)

// EVERY MARK THIS BUILD WRITES IS VALID, `compacted` among them, and a word it
// does not know is not: it reads as a mark all the same.
//
// Mutation: leave a word out of its list, and a case goes red.
func TestEveryCountMarkIsValid(t *testing.T) {
	t.Parallel()
	for _, m := range []CountMark{CountUnread, CountRelease, CountPrecompact, CountCompacted, CountUsage,
		CountCleared, CountPDF, CountMedia} {
		if !m.Valid() {
			t.Errorf("%q is not valid", m)
		}
	}
	if CountMark("a-later-builds-mark").Valid() {
		t.Error("a word this build does not know is valid")
	}
	if !slices.Contains(CountMarks, CountCompacted) {
		t.Error("a compaction with no reply after it is no mark")
	}
}

// THE WORSE OF TWO COUNTS IS THE ONE THAT TELLS MORE ([ResumedCount.Worse]):
// none, the first mark in precedence, or the larger bound beside the smaller
// window.
//
// Mutation: take the first count's bound, or the larger window, and a case goes
// red.
func TestTheWorseOfTwoCountsIsTheOneThatTellsMore(t *testing.T) {
	t.Parallel()
	small, large := &ResumedCount{Tokens: 10_000, Window: 1_000_000}, &ResumedCount{Tokens: 160_000, Window: 200_000}
	if got := small.Worse(large); got == nil || got.Tokens != 160_000 || got.Window != 200_000 || got.Mark != "" {
		t.Errorf("the worse of %+v and %+v is %+v, want 160,000 under 200,000", small, large, got)
	}
	marked := &ResumedCount{Mark: CountMedia}
	if got := large.Worse(marked); got == nil || got.Mark != CountMedia || got.Tokens != 0 {
		t.Errorf("the worse of a bound and a mark is %+v, want the mark", got)
	}
	if got := (&ResumedCount{Mark: CountMedia}).Worse(&ResumedCount{Mark: CountUnread}); got.Mark != CountUnread {
		t.Errorf("the worse of two marks is %q, want the first in precedence", got.Mark)
	}
	if got := small.Worse(nil); got != nil {
		t.Errorf("the worse of a count and none is %+v, want none", got)
	}
}

// A JOB THAT RESTARTED ITS SESSION IS KNOWN BY ITS NEWEST RESTART'S IDENTITY,
// against the one its launch handed it. A compaction the job made moves it; a
// trim that kept the newest restart does not, however many older ones it
// dropped; a fresh session that restarted, and an unknown basis with any
// restart, have restarted; a marked count is read by its restart as a bound is,
// since the mark says nothing of where the thread restarts; and a count not read
// whole cannot say, which reads as restarted.
//
// Mutation: drop the comparison, compare a count of restarts, read an unread
// count as not restarted, or a marked one as unread, and a case goes red.
func TestAJobThatRestartedItsSessionIsKnownByItsNewestRestart(t *testing.T) {
	t.Parallel()
	read := func(restart string) *ResumedCount { return &ResumedCount{Tokens: 1, Restart: new(restart)} }
	marked := func(restart string) *ResumedCount { return &ResumedCount{Mark: CountMedia, Restart: new(restart)} }
	for name, tc := range map[string]struct {
		basis *string
		count *ResumedCount
		want  bool
	}{
		"a job that compacted":                {basis: new("u-1"), count: read("u-9"), want: true},
		"a trim with no restart of the job's": {basis: new("u-1"), count: read("u-1")},
		"a fresh session that restarted":      {basis: new(""), count: read("u-9"), want: true},
		"a fresh session that did not":        {basis: new(""), count: read("")},
		"a basis not known, a restart":        {count: read("u-9"), want: true},
		"a basis not known, none":             {count: read("")},
		"a marked count, the restart handed":  {basis: new("u-1"), count: marked("u-1")},
		"a marked count, a restart of its own": {basis: new("u-1"), count: marked("u-9"),
			want: true},
		"a count that did not read the whole": {basis: new("u-1"), count: &ResumedCount{Mark: CountUnread},
			want: true},
		"no count": {basis: new("u-1"), want: true},
	} {
		if got := RestartedSince(tc.basis, tc.count); got != tc.want {
			t.Errorf("%s: restarted %v, want %v", name, got, tc.want)
		}
	}
}
