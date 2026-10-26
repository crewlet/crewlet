package config_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
)

// THE MODELS SCREEN READS AN ENTRY AS THE ENGINE DOES: the auth modes and
// modes its editor and its words are keyed on are exactly the engine's closed
// sets, and what it says an unset field means is the default the engine
// applies — so a page never names claude-code, subscription or ten minutes
// for an entry the engine runs some other way.
func TestTheModelsScreenReadsAnEntryAsTheEngineDoes(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"CLIAuthMode", strs(config.CLIAgentAuthModes)},
		{"CLIMode", strs(config.CLIModes)},
	} {
		got, err := clientsource.Union(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(tc.want))) {
			t.Errorf("contract/models.ts %s is %v, want exactly the engine's %v", tc.name, got, tc.want)
		}
	}
	var unset config.LLMProvider
	mode := string(config.CLIModeHost)
	if unset.Box() {
		mode = string(config.CLIModeBox)
	}
	for _, tc := range []struct{ name, want string }{
		{"DEFAULT_CLI_AGENT", unset.AgentName()},
		{"DEFAULT_CLI_MODE", mode},
		{"DEFAULT_CLI_AUTH_MODE", string(unset.ResolvedAuthMode())},
		{"DEFAULT_CLI_TIMEOUT_SECONDS", strconv.FormatFloat(unset.Timeout(), 'f', -1, 64)},
		{"DEFAULT_CLI_MAX_CONCURRENT", strconv.Itoa(unset.Concurrency())},
	} {
		got, err := clientsource.Scalar(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("contract/models.ts %s is %q, the engine's default is %q", tc.name, got, tc.want)
		}
	}
}
