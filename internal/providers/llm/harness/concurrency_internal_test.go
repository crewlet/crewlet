package harness

import (
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// EVERY WAY A PROFILE SAYS A CLI MAKES ONE RESPONSE'S CALLS REACHES THE HOST AS
// ITS OWN, AND NO TWO AS ONE. The profile's words are its own (at_once is a
// value there, the zero value in the contract), so the backend translates; a
// value that fell through to the default would tell the host a CLI making its
// calls in turn makes them at once, and the host would wait for siblings that
// never come.
//
// Mutation: drop a case from callConcurrency, and its value reads as at once.
func TestEveryProfileConcurrencyReachesTheHostAsItsOwn(t *testing.T) {
	t.Parallel()
	want := map[harnessprofile.Concurrency]llm.CallConcurrency{
		harnessprofile.ConcurrencyAtOnce:   llm.CallsAtOnce,
		harnessprofile.ConcurrencyReadOnly: llm.CallsReadOnlyAtOnce,
		harnessprofile.ConcurrencyInTurn:   llm.CallsInTurn,
	}
	if len(want) != len(harnessprofile.ConcurrencyValues) {
		t.Fatalf("the profile has %d values and this test maps %d — map the new one",
			len(harnessprofile.ConcurrencyValues), len(want))
	}
	seen := map[llm.CallConcurrency]harnessprofile.Concurrency{}
	for _, c := range harnessprofile.ConcurrencyValues {
		got := callConcurrency(c)
		if !got.Valid() || got != want[c] {
			t.Errorf("%q reaches the host as %q, want %q", c, got, want[c])
		}
		if other, dup := seen[got]; dup {
			t.Errorf("%q and %q both reach the host as %q", c, other, got)
		}
		seen[got] = c
	}
}
