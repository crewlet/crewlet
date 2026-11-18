package config

import "fmt"

// ValidateHost reports the rules that depend on the process judging the
// document rather than on the document alone: today, that Claude Code cannot
// run an executor as root.
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
		if Harness(entry.HarnessName()) != HarnessClaudeCode {
			continue
		}
		cell := c.EntryCell(key)
		if !RunsAsEngineUser(cell) || (c.Providers.Sandbox != nil && c.Providers.Sandbox.Fake) {
			continue
		}
		p.add(at(entryPath(key), "run_in"), ErrConflict, "%s", RootClaudeCodeProblem(key, cell))
	}
	return p.err()
}

// RunsAsEngineUser reports whether a box in cell runs its harness as the
// engine's own user.
//
// The cells whose box runs as the engine's own user, so that
// an engine running as root runs the harness as root: a `direct` box is a
// process tree of the engine, and a `container` box is started with
// `--user <engine uid>` on a rootful runtime and is mapped onto the engine's
// user by a rootless one, uid 0 either way. An e2b box is a remote sandbox with
// a user of its own.
func RunsAsEngineUser(cell Placement) bool {
	return cell == PlacementDirect || cell == PlacementContainer
}

// RootClaudeCodeProblem is the one sentence both `crewlet llm doctor` and
// validation give an operator whose engine runs as root with a Claude Code
// executor in a box that runs as the engine's user.
//
// The runner starts the harness with `--permission-mode bypassPermissions`,
// which Claude Code refuses for uid 0 ("--dangerously-skip-permissions cannot
// be used with root/sudo privileges for security reasons"), so every launch
// fails at a seat's first work. Neither a login nor a smoke run catches it: the
// smoke run uses the deny-local-tools profile, not the executor's mode.
func RootClaudeCodeProblem(key string, cell Placement) string {
	return fmt.Sprintf("this engine runs as root, and providers.llm.%s runs a Claude Code "+
		"executor in the %s cell, where the harness runs as the engine's own user: "+
		"Claude Code refuses --permission-mode bypassPermissions for root, so every "+
		"executor launch on this entry would fail at its seat's first work. Run the "+
		"engine as a non-root user, or put this entry in `run_in: e2b`",
		key, cell)
}
