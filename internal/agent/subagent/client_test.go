package subagent_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// TestTheDashboardCountsWorkersOnTheCallTheEngineMakes holds the two words
// the dashboard reads a fan-out in flight by — the tool's name and the
// argument that lists its tasks — against the tool itself.
//
// A seat's state line says "3 workers on ENG-405" off its running call: the
// ONE record that exists while the workers run, since each worker's phase
// record lands when it finishes and the batch's summary when the last one
// does. The line matches that call on this tool's NAME and counts the array
// under this ARGUMENT, so a rename of either would leave every fan-out drawn
// as an ordinary executor, with nothing on any screen to say it had moved.
func TestTheDashboardCountsWorkersOnTheCallTheEngineMakes(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	name, err := clientsource.Scalar(tree, "DELEGATE_TOOL")
	if err != nil {
		t.Fatal(err)
	}
	tool := subagent.NewTool(subagent.Config{})
	if name != tool.Name() {
		t.Errorf("the dashboard reads a fan-out off a call named %q and the tool is %q — "+
			"change DELEGATE_TOOL in contract/wire.ts", name, tool.Name())
	}

	arg, err := clientsource.Scalar(tree, "DELEGATE_TASKS")
	if err != nil {
		t.Fatal(err)
	}
	params := tool.Parameters()
	required, _ := params["required"].([]any)
	if !slices.Contains(required, any(arg)) {
		t.Errorf("the dashboard counts the tasks under %q, which the tool's schema does "+
			"not require (it requires %v) — change DELEGATE_TASKS in contract/wire.ts", arg, required)
	}
	props, _ := params["properties"].(map[string]any)
	field, _ := props[arg].(map[string]any)
	if field["type"] != "array" {
		t.Errorf("the dashboard counts %q as an array of tasks, and the schema declares it %v",
			arg, field["type"])
	}
}
