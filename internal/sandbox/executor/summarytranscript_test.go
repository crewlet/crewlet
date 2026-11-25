package executor_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A SUMMARY PAST AN ENTRY IS READ FROM ITS END: what to do next comes last in
// every summary, so a summary past its reader's entry bound is its one entry's
// END, marked Tail and cut, its whole size and where its part begins said; a
// summary within the bound is its entry whole; and a match is the window
// around the words, never Tail.
//
// Mutation: hand out the entry's opening, or leave Tail unset, and a case goes
// red.
func TestASummaryPastAnEntryIsReadFromItsEnd(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 30, 12, 1, 0, 0, time.UTC)
	const end = "## Next Move\n\nRerun the cache test."
	for _, by := range []executor.Reader{executor.ReadBySeat, executor.ReadByPerson} {
		bound := by.Bounds().EntryBytes
		long := "THE OPENING. " + strings.Repeat("a middle word. ", bound/15+1) + end
		read, err := executor.SummaryTranscript(by, executor.TranscriptFilter{}, 0, at, long)
		if err != nil || len(read.Entries) != 1 {
			t.Fatalf("%s: read %d entries (%v), want the summary's one", by, len(read.Entries), err)
		}
		e, place := read.Entries[0], read.Places[0]
		switch {
		case !e.Tail || !e.Cut || e.Bytes != len(long):
			t.Errorf("%s: the entry is tail %v, cut %v, %d bytes; want its end of %d, cut", by, e.Tail, e.Cut,
				e.Bytes, len(long))
		case !strings.HasSuffix(e.Text, end) || strings.Contains(e.Text, "THE OPENING") || len(e.Text) > bound:
			t.Errorf("%s: the entry holds %q…%q (%d bytes), want the summary's end within %d", by,
				e.Text[:min(len(e.Text), 24)], e.Text[max(0, len(e.Text)-24):], len(e.Text), bound)
		case e.Text != "…"+long[place.TextFrom:]:
			t.Errorf("%s: the entry's part begins at %d, which is not where its text begins", by, place.TextFrom)
		}

		whole, err := executor.SummaryTranscript(by, executor.TranscriptFilter{}, 0, at, end)
		if err != nil || len(whole.Entries) != 1 || whole.Entries[0].Text != end || whole.Entries[0].Tail ||
			whole.Entries[0].Cut {
			t.Errorf("%s: a summary within its bound reads %+v (%v), want it whole", by, whole.Entries, err)
		}

		match, err := executor.SummaryTranscript(by, executor.TranscriptFilter{Terms: []string{"opening"}}, 0, at,
			long)
		if err != nil || len(match.Entries) != 1 || match.Entries[0].Tail ||
			!strings.Contains(match.Entries[0].Text, "OPENING") {
			t.Errorf("%s: a match reads %+v (%v), want the window around the words", by, match.Entries, err)
		}
	}
}
