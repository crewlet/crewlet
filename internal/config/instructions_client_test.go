package config_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
)

// THE SETTINGS SCREEN OFFERS EXACTLY THE PRESETS THE ENGINE ADMITS, and counts
// a text against the engine's cap: a preset it lacks is one nobody can pick, a
// preset the engine refuses is a save that always fails.
func TestTheDashboardHoldsInstructionsToTheEnginesRules(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"AnswerLength", strs(config.AnswerLengths)},
		{"AnswerFormat", strs(config.AnswerFormats)},
	} {
		got, err := clientsource.Union(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(tc.want))) {
			t.Errorf("contract/instructions.ts %s is %v, want exactly the engine's %v", tc.name, got, tc.want)
		}
	}
	got, err := clientsource.Scalar(tree, "ASSISTANT_INSTRUCTIONS_MAX_RUNES")
	if err != nil {
		t.Fatal(err)
	}
	if want := strconv.Itoa(config.AssistantInstructionsMaxRunes); got != want {
		t.Errorf("contract/instructions.ts ASSISTANT_INSTRUCTIONS_MAX_RUNES is %s, the engine's %s", got, want)
	}
}
