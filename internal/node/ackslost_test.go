package node_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// losingQueue is the in-memory twin with the one capability the jetstream
// backend has and the twin does not: a signal that the connection every
// delivery is settled over has been closed for good.
type losingQueue struct {
	queue.EventQueue
	acks chan struct{}
}

func (q losingQueue) AcksLost() <-chan struct{} { return q.acks }

// cutBound is how long a drain cut short may take to return. Well inside the
// drain's own ten-second log interval, so a drain that noticed the loss only
// when that interval came round is caught rather than passed.
const cutBound = 2 * fleetTTL

// A DRAIN WHOSE ACKNOWLEDGEMENTS ARE GONE ENDS AT ONCE AND CANCELS ITS TURNS —
// and a drain whose acknowledgements still land waits for them as it always did.
//
// Once the queue's own broker connection is closed for good, nothing a running
// turn concludes can be acknowledged: the broker hands its delivery to whichever
// node takes the seat, which runs it again. Waiting for the turn bought only a
// second copy of every side effect it made and a later exit. The turns here
// record their cancellation and then go on blocking, so a drain that cancelled
// them and then waited for them to unwind reads as the defect it is.
//
// The third row is the other arm: a node that stopped because a SECOND
// connection closed (the coordination store's, on an embedded broker) still
// acknowledges over the queue's own, so its turns are worth finishing.
//
// Mutation: stop asking the queue for its signal in node.New, drop the cancel
// in Drain, or drop the watcher (the cut then waits out the drain's ten-second
// interval) and the first two rows go red; read a queue that merely HAS the
// signal as one that lost its acks and the third does.
func TestADrainWhoseAcksAreLostEndsAtOnceAndCancelsItsTurns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// lose closes the acks: before the drain, while it waits, or never.
		before, during bool
	}{
		{name: "the acks were lost before the drain began", before: true},
		{name: "the acks were lost while the drain waited", during: true},
		{name: "the acks still land, so the drain waits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			acks := make(chan struct{})
			started := make(chan struct{})
			cancelled := make(chan struct{})
			hold := make(chan struct{})
			var once sync.Once
			g := gatedFleetOn(t, 2, func(q queue.EventQueue) queue.EventQueue {
				return losingQueue{EventQueue: q, acks: acks}
			}, func(ctx context.Context, _ string, _ []*events.Event) queue.Result {
				once.Do(func() { close(started) })
				select {
				case <-ctx.Done():
					close(cancelled)
				case <-hold:
					return queue.Ack()
				}
				// STILL RUNNING after the cancel, as a turn mid-tool-call is
				// until it notices: the drain must not be waiting on this.
				<-hold
				return queue.Ack()
			}, "ceo")
			// Before the harness's own cleanups, which stop the node and the
			// queue a blocked handler is still inside. Once, because the row
			// whose acks land releases the turn itself.
			release := sync.OnceFunc(func() { close(hold) })
			t.Cleanup(release)

			g.send("ceo", "work")
			select {
			case <-started:
			case <-time.After(3 * fleetTTL):
				t.Fatal("the turn never started")
			}
			if tc.before {
				close(acks)
			}

			drained := make(chan struct{})
			go func() {
				g.n.Drain(context.WithoutCancel(t.Context()))
				close(drained)
			}()

			if !tc.before {
				// Long enough for a drain that is not waiting to finish.
				select {
				case <-drained:
					t.Fatal("the drain finished with a turn running and its acks " +
						"still landing: it abandoned work nothing will run again")
				case <-cancelled:
					t.Fatal("a turn was cancelled while its acks still land")
				case <-time.After(200 * time.Millisecond):
				}
			}
			if !tc.before && !tc.during {
				// The acks land: the turn finishes, and so does the drain.
				release()
				select {
				case <-drained:
				case <-time.After(cutBound):
					t.Fatal("the drain never completed once its turn finished")
				}
				select {
				case <-cancelled:
					t.Error("a turn whose ack still lands was cancelled")
				default:
				}
				return
			}
			if tc.during {
				close(acks)
			}

			select {
			case <-drained:
			case <-time.After(cutBound):
				t.Fatalf("the acks are lost and the drain is still waiting %s "+
					"later for a turn nothing can acknowledge", cutBound)
			}
			select {
			case <-cancelled:
			case <-time.After(cutBound):
				t.Fatal("the drain ended and the turn still running was never " +
					"cancelled: it goes on making side effects its seat's " +
					"successor will make again")
			}
		})
	}
}
