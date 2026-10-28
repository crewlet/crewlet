package config_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
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

// EVERY BUILT-IN CLI HAS A NAME A PERSON READS. A seat's condition names the
// CLI its entry drives ("Claude Code subscription limit reached"), and a
// profile the table lacked would be shown by its machine name, the one word a
// person reading the Inbox has never seen. Exactly the built-in profiles, so a
// name left behind for a profile removed is caught too, bar `custom`: an entry
// on it describes a CLI of its own, which the dashboard calls by the entry's
// key, the only name it has.
func TestEveryBuiltInCLIHasAProductName(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "CLI_PRODUCTS")
	if err != nil {
		t.Fatal(err)
	}
	got, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.DeleteFunc(cliprofile.BuiltinNames(), func(name string) bool {
		return name == string(config.CLICustom)
	})
	if !slices.Equal(slices.Sorted(slices.Values(got)), want) {
		t.Errorf("contract/models.ts CLI_PRODUCTS names %v, want exactly the built-in profiles %v",
			slices.Sorted(slices.Values(got)), want)
	}
}
