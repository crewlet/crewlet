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
