package coordtest

import (
	"sync"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the wake feeds' marks ------------------------------------------- //

var feedMarkCases = []fleetCase{{
	// A MARK ONLY RISES within its generation, whoever writes it: every node
	// consuming a group writes the floor it read, and a write landing after
	// a higher one must not move the mark back to a floor the group passed.
	// A later generation replaces it, whatever its floor.
	name: "a feed mark only rises within its generation",
	fn: func(h *fleetHarness) {
		mark := func(gen uint32, floor uint64) coord.FeedMark {
			return coord.FeedMark{Stream: "CREWLET_CHAT_LOG", Group: "crewlet-chat-feed",
				Generation: gen, Floor: floor}
		}
		if _, found, err := h.f.FeedMark(h.ctx, "CREWLET_CHAT_LOG", "crewlet-chat-feed"); err != nil || found {
			h.t.Fatalf("an unwritten mark reads found %v, %v", found, err)
		}
		steps := []struct {
			m      coord.FeedMark
			raised bool
			want   uint64
		}{
			{mark(0, 40), true, 40},
			{mark(0, 55), true, 55},
			{mark(0, 50), false, 55},
			{mark(0, 55), false, 55},
			{mark(1, 3), true, 3},
			{mark(0, 900), false, 3},
		}
		for i, s := range steps {
			raised, err := h.f.RaiseFeedMark(h.ctx, s.m)
			if err != nil || raised != s.raised {
				h.t.Fatalf("step %d: raise %+v = %v, %v; want %v", i, s.m, raised, err, s.raised)
			}
			got, found, err := h.f.FeedMark(h.ctx, "CREWLET_CHAT_LOG", "crewlet-chat-feed")
			if err != nil || !found || got.Floor != s.want {
				h.t.Fatalf("step %d: the mark reads %+v (found %v, %v), want floor %d",
					i, got, found, err, s.want)
			}
		}
		if _, found, err := h.f.FeedMark(h.ctx, "CREWLET_CHAT_LOG", "crewlet-tracker-feed"); err != nil || found {
			h.t.Fatalf("another group's mark on the same log reads found %v, %v", found, err)
		}
	},
}, {
	// NODES RAISING AT ONCE leave the highest floor, never a lower one that
	// landed last.
	name: "nodes raising a feed mark at once leave the highest",
	fn: func(h *fleetHarness) {
		nodes := h.nodes(4)
		var wg sync.WaitGroup
		for i, node := range nodes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for floor := uint64(1); floor <= 10; floor++ {
					if _, err := node.RaiseFeedMark(h.ctx, coord.FeedMark{Stream: "S", Group: "g",
						Floor: floor*10 + uint64(i)}); err != nil {
						h.t.Errorf("node %d raise %d: %v", i, floor, err)
					}
				}
			}()
		}
		wg.Wait()
		got, found, err := h.f.FeedMark(h.ctx, "S", "g")
		if err != nil || !found || got.Floor != 103 {
			h.t.Fatalf("the mark reads %+v (found %v, %v), want the highest floor 103", got, found, err)
		}
	},
}, {
	// A MARK AT ZERO, OR WITHOUT ITS KEY, IS REFUSED: zero is what an absent
	// mark already says, and a mark naming no stream or group would be read
	// as another log's.
	name: "a feed mark at zero or without its key is refused",
	fn: func(h *fleetHarness) {
		for _, m := range []coord.FeedMark{
			{Stream: "S", Group: "g"},
			{Group: "g", Floor: 4},
			{Stream: "S", Floor: 4},
		} {
			if _, err := h.f.RaiseFeedMark(h.ctx, m); err == nil {
				h.t.Errorf("the mark %+v was accepted", m)
			}
		}
	},
}}
