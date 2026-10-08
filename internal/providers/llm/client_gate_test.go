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
