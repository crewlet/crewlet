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
// [Host.Start] makes its first placement pass synchronously, and a pass runs
// each claimed seat's acquire hook inline — the hook spawns the seat's MCP
// children, hydrates its memory and recovers an interrupted executor, which
// can outlast a TTL. The seat is in the held set while it does, so that the
// heartbeat renews its lease throughout ([heldSeat] says why). But the start
// ran the heartbeat loop only once that pass was over, so on the one pass
// where every seat is new, nothing renewed anything: a hook that ran past the
// TTL let the lease lapse under a seat this node was halfway through building,
// for a peer to claim; and the watchdog, which reads the heartbeat's beat,
// read the whole pass as the heartbeat wedged — every gap past two seconds
// that 126 started engines showed under load was that first pass, 5.1 s the
// longest — and past the TTL it ends the process.
//
// Here the hook takes two TTLs, on a bubble's clock, and a goroutine reads the
// beat the way the watchdog does while the start runs. The seat must come out
// of the start still leased to this node at the epoch it was claimed at, and
// admitting turns, and the beat must never have gone more than a beat old.
//
// Mutation: start the heartbeat loop after the first pass, as it was, and the
// lease lapses under the hook and the beat goes two TTLs stale.
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
