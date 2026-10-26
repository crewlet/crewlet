package toolloop_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD NAMES EVERY WAY A CLI RUN ENDS, AND NO OTHER. A phase's
// `invocations[].outcome` is what the phase card and the waterfall head a run's
// rounds with, in words keyed on this union: an outcome only the engine records
// reaches the screen as its bare value, and one only the dashboard knows is a
// sentence no run will ever earn.
func TestTheDashboardKnowsEveryInvocationOutcome(t *testing.T) {
	t.Parallel()
	client, err := clientsource.Union(clientsource.Tree(t), "InvocationOutcome")
	if err != nil {
		t.Fatal(err)
	}
	var engine []string
	for _, o := range toolloop.InvocationOutcomes() {
		if !o.Valid() {
			t.Errorf("InvocationOutcomes lists %q, which Valid refuses", o)
		}
		engine = append(engine, string(o))
	}
	for _, o := range engine {
		if !slices.Contains(client, o) {
			t.Errorf("the engine records run outcome %q and contract/stops.ts does not name it", o)
		}
	}
	for _, o := range client {
		if !slices.Contains(engine, o) {
			t.Errorf("contract/stops.ts names run outcome %q, which the engine never records", o)
		}
	}
	if toolloop.InvocationOutcome("running").Valid() {
		t.Error("Valid accepts an outcome the engine never records")
	}
}
