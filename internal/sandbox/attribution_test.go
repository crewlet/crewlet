package sandbox

import (
	"reflect"
	"testing"
)

// A LAUNCH CREDITS THE RUN'S COMMITS AS ITS CATALOGUE SAYS, NAMING THE TURN.
// Under a catalogue that credits commits (providers.sandbox.commit_attribution,
// on unless turned off) the job is started asking its runner to credit Crewlet
// and to name the run by its turn, which is what the run's page is addressed
// by; under one that turned the credit off the job is started asking for
// nothing, so its git is exactly the box's.
//
// Mutation: hand the runner no attribution, name the run by its launch rather
// than its turn, or ask whatever the catalogue says, and this goes red.
func TestALaunchCreditsTheRunsCommitsAsItsCatalogueSays(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		attributes bool
		want       *CommitAttribution
	}{
		"a catalogue crediting commits":  {attributes: true, want: &CommitAttribution{Run: "t1"}},
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
			if _, err = rig.launchVia(t.Context(), m, launchReq("t1")); err != nil {
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
