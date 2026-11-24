package executorkeep

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// AN ENDING'S PROSE IS CUT TO ITS BOUND, MARKED, AND NEVER INSIDE A RUNE: a
// reviewer's account past its cap is recorded rather than refusing the turn's
// end, and the cut is visible to the turn that reads it.
//
// Mutation: cut with textcut.Bytes, which marks nothing, and the reader takes
// a severed account for the whole of it.
func TestAnEndingsProseIsCutToItsBoundAndMarked(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", CompletedWorkMaxBytes)
	fitted := Ending{Outcome: OutcomeCompleted, CompletedWork: long}.Fit()
	if len(fitted.CompletedWork) > CompletedWorkMaxBytes {
		t.Fatalf("the fitted account is %d bytes past its %d-byte cap",
			len(fitted.CompletedWork), CompletedWorkMaxBytes)
	}
	if !strings.HasSuffix(fitted.CompletedWork, "…") {
		t.Error("a cut account carries no marker")
	}
	if err := (Ending{Outcome: OutcomeCompleted,
		Decision: strings.Repeat("d", WordMaxBytes+1)}).Validate(); err == nil {
		t.Error("an engine word past its bound was accepted rather than refused")
	}
	if err := (Ending{Outcome: "abandoned"}).Validate(); err == nil {
		t.Error("an outcome this build does not know was accepted")
	}
	if err := (Ending{Outcome: OutcomeCompleted, Unkept: &Unkept{Loss: LossStoreRefused, Reason: "refused"}}).Validate(); err == nil {
		t.Error("an unkept capture placed nowhere was accepted")
	}
	cut := Ending{Outcome: OutcomeCompleted,
		Unkept: &Unkept{Loss: LossStoreRefused, LaunchSeq: 1, CaptureSeq: 1, Reason: long}}.Fit()
	if len(cut.Unkept.Reason) > UnkeptReasonMaxBytes || !cut.Within() {
		t.Errorf("the unkept capture's reason is %d bytes past its %d-byte cap",
			len(cut.Unkept.Reason), UnkeptReasonMaxBytes)
	}
}

// githubToken is a credential's shape the redaction knows: its body is the
// rule's thirty-six characters, so any part of it is no token it recognises.
const githubToken = "ghp_Q7wErTy9UiOp3AsDfGh5JkLzXcVbNm1PqRsT"

// A RECORD'S WORDS ARE REDACTED BEFORE THEY ARE CUT. A credential its bound
// runs across is a marker before the cut is made, never a head of itself no
// later redaction recognises — in an ask's excerpt and in every prose field
// of an ending alike — and a cut that would split the marker is made where the
// marker begins, so no marker is kept in part either. Words inside the bound
// are kept redacted, whole.
//
// Mutation: cut a record's words before redacting them, or cut through a
// marker, and a check goes red.
func TestARecordsWordsAreRedactedBeforeTheyAreCut(t *testing.T) {
	t.Parallel()
	// THE TOKEN BEGINS SIXTEEN BYTES BEFORE THE BOUND: cut first, its first
	// sixteen characters were kept, and no rule reads them as a token.
	across := func(bound int) string {
		return strings.Repeat("a", bound-16) + githubToken + " and more after it"
	}
	if got := Excerpt("push with "+githubToken, 1<<10); got != "push with [REDACTED:github-token]" {
		t.Errorf("words inside the bound are kept as %q, want them redacted, whole", got)
	}
	ask := Excerpt(across(1<<10), 1<<10)
	if len(ask) > 1<<10 || !strings.HasSuffix(ask, strings.Repeat("a", 8)+"…") ||
		strings.Contains(ask, "ghp_") || strings.Contains(ask, "[REDACTED") {
		t.Errorf("an excerpt whose bound runs across a token keeps %q, want it cut where the token's "+
			"marker begins: no head of the token, no piece of its marker", ask[max(len(ask)-40, 0):])
	}
	fitted := Ending{Outcome: OutcomeCompleted, CompletedWork: across(CompletedWorkMaxBytes),
		BlockedOn: across(BlockedOnMaxBytes), StopReason: across(StopReasonMaxBytes),
		Failure: across(FailureMaxBytes),
		Unkept:  &Unkept{Loss: LossStoreRefused, LaunchSeq: 1, CaptureSeq: 1, Reason: across(UnkeptReasonMaxBytes)}}.Fit()
	for field, prose := range map[string]string{
		"completed_work": fitted.CompletedWork, "blocked_on": fitted.BlockedOn,
		"stop_reason": fitted.StopReason, "failure": fitted.Failure, "unkept reason": fitted.Unkept.Reason,
	} {
		if strings.Contains(prose, "ghp_") || strings.Contains(prose, "Q7w") {
			t.Errorf("the ending's %s keeps the head of a token its bound ran across: %q", field,
				prose[max(len(prose)-40, 0):])
		}
	}
	if !fitted.Within() {
		t.Error("the fitted ending is not within its bounds")
	}
}

// An ending's exhausted sessions are refused past their bounds, never cut: a shortened id
// names another session.
// Mutation: drop the count, size or empty check from exhaustedWithin and a case goes red.
func TestAnEndingsExhaustedSessionsAreBounded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		sessions []string
		ok       bool
	}{
		"none":                    {ok: true},
		"at the most":             {sessions: slices.Repeat([]string{"s-1"}, ExhaustedMaxSessions), ok: true},
		"one past":                {sessions: slices.Repeat([]string{"s-1"}, ExhaustedMaxSessions+1)},
		"an empty id":             {sessions: []string{""}},
		"an id at its bound":      {sessions: []string{strings.Repeat("s", SessionIDMaxBytes)}, ok: true},
		"an id past its bound":    {sessions: []string{strings.Repeat("s", SessionIDMaxBytes+1)}},
		"an id that is not UTF-8": {sessions: []string{"s-\xff"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := Ending{Outcome: OutcomeFailed, Exhausted: tc.sessions}
			if err := e.Validate(); (err == nil) != tc.ok {
				t.Errorf("Validate = %v, want ok %v", err, tc.ok)
			}
			if e.Within() != tc.ok {
				t.Errorf("Within = %v, want %v", e.Within(), tc.ok)
			}
			if got := e.Fit().Exhausted; !slices.Equal(got, tc.sessions) {
				t.Errorf("Fit changed the ids to %q: a shortened id names another session", got)
			}
		})
	}
}

// AN ENDING'S LAST WORD IS PLACED FROM THE FIRST CAPTURE, as every capture of
// a turn is: a reader holds the row's place to it, so a last word placed at
// nothing — a zero launch or a zero capture — is no capture's place, and is
// refused by the log's writer and by its applier alike rather than read as one
// older than every row. One at the first capture or any later is kept as it
// is.
//
// Mutation: drop the last word's place check from Validate, or from Within,
// and a case goes red.
func TestAnEndingsLastWordIsPlacedFromTheFirstCapture(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		place *CapturePlace
		ok    bool
	}{
		"none":                 {ok: true},
		"the first capture":    {place: &CapturePlace{LaunchSeq: 1, CaptureSeq: 1}, ok: true},
		"a later one":          {place: &CapturePlace{LaunchSeq: 3, CaptureSeq: 7}, ok: true},
		"no launch":            {place: &CapturePlace{LaunchSeq: 0, CaptureSeq: 1}},
		"no capture":           {place: &CapturePlace{LaunchSeq: 1, CaptureSeq: 0}},
		"before the first one": {place: &CapturePlace{LaunchSeq: -1, CaptureSeq: -1}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := Ending{Outcome: OutcomeFailed, WentOn: true, LastWord: tc.place}
			if err := e.Validate(); (err == nil) != tc.ok {
				t.Errorf("Validate = %v, want ok %v", err, tc.ok)
			}
			if e.Within() != tc.ok {
				t.Errorf("Within = %v, want %v", e.Within(), tc.ok)
			}
			if got := e.Fit().LastWord; (got == nil) != (tc.place == nil) || (got != nil && *got != *tc.place) {
				t.Errorf("Fit moved the last word to %+v, want %+v", got, tc.place)
			}
		})
	}
}

// excerptOfTheWhole is [Excerpt] over the whole text's redaction: the answer
// an excerpt read from an opening must give.
func excerptOfTheWhole(text string, bound int) string {
	shown := redact.Secrets(text)
	if len(shown) <= bound {
		return shown
	}
	kept := textcut.Bytes(shown, bound-len(textcut.Opening("", bound)))
	if begin, _, inside := redact.MarkersAround(shown, len(kept)); inside {
		kept = kept[:begin]
	}
	return textcut.Opening(kept, bound)
}

// AN EXCERPT IS THE WHOLE REDACTED, THEN CUT: read from an opening of the
// redaction, it answers what cutting the whole redaction answers, with a run
// of markers at every cut near the bound, the longest marker the pass writes
// straddling where the cut falls, and past the length up to which the
// redaction reads a text whole (32 MiB). A literal [REDACTED: run in the
// text's own words longer than any marker the pass writes is not one of its
// markers; an excerpt there shows the text's own words and nothing else.
//
// Mutation: read the opening only one byte past the bound, and a marker the
// cut falls in is kept in part.
func TestAnExcerptIsTheWholeRedactedThenCut(t *testing.T) {
	t.Parallel()
	// The DigitalOcean token's marker, [REDACTED:digitalocean-token], is the
	// longest a rule writes, and longer than the token it replaces.
	do := "dop_v1_" + strings.Repeat("0a", 16)
	words := func(n int) string { return strings.Repeat("work went on ", n/13+1)[:n] }
	runs := []string{do, do + " " + githubToken, githubToken + do, "pwd=" + strings.Repeat(`x\n`, 6) + "\n" + do}
	for _, bound := range []int{64, 1 << 10} {
		for _, run := range runs {
			for at := bound - 3 - 2*len(redact.Secrets(run)); at <= bound+1; at++ {
				text := words(max(at, 0)) + " " + run + " and after it " + words(bound)
				if got, want := Excerpt(text, bound), excerptOfTheWhole(text, bound); got != want {
					t.Errorf("bound %d, %q at byte %d: got %q, want %q", bound, run, at, got, want)
				}
			}
		}
	}
	long := words(FailureMaxBytes-20) + " " + do + " " + words(33<<20)
	if got, want := Excerpt(long, FailureMaxBytes), excerptOfTheWhole(long, FailureMaxBytes); got != want {
		t.Errorf("a %d-byte text: got %q, want %q", len(long), got[max(0, len(got)-80):],
			want[max(0, len(want)-80):])
	}
	const bound = 1 << 10
	literal := words(bound-20) + redact.Marker + strings.Repeat("a", 2*redact.LongestMarker()) + "] after"
	got := Excerpt(literal, bound)
	if len(got) > bound || !strings.HasPrefix(literal, strings.TrimSuffix(got, "…")) {
		t.Errorf("a literal marker run across the cut gave %q, want an opening of the text's own words",
			got[max(0, len(got)-80):])
	}
}
