package executor_test

import "testing"

// loadsRows is, for a runner by name, its row of
// [TestARunLoadsWhatAPersonsSessionLoads]: a real box whose recording stand-in
// runs a whole run, with instructions, configuration, hooks, skills and MCP
// servers planted in the run's start directory and in the box's home.
var loadsRows = map[string]func(t *testing.T){
	"opencode": openCodeLoads,
}

// A RUN LOADS WHAT A PERSON'S SESSION OF ITS HARNESS LOADS: what the start
// directory and the box's home hold of each kind reaches the harness, Crewlet's
// own hook and MCP server are still there, and an entry of the home's survives
// beside Crewlet's. Each row says what breaks it.
func TestARunLoadsWhatAPersonsSessionLoads(t *testing.T) {
	t.Parallel()
	for name, row := range loadsRows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row(t)
		})
	}
}
