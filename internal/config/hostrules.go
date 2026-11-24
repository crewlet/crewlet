package config

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// ValidateHost reports the rules that depend on the process judging the
// document, not the document alone: today, that a harness whose runner refuses
// root cannot run an executor as root ([harnessprofile.Executor.RefusesRoot]).
//
// SEPARATE FROM [Company.Validate] ON PURPOSE: that is a pure function of the
// document, valid or not on every machine. A launch also depends on who the
// engine runs as, so callers that judge one (`crewlet validate`, boot, apply, an
// API write) pass euid, the engine's effective user id.
//
// RUNNABLE, NOT ADMISSION: a revision that cannot launch on this node is not one
// it can run, so it is refused at apply instead of placing every seat on a box
// that fails its first launch.
func (c *Company) ValidateHost(euid int) error {
	if euid != 0 {
		return nil
	}
	var p problems
	for _, key := range c.executorKeys() {
		entry := c.Providers.LLM[key]
		harness := entry.HarnessName()
		cell := c.EntryCell(key)
		// An entry whose cell is unset or names no cell is refused by its own
		// rule, and the double runs no harness at all.
		if !slices.Contains(Placements, cell) || (c.Providers.Sandbox != nil && c.Providers.Sandbox.Fake) {
			continue
		}
		if problem := RootProblem(harness, key, cell); problem != "" {
			p.add(at(entryPath(key), "run_in"), ErrConflict, "%s", problem)
		}
	}
	return p.err()
}

// RootProblem is the one sentence both `crewlet llm doctor` and validation
// give an operator whose engine runs as root with an executor on harness in
// cell, where the harness refuses for uid 0 the flag its runner starts it with
// ([harnessprofile.Executor.RefusesRoot]); "" for a harness that refuses none.
// Every launch would fail at a seat's first work, and neither a login nor a
// smoke run catches it: the smoke run uses the deny-local-tools profile, not
// the executor's mode.
//
// EVERY CELL runs the harness as the engine's own user, so an engine running
// as root runs it as root: a `direct` box is a process tree of the engine, and
// a `container` box is started with `--user <engine uid>` on a rootful runtime
// and is mapped onto the engine's user by a rootless one, uid 0 either way.
func RootProblem(harness, key string, cell Placement) string {
	runner, _ := harnessprofile.ExecutorOf(harness)
	if runner.RefusesRoot == "" {
		return ""
	}
	return fmt.Sprintf("this engine runs as root, and providers.llm.%s runs a %s "+
		"executor in the %s cell, where the harness runs as the engine's own user, "+
		"as it does in every cell: %s refuses %s for root, so every executor launch "+
		"on this entry would fail at its seat's first work. Run the engine as a "+
		"non-root user",
		key, harness, cell, harness, runner.RefusesRoot)
}
