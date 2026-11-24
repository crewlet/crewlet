package coord_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/coord"
)

// THE MODELS SCREEN NAMES EVERY DEVICE SIGN-IN STATUS, AND KNOWS WHICH END A
// SESSION. Settings › Models & Fleets' dialog keys its status line on the status
// `llm_device_sign_in` answers and stops polling on the ones that end a
// session: a status it lacks draws no words, and a disagreement about which
// end a session leaves a dialog polling for ever or abandons a live code.
func TestTheModelsScreenNamesEveryDeviceSignInStatus(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	var all, ended []string
	for _, s := range coord.DeviceSignInStatuses {
		all = append(all, string(s))
		if s.Ended() {
			ended = append(ended, string(s))
		}
	}
	got, err := clientsource.Union(tree, "ModelDeviceStatus")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(all))) {
		t.Errorf("contract/models.ts ModelDeviceStatus is %v, want exactly the engine's %v", got, all)
	}
	body, err := clientsource.Literal(tree, "DEVICE_SIGN_IN_ENDED")
	if err != nil {
		t.Fatal(err)
	}
	if got := clientsource.Strings(body); !slices.Equal(
		slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(ended))) {
		t.Errorf("contract/models.ts DEVICE_SIGN_IN_ENDED is %v, want exactly the statuses the "+
			"engine ends a session in %v", got, ended)
	}
}
