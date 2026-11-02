package cliagent

import (
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
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
	want := map[cliprofile.Concurrency]llm.CallConcurrency{
		cliprofile.ConcurrencyAtOnce:   llm.CallsAtOnce,
		cliprofile.ConcurrencyReadOnly: llm.CallsReadOnlyAtOnce,
		cliprofile.ConcurrencyInTurn:   llm.CallsInTurn,
	}
	if len(want) != len(cliprofile.ConcurrencyValues) {
		t.Fatalf("the profile has %d values and this test maps %d — map the new one",
			len(cliprofile.ConcurrencyValues), len(want))
	}
	seen := map[llm.CallConcurrency]cliprofile.Concurrency{}
	for _, c := range cliprofile.ConcurrencyValues {
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
