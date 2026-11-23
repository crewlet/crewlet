package executor_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// EVERY RUN LOADS WHAT A PERSON'S SESSION OF ITS HARNESS LOADS: for a runner
// by name, a real box whose start directory and home hold instructions,
// settings, hooks, skills and MCP servers of a repository's and a person's
// own, and a run to its end through the runner's stand-in, which records what
// it found as it started. Each row shows every one there, Crewlet's own hook
// and MCP server still there, and a home entry beside Crewlet's own of its
// kind.
//
// Mutation: each row names what turns it red.
func TestEveryRunLoadsWhatAPersonsSessionLoads(t *testing.T) {
	t.Parallel()
	for _, name := range slices.Sorted(maps.Keys(loadRows)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			loadRows[name](t)
		})
	}
}

// loadRows is, for a runner by name, its row of
// TestEveryRunLoadsWhatAPersonsSessionLoads, beside the runner's own tests.
var loadRows = map[string]func(t *testing.T){
	executor.PiName: piLoads,
}
