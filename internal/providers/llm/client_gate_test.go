package llm_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE DASHBOARD NAMES EVERY STOP REASON THE ENGINE RECORDS, AND NO OTHER. A
// round's `stop_reason` is what the phase card explains a cut-off or refused
// round with, and its sentences are keyed on this union: a reason only the
// engine knows reaches the screen as a round with nothing said about why it
// ended, and one only the dashboard knows is a sentence nobody will read.
func TestTheDashboardKnowsEveryStopReason(t *testing.T) {
	t.Parallel()
	client, err := clientsource.Union(clientsource.Tree(t), "StopReason")
	if err != nil {
		t.Fatal(err)
	}
	var engine []string
	for _, r := range llm.StopReasons() {
		engine = append(engine, string(r))
	}
	for _, r := range engine {
		if !slices.Contains(client, r) {
			t.Errorf("the engine records stop reason %q and contract/stops.ts does not name it", r)
		}
	}
	for _, r := range client {
		if !slices.Contains(engine, r) {
			t.Errorf("contract/stops.ts names stop reason %q, which the engine never records", r)
		}
	}
}

// THE DASHBOARD NAMES EVERY ERROR KIND THE CLASSIFIER WRITES, AND NO OTHER. A
// failed turn's `last_error.kind` is the machine code a screen turns into a
// plain sentence (`FAILURE_WORDS` in lib/seats.ts, typed over the union this
// reads): a kind only the engine knows reaches a reader as the generic
// sentence, and one only the dashboard knows is words nothing reaches.
//
// EVERY KIND BY ITS SPELLING. [llm.ErrorKind] is an iota with a String that
// spells every named kind and falls back to "fatal", so walking well past the
// last constant collects each spelling exactly once, and a kind added at the
// end is read without this test naming it.
func TestTheDashboardKnowsEveryErrorKind(t *testing.T) {
	t.Parallel()
	client, err := clientsource.Union(clientsource.Tree(t), "ProviderErrorKind")
	if err != nil {
		t.Fatal(err)
	}
	var engine []string
	for k := llm.ErrorKind(0); k < 64; k++ {
		if s := k.String(); !slices.Contains(engine, s) {
			engine = append(engine, s)
		}
	}
	for _, k := range engine {
		if !slices.Contains(client, k) {
			t.Errorf("the engine records error kind %q and contract/failures.ts does not name it", k)
		}
	}
	for _, k := range client {
		if !slices.Contains(engine, k) {
			t.Errorf("contract/failures.ts names error kind %q, which the engine never records", k)
		}
	}
}
