package harness

import "github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"

// SystemArgsForTest exposes the system-prompt argv renderer to the package's
// external test, which is where the file's privacy and placement are pinned.
func SystemArgsForTest(ch harnessprofile.Channel, system, dir string) ([]string, error) {
	return systemArgs(ch, system, dir)
}
