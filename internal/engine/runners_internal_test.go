package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// THE DOCTOR'S LIST AND THE ENGINE'S REGISTRY ARE THE SAME LIST.
//
// `crewlet llm doctor` checks an agent-mode entry's CLI against
// codingagent.Names(); the engine builds its runner map here. Nothing connects
// them, and a drift fails in the direction that is hardest to see: the doctor
// passes the entry, the deploy script goes green, and the seat's first turn
// refuses with "no coding-agent runner registered" — which is precisely the
// failure the probe exists to catch, reintroduced by the check for it.
func TestTheDoctorsRunnerListIsTheEngineList(t *testing.T) {
	t.Parallel()
	built := sandboxRunners()
	names := make([]string, 0, len(built))
	for name := range built {
		names = append(names, name)
	}
	slices.Sort(names)
	want := slices.Clone(codingagent.Names())
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("the engine registers %v and the doctor checks against %v", names, want)
	}
	for _, name := range want {
		if built[name] == nil {
			t.Errorf("runner %q is named and nil, so agent mode on it fails at the first turn", name)
		}
	}
}

// THE BRIDGE VARIABLE IS SPELLED TWICE AND MUST MEAN ONE THING.
//
// The provider package names it rather than importing the API package — a leaf
// depending on an edge for one string — so this is what stops the two from
// drifting. A drift is silent: `doctor` would tell an operator to set a
// variable the engine never reads.
func TestTheBridgeVariableIsSpelledOnce(t *testing.T) {
	t.Parallel()
	if got := cliagent.BridgeURLVar(); got != mcpbridge.BaseURLVar {
		t.Fatalf("the doctor names %q and the bridge reads %q", got, mcpbridge.BaseURLVar)
	}
}
