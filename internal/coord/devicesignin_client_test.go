package coord_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/coord"
)

// THE MODELS SCREEN NAMES EVERY DEVICE SIGN-IN STATUS, AND KNOWS WHICH END A
// SESSION. Settings › Models' device sign-in dialog keys its status line and
// its tone on the status `llm_device_sign_in` answers, and stops polling and
// offers to start again on the ones that end a session. A status the engine
// adds that the dashboard lacks is a session drawn with no words; one it
// treats as still moving after the engine ended it is a dialog polling for
// ever, and the reverse a live code abandoned.
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
