package config

import "testing"

// AN ENTRY CANNOT SET THE EXECUTOR BLOCK: validation refuses `overrides.executor`
// at its path, since which harnesses run a seat's executor is the built-in
// profile's alone.
//
// Mutation: drop the refusal in harnessprofile.Load, and the entry loads.
func TestAnEntryCannotSetTheExecutorBlock(t *testing.T) {
	t.Parallel()
	rejects(t, assistantCompany("    assistant:\n      harness: claude-code\n      model: sonnet\n"+
		"      overrides: {executor: {runner: true, e2b_template: base}}\n", ""),
		"providers.llm.assistant.overrides.executor")
}
