package executorkeep

import (
	"slices"
	"strings"
	"testing"
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
	if err := (Ending{Outcome: OutcomeCompleted, Unkept: &Unkept{Reason: "refused"}}).Validate(); err == nil {
		t.Error("an unkept capture placed nowhere was accepted")
	}
	cut := Ending{Outcome: OutcomeCompleted,
		Unkept: &Unkept{LaunchSeq: 1, CaptureSeq: 1, Reason: long}}.Fit()
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
		Unkept:  &Unkept{LaunchSeq: 1, CaptureSeq: 1, Reason: across(UnkeptReasonMaxBytes)}}.Fit()
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
