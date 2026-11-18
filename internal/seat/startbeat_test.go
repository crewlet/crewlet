package seat

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
)

// A START WHOSE FIRST PASS OUTLASTS THE LEASE KEEPS ITS SEAT, AND ITS BEAT.
//
// [Host.Start]'s first pass runs acquire hooks inline, which can outlast a
// TTL while the heartbeat renews the held seat ([heldSeat]). Started after
// that pass, the loop lets the lease lapse, and the watchdog takes the pass
// for a wedged heartbeat and ends the process.
//
// The hook takes two TTLs on a bubble's clock while a goroutine reads the beat
// as the watchdog does: the seat stays leased at its epoch, the beat fresh.
// Mutation: start the heartbeat loop after the first pass, and the lease
// lapses under the hook and the beat goes two TTLs stale.
func TestAStartWhoseFirstPassOutlastsTheLeaseKeepsItsSeatAndItsBeat(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const ttl = 15 * time.Second
		store := memory.New()
		hooks := &hookLog{block: func(string) { time.Sleep(2 * ttl) }}
		h, err := New(Config{
			Backend: store, Owner: "node-a:1", NodeID: "node-a",
			Seats: seatsNamed("ceo"), TTL: ttl, Hooks: hooks,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		var (
			mu    sync.Mutex
			worst time.Duration
		)
		started := make(chan struct{})
		watched := make(chan struct{})
		go func() {
			defer close(watched)
			for {
				select {
				case <-started:
					return
				case <-time.After(100 * time.Millisecond):
				}
				if last, live := h.Beat(); live {
					mu.Lock()
					worst = max(worst, time.Since(last))
					mu.Unlock()
				}
			}
		}()
		h.Start(t.Context())
		close(started)
		<-watched
		defer h.Stop(context.Background())

		if got := hooks.acquired(); len(got) != 1 {
			t.Fatalf("the premise: the start's pass ran %d acquire hooks, want the seat's one", len(got))
		}
		lease, err := store.Get(t.Context(), seatResource("ceo"))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if lease == nil || lease.Owner != h.Owner() {
			t.Errorf("after a first pass of %v against a %v lease, the seat's lease is %+v, "+
				"want it still this node's: nothing renewed it while the pass ran", 2*ttl, ttl, lease)
		}
		wantAdmits(t, h, "ceo", true)
		mu.Lock()
		defer mu.Unlock()
		if beat := h.beatInterval(); worst > beat {
			t.Errorf("while the start's pass ran, the heartbeat's beat went %v old against a "+
				"beat of %v and a watchdog threshold of %v: the pass was charged to a loop "+
				"that had not been started", worst, beat, ttl)
		}
	})
}
