package cliagent

// SystemArgsForTest exposes the system-prompt argv renderer to the package's
// external test, which is where the file's privacy and placement are pinned.
func SystemArgsForTest(template []string, system, dir string) ([]string, error) {
	return systemArgs(template, system, dir)
}
