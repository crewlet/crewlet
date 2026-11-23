package executor_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// loadsRows is, for each runner by name, its row of
// TestEveryRunLoadsWhatAPersonsSessionLoads: a real box with a recording
// stand-in of its harness, whose home and start directory hold what a person's
// session of that harness loads, and a run that goes to its end.
var loadsRows = map[string]func(t *testing.T){
	executor.MuseName: museLoads,
}

// A RUN LOADS WHAT A PERSON'S SESSION OF ITS HARNESS LOADS: each row plants
// instructions, settings, hooks, skills and MCP servers in the box home and
// the start directory; each reaches the harness as it starts, Crewlet's own
// hook and MCP server are there too, and a home entry survives beside
// Crewlet's.
func TestEveryRunLoadsWhatAPersonsSessionLoads(t *testing.T) {
	t.Parallel()
	for name, row := range loadsRows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row(t)
		})
	}
}
