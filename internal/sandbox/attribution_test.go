package sandbox

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A LAUNCH CREDITS THE RUN'S COMMITS AS ITS CATALOGUE SAYS, NAMING THE TURN,
// THE SEAT AND ITS WORK ITEM. Under a catalogue that credits commits
// (providers.sandbox.commit_attribution, on unless turned off) the job is
// started asking its runner to credit Crewlet, to name the run by its turn,
// which is what the run's page is addressed by, and to name the seat and the
// item the turn is charged to; under one that turned the credit off the job is
// started asking for nothing, so its git is exactly the box's.
//
// Mutation: hand the runner no attribution, name the run by its launch rather
// than its turn, drop the seat or the item, or ask whatever the catalogue
// says, and this goes red.
func TestALaunchCreditsTheRunsCommitsAsItsCatalogueSays(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		attributes bool
		want       *CommitAttribution
	}{
		"a catalogue crediting commits": {attributes: true,
			want: &CommitAttribution{Run: "t1", Seat: "swe", WorkItem: &attributedItem}},
		"a catalogue that turned it off": {attributes: false, want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRig(t)
			m, err := NewManager(ManagerOptions{
				Providers:        map[Placement]Provider{Direct: rig.provider},
				Runners:          map[string]Runner{"claude-code": rig.runner},
				AttributeCommits: tc.attributes,
			})
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			req := launchReq("t1")
			item := attributedItem
			req.Turn.WorkItem = &item
			if _, err = rig.launchVia(t.Context(), m, req); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			started := rig.runner.Started()
			if len(started) != 1 {
				t.Fatalf("%d jobs started, want 1", len(started))
			}
			if got := started[0].Attribution; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the job was started asking for attribution %+v, want %+v", got, tc.want)
			}
		})
	}
}

// attributedItem is the item the attributed launch's turn is charged to.
var attributedItem = types.WorkItem{Backend: types.WorkWorklet, ID: "task-7", Key: "ENG-7", Project: "ENG"}
