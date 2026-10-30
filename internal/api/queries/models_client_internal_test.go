package queries

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE MODELS SCREEN READS THE ENGINE'S HEALTH AND ALARMS. A model's condition
// is decided once, here, and the page keys its words, its dot and its sort on
// the reading it is handed: a reading the dashboard lacks is a model drawn with
// no condition at all, and one the engine dropped is copy for a state that never
// arrives. The same holds for the five alarms `llm_entries` carries, whose
// banners link each one to the model it names.
func TestTheModelsScreenReadsTheEnginesHealthAndAlarms(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"ModelHealth", names(ModelHealths)},
		{"ModelAlarmKind", names(llmAlarmKinds)},
	} {
		got, err := clientsource.Union(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(tc.want))) {
			t.Errorf("contract/models.ts %s is %v, want exactly the engine's %v", tc.name, got, tc.want)
		}
	}
}
