package config

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// Config accepts a closed set of agent names; cliprofile ships the profiles
// for them. A name accepted by validation with no profile behind it is a
// company that passes `crewlet validate` and fails at its first turn, which is
// the worst possible place to learn about it.
//
// HELD HERE rather than beside the profiles, because cliprofile exists so that
// config can judge an entry against its profile: config imports it, and a test
// inside cliprofile reaching back into config would be an import cycle.
func TestEveryConfiguredAgentNameHasAProfile(t *testing.T) {
	t.Parallel()
	// Compared as plain strings: config's set is typed (CLIAgentName) so a
	// document cannot name one it does not define, while cliprofile keys its
	// registry by string. The two lists still have to be the same list.
	shipped := cliprofile.BuiltinNames()
	accepted := make([]string, len(CLIAgentNames))
	for i, name := range CLIAgentNames {
		accepted[i] = string(name)
	}
	for _, name := range accepted {
		if !slices.Contains(shipped, name) {
			t.Errorf("config accepts cli.agent %q but no profile ships for it", name)
		}
	}
	for _, name := range shipped {
		if !slices.Contains(accepted, name) {
			t.Errorf("profile %q ships but config refuses it as cli.agent", name)
		}
	}
}
