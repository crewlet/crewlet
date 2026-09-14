package jetstreamtest

import (
	"fmt"
	"sync"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// MEMBERS CREATING ONE STREAM AT DIFFERENT CEILINGS ALL COME UP.
//
// A fleet booting together has every node create every state log at once, and
// each node sizes the ceiling from its own broker member, so two creates of one
// stream differ by a few bytes. The metadata leader refuses the second as a
// name already in use while the first is still IN FLIGHT, before it has
// committed, and a single read-back in that window finds nothing: the loser
// refused to boot over a stream that existed a moment later. Several streams,
// because the window is a race and one can miss it.
func TestMembersCreatingOneStreamAtDifferentCeilingsAllComeUp(t *testing.T) {
	t.Parallel()
	c := StartCluster(t, 3, js.Config{})
	members := []*js.Queue{c.Client(t, 0), c.Client(t, 1), c.Client(t, 2)}
	for n := range 3 {
		name := fmt.Sprintf("CREWLET_RACE_%d_LOG", n)
		var wg sync.WaitGroup
		errs := make([]error, len(members))
		for i, q := range members {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = q.EnsureDomainStream(t.Context(), js.DomainStream{
					Name:     name,
					Subjects: []string{fmt.Sprintf("crewlet.race.%d.>", n)},
					// A FEW BYTES APART, as two members sizing
					// from two brokers always are.
					MaxBytes:   int64(1<<30 + i*4096),
					Duplicates: 2 * time.Minute,
				})
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("member %d could not provision %s while its peers created "+
					"it: %v", i, name, err)
			}
		}
	}
}
