package node_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// pausedHold is the hold a person's pause takes, spelled as the engine spells
// it (inbox.HoldSeatPaused). The node knows no hold by name, which is the
// point: it takes whatever it is told to before it attaches.
const pausedHold = "seat_paused"

// A SEAT A PERSON PAUSED STAYS PAUSED WHEN PLACEMENT MOVES IT.
//
// The node that held the paused seat held its mail under a pause hold, and the
// RELEASE drops that hold with the attachment — so the node that acquires the
// seat next starts with none, and the mail that waited through the pause would
// be the first thing its consumer ran. The hold has to be on the subscription
// before the attach, which is what node.Config.AttachHolds is for; a hold
// taken after the attach is taken after the first delivery can arrive.
//
// The resume is then the only thing that delivers the mail: lifting the hold
// on the node that now holds the seat hands it on, in order.
func TestPlacementMovingAPausedSeatKeepsItPaused(t *testing.T) {
	for _, sub := range substrates() {
		t.Run(sub.name, func(t *testing.T) {
			f := newFleet(t, sub, "ceo")
			var mu sync.Mutex
			paused := true
			f.attachHolds = func(string) []string {
				mu.Lock()
				defer mu.Unlock()
				if paused {
					return []string{pausedHold}
				}
				return nil
			}
			f.ensureMailboxes()

			a := f.start("node-a")
			eventually(t, "node-a to hold the seat", func() bool {
				return len(a.Attached()) == 1
			})
			f.send("ceo", "w1")
			time.Sleep(200 * time.Millisecond)
			if got := f.workSeen("ceo"); len(got) != 0 {
				t.Fatalf("a paused seat ran %v on the node that first held it", got)
			}

			// Placement moves the seat: node-a leaves, node-b arrives.
			a.Drain(context.WithoutCancel(t.Context()))
			eventually(t, "node-a to let the seat go", func() bool {
				return len(a.Attached()) == 0
			})
			f.send("ceo", "w2")
			b := f.start("node-b")
			eventually(t, "node-b to take the seat", func() bool {
				return len(b.Attached()) == 1
			})
			time.Sleep(300 * time.Millisecond)
			if got := f.workSeen("ceo"); len(got) != 0 {
				t.Fatalf("the paused seat ran %v on the node placement moved it to: "+
					"the mail was delivered before its hold was taken", got)
			}

			// The resume: the hold is lifted on the node that holds the seat.
			mu.Lock()
			paused = false
			mu.Unlock()
			resume(t, f, "node-b", "ceo")
			eventually(t, "the resume to deliver what waited", func() bool {
				return len(f.workSeen("ceo")) == 2
			})
			if got := f.workSeen("ceo"); got[0] != "w1" || got[1] != "w2" {
				t.Errorf("after the resume the seat saw %v, want [w1 w2] in order", got)
			}
			if ran := f.nodesThatRan("ceo"); len(ran) != 1 || ran[0] != "node-b" {
				t.Errorf("the held mail ran on %v, want only node-b", ran)
			}
		})
	}
}

// resume lifts the pause hold on the queue client of the node holding handle.
//
// Through the node's own client, because a hold lives on the client that took
// it: lifting it on any other connection lifts nothing.
func resume(t *testing.T, f *fleet, nodeID, handle string) {
	t.Helper()
	f.mu.Lock()
	q := f.queues[nodeID]
	f.mu.Unlock()
	if err := q.ResumeTopic(t.Context(), topics.AgentInbox(handle),
		topics.AgentInboxGroup(handle), pausedHold); err != nil {
		t.Fatalf("ResumeTopic: %v", err)
	}
}
