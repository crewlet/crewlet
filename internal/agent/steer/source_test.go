package steer_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner/steersourcetest"
	"github.com/crewlet/crewlet/internal/agent/steer"
)

// THE FIRST SEGMENT'S BOX IS A STEER SOURCE LIKE A RUN'S RECORD: the one suite
// every source runs (internal/agent/runner/steersourcetest). A box has no
// record to write a brief's notes on and no store to wait on, so the cases for
// those are the record's alone; what it records expired it reports at its
// close, which is the desk's, so it records none here.
func TestTheBoxIsASteerSource(t *testing.T) {
	t.Parallel()
	steersourcetest.Run(t, func(t *testing.T) steersourcetest.Source {
		box := steer.New()
		t.Cleanup(func() { box.Close() })
		return steersourcetest.Source{
			SteerSource: box,
			File: func(t *testing.T, n steer.Note) {
				t.Helper()
				if got := box.Offer(n); got != steer.StatusAccepted {
					t.Fatalf("the box answered %s to note %s", got, n.ID)
				}
			},
			Expired: func() []string { return nil },
		}
	})
}
