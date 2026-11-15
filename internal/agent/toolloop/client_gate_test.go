package toolloop_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD NAMES EVERY WAY A harness RUN ENDS, AND NO OTHER. A phase's
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

// THE DASHBOARD KNOWS EVERY REASON A CALL'S ARGUMENTS ARE NOT KNOWN, both
// ways. A reason it does not know is still drawn as arguments not known, in
// general words, so a person reading a call cut off in flight is not told
// why; one it names that the engine never records is a branch nothing reaches.
func TestTheDashboardKnowsEveryReasonArgumentsAreNotKnown(t *testing.T) {
	t.Parallel()
	client, err := clientsource.Union(clientsource.Tree(t), "ArgumentsUnknown")
	if err != nil {
		t.Fatal(err)
	}
	var engine []string
	for _, a := range toolloop.ArgsUnknowns() {
		if !a.Valid() {
			t.Errorf("ArgsUnknowns lists %q, which Valid refuses", a)
		}
		engine = append(engine, string(a))
	}
	for _, a := range engine {
		if !slices.Contains(client, a) {
			t.Errorf("the engine records arguments_unknown %q and contract/calls.ts does not name it", a)
		}
	}
	for _, a := range client {
		if !slices.Contains(engine, a) {
			t.Errorf("contract/calls.ts names arguments_unknown %q, which the engine never records", a)
		}
	}
	if toolloop.ArgsUnknown("none").Valid() || toolloop.ArgsUnknown("").Valid() {
		t.Error("Valid accepts a reason the engine never records")
	}
}
