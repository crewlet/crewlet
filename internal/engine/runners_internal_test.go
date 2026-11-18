package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// THE DOCTOR'S LIST AND THE ENGINE'S REGISTRY ARE THE SAME LIST. `crewlet llm
// doctor` checks an executor entry's harness against executor.Names() and the
// engine builds its runner map here, with nothing connecting them: a drift
// passes the entry at the doctor and refuses the seat's first turn with "no
// harness runner registered", the very failure the doctor exists to catch.
func TestTheDoctorsRunnerListIsTheEngineList(t *testing.T) {
	t.Parallel()
	built := sandboxRunners()
	names := make([]string, 0, len(built))
	for name := range built {
		names = append(names, name)
	}
	slices.Sort(names)
	want := slices.Clone(executor.Names())
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("the engine registers %v and the doctor checks against %v", names, want)
	}
	for _, name := range want {
		if built[name] == nil {
			t.Errorf("runner %q is named and nil, so an executor on it fails at the first turn", name)
		}
	}
}

// THE BRIDGE VARIABLE IS SPELLED TWICE AND MUST MEAN ONE THING: the provider
// package names it rather than import the API package (a leaf on an edge) for
// one string, and a drift is silent, `doctor` telling an operator to set a
// variable the engine never reads.
func TestTheBridgeVariableIsSpelledOnce(t *testing.T) {
	t.Parallel()
	if got := harness.BridgeURLVar(); got != mcpbridge.BaseURLVar {
		t.Fatalf("the doctor names %q and the bridge reads %q", got, mcpbridge.BaseURLVar)
	}
}
