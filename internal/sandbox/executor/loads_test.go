package executor_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// EVERY RUN LOADS WHAT A PERSON'S SESSION OF ITS HARNESS LOADS: for every
// registered runner, a real box whose start directory and home hold
// instructions, settings, hooks, skills and MCP servers of a repository's and
// a person's own, and a run to its end through the runner's stand-in, which
// records what it found as it started. Each row shows every one there,
// Crewlet's own hook and MCP server still there, and a home entry beside
// Crewlet's own of its kind.
//
// Mutation: drop a runner's row from loadRows, and it is named.
func TestEveryRunLoadsWhatAPersonsSessionLoads(t *testing.T) {
	t.Parallel()
	for _, name := range executor.Names() {
		row, ok := loadRows[name]
		if !ok {
			t.Errorf("runner %s has no row in loadRows, so nothing shows its run loading what a person's does", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row(t)
		})
	}
}

// loadRows is, for every registered runner by name, its row of
// TestEveryRunLoadsWhatAPersonsSessionLoads, beside the runner's own tests.
var loadRows = map[string]func(t *testing.T){
	executor.KimiName: kimiLoads,
}
