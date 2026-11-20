package engine

import (
	"testing"
	"time"
)

// A TEST'S ANSWER STANDS FOR THE MODEL'S OWN, for a while: a model saved
// after its test passed read "Not tested", because the test ran in a fresh
// home its own sign-in never hears. The answer stands for [VerifiedLinger],
// the latest kept, and lapses after.
func TestAModelsVerifiedAnswerStandsUntilItLapses(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	at := time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC)
	if got := e.verifiedAt("sonnet", at); !got.IsZero() {
		t.Fatalf("no test yet = %v, want none", got)
	}
	e.Verified("sonnet", at)
	e.Verified("sonnet", at.Add(-time.Minute)) // an older answer is not the last
	if got := e.verifiedAt("sonnet", at.Add(30*time.Minute)); !got.Equal(at) {
		t.Errorf("half an hour on = %v, want %v", got, at)
	}
	if got := e.verifiedAt("haiku", at); !got.IsZero() {
		t.Errorf("another model = %v, want none", got)
	}
	if got := e.verifiedAt("sonnet", at.Add(VerifiedLinger+time.Second)); !got.IsZero() {
		t.Errorf("past the linger = %v, want none", got)
	}
}
