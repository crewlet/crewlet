package coordtest

import (
	"sync"
	"time"
)

// ---- the rebases -------------------------------------------------------- //

func (h *fleetHarness) rebase(seed string) (time.Time, uint64) {
	h.t.Helper()
	at, version, err := h.f.Rebase(h.ctx, seed)
	if err != nil {
		h.t.Fatalf("Rebase(%s): %v", seed, err)
	}
	return at, version
}

func (h *fleetHarness) recordRebase(seed string, at time.Time, version uint64) bool {
	h.t.Helper()
	ok, err := h.f.RecordRebase(h.ctx, seed, at, version)
	if err != nil {
		h.t.Fatalf("RecordRebase(%s): %v", seed, err)
	}
	return ok
}

var rebaseCases = []fleetCase{{
	// The reason it is the fleet's. An attempt that rebased its work's ids
	// records the instant, and the attempt after it — a crash re-run, a
	// resume retried on the seat's next owner — reads it on another node.
	// On the node's own database that successor read nothing, minted anew,
	// and wrote a second copy of every write the first attempt made.
	name: "a rebase one node recorded is what every node reads",
	fn: func(h *fleetHarness) {
		at := h.now().Add(40 * 24 * time.Hour)
		if !h.recordRebase("wk-1", at, 0) {
			h.t.Fatal("the first record lost")
		}
		got, version := h.rebase("wk-1")
		if !got.Equal(at) {
			h.t.Fatalf("Rebase = %s, want the recorded %s", got, at)
		}
		if version == 0 {
			h.t.Fatal("a recorded rebase reads with no version, so no write can be conditioned on it")
		}
	},
}, {
	// "Nothing recorded" is the answer that sends an attempt to mint at its
	// own instant, so it must be distinguishable from a record — by the
	// version, never by a zero instant a record could hold.
	name: "a seed nothing rebased reads as nothing recorded",
	fn: func(h *fleetHarness) {
		at, version := h.rebase("never-rebased")
		if !at.IsZero() || version != 0 {
			h.t.Fatalf("Rebase of an unrecorded seed = (%s, %d), want (zero, 0)", at, version)
		}
	},
}, {
	// THE FIRST RECORD WINS. Two attempts that both found the start past
	// the horizon each want their own instant; the one that loses must be
	// told so, read the winner's, and inherit it — or the two write under
	// two sets of ids.
	name: "a create over a recorded rebase loses and leaves it",
	fn: func(h *fleetHarness) {
		first := h.now().Add(40 * 24 * time.Hour)
		if !h.recordRebase("wk-1", first, 0) {
			h.t.Fatal("the first record lost")
		}
		if h.recordRebase("wk-1", first.Add(time.Minute), 0) {
			h.t.Fatal("a second create replaced the recorded rebase")
		}
		if got, _ := h.rebase("wk-1"); !got.Equal(first) {
			h.t.Fatalf("Rebase = %s, want the first record's %s", got, first)
		}
	},
}, {
	// A RECORD MOVES ONLY FROM THE VERSION IT WAS READ AT. An attempt past
	// the horizon of the recorded instant replaces it with its own; one
	// that read before another attempt did the same must lose, or it puts
	// back an instant the fleet has already moved past.
	name: "a rebase moves only from the version it was read at",
	fn: func(h *fleetHarness) {
		first := h.now().Add(40 * 24 * time.Hour)
		h.recordRebase("wk-1", first, 0)
		_, read := h.rebase("wk-1")
		second := first.Add(35 * 24 * time.Hour)
		if !h.recordRebase("wk-1", second, read) {
			h.t.Fatal("a write at the version just read lost")
		}
		if h.recordRebase("wk-1", second.Add(time.Hour), read) {
			h.t.Fatal("a write at a version already moved past won")
		}
		got, now := h.rebase("wk-1")
		if !got.Equal(second) {
			h.t.Fatalf("Rebase = %s, want %s", got, second)
		}
		if now == read {
			h.t.Fatal("a moved rebase reads at the version it was moved from")
		}
		if h.recordRebase("never-rebased", second, now) {
			h.t.Fatal("a write at a version of another seed created a record")
		}
	},
}, {
	// The instant is compared with the ids minted at it, to the
	// millisecond, and a store that shifted it — a zone, a rounding — would
	// hand a later attempt an instant no earlier attempt minted at.
	name: "the recorded instant reads back exactly, in UTC",
	fn: func(h *fleetHarness) {
		zone := time.FixedZone("UTC+5", 5*60*60)
		at := time.Date(2026, 10, 11, 17, 30, 1, 234_000_000, zone)
		h.recordRebase("wk-1", at, 0)
		got, _ := h.rebase("wk-1")
		if !got.Equal(at) {
			h.t.Fatalf("Rebase = %s, want %s", got, at)
		}
		if got.Location() != time.UTC {
			h.t.Fatalf("Rebase is in %s, want UTC", got.Location())
		}
	},
}, {
	// A seed is a work key or a run id, and each is a unit of work of its
	// own: one attempt's rebase moving another's would mint that other
	// work's ids somewhere none of its attempts ever minted.
	name: "each seed is its own record, whatever bytes it carries",
	fn: func(h *fleetHarness) {
		at := h.now().Add(40 * 24 * time.Hour)
		seeds := []string{"wk-1", "wk-2", "019a0b1c-0000-7000-8000-000000000001",
			"wk:with.dots and spaces/ü"}
		for i, seed := range seeds {
			if !h.recordRebase(seed, at.Add(time.Duration(i)*time.Hour), 0) {
				h.t.Fatalf("the first record of %q lost", seed)
			}
		}
		for i, seed := range seeds {
			if got, _ := h.rebase(seed); !got.Equal(at.Add(time.Duration(i) * time.Hour)) {
				h.t.Errorf("Rebase(%q) = %s, want its own record", seed, got)
			}
		}
	},
}, {
	// An empty seed is a caller that lost its identity on the way here,
	// and answering it "nothing recorded" would send an attempt to mint
	// under a record no retry can find.
	name: "an empty seed is an error, not nothing recorded",
	fn: func(h *fleetHarness) {
		if _, _, err := h.f.Rebase(h.ctx, ""); err == nil {
			h.t.Error("Rebase accepted an empty seed")
		}
		ok, err := h.f.RecordRebase(h.ctx, "", h.now(), 0)
		if err == nil {
			h.t.Error("RecordRebase accepted an empty seed")
		}
		if ok {
			h.t.Error("RecordRebase recorded a rebase under an empty seed")
		}
	},
}, {
	name: "exactly one of many concurrent first records wins",
	fn: func(h *fleetHarness) {
		at := h.now().Add(40 * 24 * time.Hour)
		var wg sync.WaitGroup
		won := make(chan time.Time, 16)
		for i := range 16 {
			wg.Go(func() {
				mine := at.Add(time.Duration(i) * time.Second)
				ok, err := h.f.RecordRebase(h.ctx, "wk-race", mine, 0)
				if err == nil && ok {
					won <- mine
				}
			})
		}
		wg.Wait()
		close(won)
		var winners []time.Time
		for w := range won {
			winners = append(winners, w)
		}
		if len(winners) != 1 {
			h.t.Fatalf("%d attempts recorded a first rebase, want exactly 1", len(winners))
		}
		if got, _ := h.rebase("wk-race"); !got.Equal(winners[0]) {
			h.t.Fatalf("Rebase = %s, want the winner's %s", got, winners[0])
		}
	},
}}
