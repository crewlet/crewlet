package cliagent

import "github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"

// SystemArgsForTest exposes the system-prompt argv renderer to the package's
// external test, which is where the file's privacy and placement are pinned.
func SystemArgsForTest(
	template []string, spec *cliprofile.SystemPromptFile, system, dir string,
) ([]string, error) {
	return systemArgs(template, spec, system, dir)
}
