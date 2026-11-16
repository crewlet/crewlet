package seat

import (
	"context"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// passRoundTrip is how long the stand-in store takes to answer a renewal, and
// a presence claim: a link of a hundred milliseconds each way.
const passRoundTrip = 200 * time.Millisecond

// roundTrips is a lease store that, once armed, answers every renewal and
// every presence claim one round trip after it was asked — the round trip is
// the case's to play, on its bubble's clock — and counts how many renewals it
// holds unanswered at once.
type roundTrips struct {
	coord.Backend
	armed     atomic.Bool
	roundTrip func()

	mu       sync.Mutex
	inFlight int
	most     int
}

func (r *roundTrips) Renew(ctx context.Context, resource, owner string, epoch int64,
	ttl time.Duration) (bool, error) {

	if r.armed.Load() {
		r.mu.Lock()
		r.inFlight++
		r.most = max(r.most, r.inFlight)
		r.mu.Unlock()
		r.roundTrip()
		r.mu.Lock()
		r.inFlight--
		r.mu.Unlock()
	}
	return r.Backend.Renew(ctx, resource, owner, epoch, ttl)
}

func (r *roundTrips) TryAcquire(ctx context.Context, resource string,
	opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {

	if r.armed.Load() && coord.IsNodeResource(resource) {
		r.roundTrip()
	}
	return r.Backend.TryAcquire(ctx, resource, opts)
}

func (r *roundTrips) mostInFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.most
}

// holding is a host over store holding n seats, every one claimed before the
// store is armed.
func holding(t *testing.T, store *roundTrips, n int) *Host {
	t.Helper()
	return holdingWith(t, Config{Backend: store}, n)
}

// holdingWith is a host built from cfg — its backend, hooks and TTL, the
// shipped TTL where it names none — holding n seats, every one claimed in one
// sweep on the clock of the bubble it is called in.
func holdingWith(t *testing.T, cfg Config, n int) *Host {
	t.Helper()
	cfg.Owner, cfg.NodeID = "node-a:1", "node-a"
	cfg.Seats, cfg.ClaimLimit = numberedSeats(n), n
	if cfg.TTL == 0 {
		cfg.TTL = SeatLeaseTTL
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.renewNodePresence(t.Context())
	h.Sweep(t.Context())
	if held := len(h.Held()); held != n {
		t.Fatalf("the premise: one sweep took %d of %d seats", held, n)
	}
	return h
}

// renewStamps is when each seat h holds last had a renewal sent, in the order a
// pass renews them (heartbeatTargets: by handle).
func renewStamps(h *Host) []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []time.Time
	for _, handle := range slices.Sorted(maps.Keys(h.held)) {
		out = append(out, h.held[handle].renewedAt)
	}
	return out
}

// A RENEWAL PASS OVER MANY SEATS TAKES ABOUT ONE ROUND TRIP: its renewals go
// to the store together, and the presence's claim beside them, rather than one
// after another.
//
// Forty seats, each renewal and the presence's claim answered a round trip of
// [passRoundTrip] after it was asked, on a bubble's clock, so the pass's length
// is exact arithmetic: one round trip. Renewed one after another, as on main,
// the same pass took forty-one — 8.2 s of a 14 s renew period at the shipped
// TTL, and past the floor TTL's 4 s one, where the design measured a real
// store's two round trips a renewal over a lagging link: 40 × 2 × 0.2 s = 16 s.
// Every seat's renewal is stamped at the instant it was sent, the pass's start.
//
// Mutation: renew the seats one after another (forty-one round trips), or
// claim the presence after them (two).
func TestARenewPassOverManySeatsTakesAboutOneRoundTrip(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := &roundTrips{Backend: memory.New(), roundTrip: func() { time.Sleep(passRoundTrip) }}
		const seats = 40
		h := holding(t, store, seats)

		store.armed.Store(true)
		start := time.Now()
		lost := h.Heartbeat(t.Context())
		took := time.Since(start)

		if len(lost) != 0 || len(h.Held()) != seats {
			t.Fatalf("the pass lost %v and holds %d of %d seats", lost, len(h.Held()), seats)
		}
		if took != passRoundTrip {
			t.Errorf("a pass over %d seats, every renewal and the presence's claim answered "+
				"%v after it was asked, took %v (%.1f round trips), want one round trip",
				seats, passRoundTrip, took, float64(took)/float64(passRoundTrip))
		}
		for i, sent := range renewStamps(h) {
			if !sent.Equal(start) {
				t.Errorf("seat %d's renewal is stamped %v into the pass, want the pass's start, "+
					"when every renewal was sent", i, sent.Sub(start))
				break
			}
		}
	})
}

// A RENEWAL PASS HOLDS AT MOST ITS CONCURRENCY IN FLIGHT, and fills it: a pass
// over more seats than [renewConcurrency] renews them in batches of exactly
// that many, each stamped at its own send.
//
// Two hundred seats, each renewal answered a round trip after it was asked on
// a bubble's clock: the store sees 64 at once and never more, the pass takes
// ⌈200 / 64⌉ = 4 round trips, and a seat of the k-th batch is stamped k round
// trips in, when it was sent — never the pass's start, which would credit it
// with a renewal earlier than it happened.
//
// Mutation: dispatch every renewal at once (200 in flight, one round trip),
// renew one at a time (1 in flight, 200 round trips), or stamp every seat once
// for the pass (each later batch stamped at the start).
func TestARenewPassHoldsAtMostItsConcurrencyInFlight(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := &roundTrips{Backend: memory.New(), roundTrip: func() { time.Sleep(passRoundTrip) }}
		const seats = 200
		h := holding(t, store, seats)

		store.armed.Store(true)
		start := time.Now()
		lost := h.Heartbeat(t.Context())
		took := time.Since(start)

		if len(lost) != 0 || len(h.Held()) != seats {
			t.Fatalf("the pass lost %v and holds %d of %d seats", lost, len(h.Held()), seats)
		}
		if most := store.mostInFlight(); most != renewConcurrency {
			t.Errorf("the store held %d renewals unanswered at once, want exactly %d "+
				"(renewConcurrency)", most, renewConcurrency)
		}
		batches := (seats + renewConcurrency - 1) / renewConcurrency
		if want := time.Duration(batches) * passRoundTrip; took != want {
			t.Errorf("a pass over %d seats, %d at once, took %v, want %d round trips (%v)",
				seats, renewConcurrency, took, batches, want)
		}
		for i, sent := range renewStamps(h) {
			want := start.Add(time.Duration(i/renewConcurrency) * passRoundTrip)
			if !sent.Equal(want) {
				t.Errorf("seat %d (batch %d) is stamped %v into the pass, want %v, when its "+
					"renewal was sent", i, i/renewConcurrency, sent.Sub(start), want.Sub(start))
				break
			}
		}
	})
}
