package gitlet

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// gatedEvents is a node's event stream that takes no event until its gate
// opens — whatever the publisher's context says, as a stream that stopped
// acknowledging holds a publish — and then keeps each it takes.
type gatedEvents struct {
	gate chan struct{}
	mu   sync.Mutex
	took []*events.Event
}

func (g *gatedEvents) Publish(_ context.Context, _ string, ev *events.Event) error {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.took = append(g.took, ev)
	return nil
}

// taken is every event the stream has taken.
func (g *gatedEvents) taken() []*events.Event {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*events.Event(nil), g.took...)
}

// noRecords is a code log's write authority a committer that lands nothing
// is built over: a publish through it is no case's.
type noRecords struct{}

func (noRecords) Publish(context.Context, statelog.Request) (statelog.Result, error) {
	return statelog.Result{}, errors.New("this case's committer lands no record")
}

// A STOPPING COMMITTER PUBLISHES WHAT IT LANDED BEFORE IT RETURNS: Stop waits
// for the announcer to drain what the repositories handed it, so a node that
// stops loses no announcement of a record it landed while its stream still
// takes them.
//
// In a bubble, where synctest.Wait returns once every goroutine but the
// case's is durably blocked: the announcer is parked on the held publish, so
// a Stop that was going to return without waiting for it has returned by
// then, and one that waits is parked behind it. The opening is handed over as
// a repository's goroutine hands what its write landed (announceLater); the
// committer lands no record here, so it is built over a store it never reads.
//
// Mutation: return from Stop once the repositories' goroutines are done,
// without waiting for the announcer — Stop has returned while the stream
// still holds the opening, red.
func TestAStoppingCommitterPublishesWhatItLanded(t *testing.T) {
	t.Parallel()
	_, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	synctest.Test(t, func(t *testing.T) {
		stream := &gatedEvents{gate: make(chan struct{})}
		// RELEASED HOWEVER THE CASE ENDS, so a failing case leaves no
		// goroutine of the bubble parked on the stream.
		release := sync.OnceFunc(func() { close(stream.gate) })
		t.Cleanup(release)
		c, err := NewCommitter(CommitterOptions{
			Publisher: noRecords{}, DB: db.Reader(), NodeID: "node-a", Limits: DefaultLimits,
			Retention: func() time.Duration { return DefaultDeletedRetention },
			SeatRules: func() (SeatRules, bool) { return SeatRules{}, false },
			Events:    stream, AgentIDs: func(string) (string, bool) { return "", false },
		})
		if err != nil {
			t.Fatalf("NewCommitter: %v", err)
		}
		opened := types.ChangeOpened{RepoID: "web", RepoName: "web", ChangeID: "change-1", Number: 1}
		c.announceLater(announcement{kind: "change_opened", repoID: "web", id: opened.ChangeID,
			publish: func(ctx context.Context, when time.Time) {
				if err := announceOn(ctx, c.events, c.nodeID, when, opened); err != nil {
					t.Errorf("publish the opening: %v", err)
				}
			}})
		stopped := make(chan struct{})
		go func() {
			c.Stop(context.Background())
			close(stopped)
		}()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("Stop returned while the stream still held the opening the committer landed")
		default:
		}

		release()
		<-stopped
		if got := stream.taken(); len(got) != 1 || got[0].Data.(*types.ChangeOpened).ChangeID != opened.ChangeID {
			t.Fatalf("the stream took %d events by the time Stop returned, want the opening the committer landed",
				len(got))
		}
	})
}
