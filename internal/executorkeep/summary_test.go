package executorkeep

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
)

// A SUMMARY PAST ITS BOUND KEEPS ITS END, marked at its start and never cut
// inside a rune or a marker the redaction wrote: what to do next is last in
// every summary shape, and a marker in part reads as words of the summary's
// own. Within the bound it is kept whole and unmarked.
//
// Mutation: keep the opening, count the mark outside the bound, or cut inside
// a marker, and a case goes red.
func TestASummaryPastItsBoundKeepsItsEnd(t *testing.T) {
	t.Parallel()
	if kept, cut := SummaryTail("short"); kept != "short" || cut {
		t.Fatalf("a summary within its bound is kept as %q (cut %v), want it whole", kept, cut)
	}
	end := "## Next Move\n\nRerun the cache test."
	long := strings.Repeat("é", SummaryMaxBytes) + end
	kept, cut := SummaryTail(long)
	switch {
	case !cut || !strings.HasPrefix(kept, summaryCutMark) || !strings.HasSuffix(kept, end):
		t.Fatalf("a summary past its bound kept %q… (cut %v), want its end, marked", kept[:16], cut)
	case len(kept) > SummaryMaxBytes || !utf8.ValidString(kept):
		t.Fatalf("the kept part is %d bytes (valid UTF-8 %v), want at most %d of whole runes",
			len(kept), utf8.ValidString(kept), SummaryMaxBytes)
	}
	marked := redact.Secrets("token ghp_" + strings.Repeat("A", 36))
	at := strings.Index(marked, redact.Marker)
	if at < 0 {
		t.Fatalf("the premise: %q holds no marker", marked)
	}
	// THE CUT WOULD FALL ONE BYTE INTO THE MARKER, and moves on past it.
	after := SummaryMaxBytes - len(summaryCutMark) + at + 1 - len(marked)
	text := strings.Repeat("y", 10) + marked + strings.Repeat("z", after)
	if kept, _ := SummaryTail(text); strings.Contains(kept, "REDACTED") || strings.Contains(kept, "]") {
		t.Fatalf("the cut fell inside a marker: %q", kept[:min(len(kept), 40)])
	}
}

// AN ENDING REFUSES A SUMMARY NO READER COULD OPEN OR PLACE: no object, one
// past a sealed summary's bound, no keyring key, no instant, no bytes kept,
// more kept than it holds or past a kept summary's bound, or no harness; and
// takes one that names all of them. Such a summary is not Within, while the
// ending's other fields leave the ending Within, so a reader drops the summary
// alone.
//
// Mutation: drop a check from Summary.Validate, or count the summary in
// Ending.Within, and its case is taken.
func TestAnEndingRefusesASummaryNoReaderCouldOpen(t *testing.T) {
	t.Parallel()
	whole := func() Summary {
		return Summary{Object: objstore.Object{Key: objstore.KeyAt(time.Unix(1_700_000_000, 0)),
			Hash: objstore.Hash(strings.Repeat("ab", 32)), Size: 512}, ContentKey: "k1",
			At: time.Unix(1_700_000_000, 0).UTC(), Bytes: 400, Kept: 400, Harness: "claude-code", Session: "s-1"}
	}
	ok := whole()
	if err := (Ending{Outcome: OutcomeCompleted, Summary: &ok}).Validate(); err != nil {
		t.Fatalf("an ending naming a whole summary was refused: %v", err)
	}
	for name, broken := range map[string]func(*Summary){
		"no object":           func(s *Summary) { s.Object = objstore.Object{} },
		"past its bound":      func(s *Summary) { s.Object.Size = SummarySealedMaxBytes + 1 },
		"no keyring key":      func(s *Summary) { s.ContentKey = "" },
		"no instant":          func(s *Summary) { s.At = time.Time{} },
		"no size":             func(s *Summary) { s.Bytes = 0 },
		"no bytes kept":       func(s *Summary) { s.Kept = 0 },
		"more kept than held": func(s *Summary) { s.Kept = s.Bytes + 1 },
		"kept past its bound": func(s *Summary) { s.Bytes, s.Kept = SummaryMaxBytes+2, SummaryMaxBytes+1 },
		"no harness":          func(s *Summary) { s.Harness = "" },
		"a session too long":  func(s *Summary) { s.Session = strings.Repeat("s", SessionIDMaxBytes+1) },
	} {
		s := whole()
		broken(&s)
		e := Ending{Outcome: OutcomeCompleted, Summary: &s}
		if e.Validate() == nil || s.Within() || !e.Within() {
			t.Errorf("%s: the ending was taken, or its summary was Within, or the rest of it was not", name)
		}
	}
}
