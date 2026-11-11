package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE ATLAS NAMES ONLY THE REVISION ITS SETTINGS CAME FROM, never one the fleet
// activated that this node has not applied yet. Mutation: answer the pointer's
// revision whatever it names, and the second case goes red.
func TestTheAtlasNamesOnlyTheRevisionThisNodeRuns(t *testing.T) {
	t.Parallel()
	applied := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	running := &Company{ActivatedAt: applied}
	for _, tc := range []struct {
		name    string
		target  coord.Activation
		found   bool
		company *Company
		want    string
	}{
		{"the pointer names the activation this node runs",
			coord.Activation{RevisionID: "r7", At: applied}, true, running, "r7"},
		{"the fleet activated another this node has not applied",
			coord.Activation{RevisionID: "r8", At: applied.Add(time.Minute)}, true, running, ""},
		{"no activation is named", coord.Activation{}, false, running, ""},
		{"an epoch no activation named",
			coord.Activation{RevisionID: "r7", At: applied}, true, &Company{}, ""},
		{"no company", coord.Activation{RevisionID: "r7", At: applied}, true, nil, ""},
	} {
		if got := revisionRunning(tc.target, tc.found, tc.company); got != tc.want {
			t.Errorf("%s: revisionRunning = %q, want %q", tc.name, got, tc.want)
		}
	}
}
