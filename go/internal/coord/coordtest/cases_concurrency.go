package coordtest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// churnTTL is short enough that leases genuinely lapse mid-churn, so the
// stress case exercises takeover and the epoch bumps that come with it rather
// than just hammering one uncontested hold.
const churnTTL = 2 * time.Millisecond

// concurrencyCases are the ones that matter most under -race.
//
// The Python engine's correctness here rested on there being a single event
// loop: two coroutines could not be inside try_acquire at the same instant, so
// a read-then-write over the store's records was atomic by accident. Every one
// of those accidents is a real race now.
var concurrencyCases = []testCase{
	{"one_winner_under_a_claim_stampede", func(h *harness) {
		// Every node in a fleet sweeps for unclaimed seats on the same
		// tick. The mutual exclusion the whole seat model rests on is
		// this: one winner, one epoch, and every loser told so
		// definitively rather than by an error it would have to retry.
		const claimants = 32
		ctx := context.Background()
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var winners []*coord.Lease
		var failures []error

		for i := range claimants {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				lease, err := h.b.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
					Owner: fmt.Sprintf("node-%02d:1", i),
					TTL:   LongTTL,
				})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					failures = append(failures, fmt.Errorf("claimant %d: %w", i, err))
				case lease != nil:
					winners = append(winners, lease)
				}
			}()
		}
		close(start)
		wg.Wait()

		for _, err := range failures {
			h.t.Errorf("a contended claim answered unknown, not a refusal: %v", err)
		}
		if len(winners) != 1 {
			h.t.Fatalf("%d claimants won the same resource: %v", len(winners), winners)
		}
		if winners[0].Epoch != 1 {
			h.t.Fatalf("the winner holds epoch %d, want 1", winners[0].Epoch)
		}
		h.mustHold("seat:ceo", winners[0].Owner)
	}},

	{"concurrent_claims_by_one_owner_share_one_epoch", func(h *harness) {
		// One owner's own heartbeat, sweep and recovery path can all
		// re-claim at once. Nothing was ever unowned across those calls,
		// so they must all be the same tenure — a store that minted a
		// second epoch here would fence a node's in-flight writes
		// against itself.
		const callers = 16
		ctx := context.Background()
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		epochs := map[int64]int{}
		var failures []error

		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				lease, err := h.b.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
					Owner: "node-a:1", TTL: LongTTL,
				})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					failures = append(failures, err)
				case lease == nil:
					failures = append(failures, fmt.Errorf("an owner was refused its own live lease"))
				default:
					epochs[lease.Epoch]++
				}
			}()
		}
		close(start)
		wg.Wait()

		for _, err := range failures {
			h.t.Errorf("same-owner claim failed: %v", err)
		}
		if len(epochs) != 1 {
			h.t.Fatalf("one owner's concurrent claims minted %d epochs: %v", len(epochs), epochs)
		}
	}},

	{"no_epoch_is_ever_handed_to_two_owners", func(h *harness) {
		// The fencing token's defining property, stated as the thing a
		// zombie could exploit: for the LIFETIME of a resource, an epoch
		// belongs to exactly one tenure. Acquire, renew and release race
		// each other over a TTL short enough that leases really do lapse
		// underneath, which is where a read-then-write store hands the
		// same number to two owners.
		const (
			workers    = 8
			iterations = 150
		)
		ctx := context.Background()
		var mu sync.Mutex
		holder := map[int64]string{}
		var maxEpoch int64
		var failures []error

		note := func(lease *coord.Lease) {
			mu.Lock()
			defer mu.Unlock()
			if prev, ok := holder[lease.Epoch]; ok && prev != lease.Owner {
				failures = append(failures, fmt.Errorf(
					"epoch %d handed to both %q and %q — a write fenced by it cannot say "+
						"which tenure made it", lease.Epoch, prev, lease.Owner))
				return
			}
			holder[lease.Epoch] = lease.Owner
			if lease.Epoch > maxEpoch {
				maxEpoch = lease.Epoch
			}
		}
		fail := func(err error) {
			mu.Lock()
			defer mu.Unlock()
			failures = append(failures, err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				owner := fmt.Sprintf("node-%d:1", i)
				var held *coord.Lease
				<-start
				for n := range iterations {
					switch n % 3 {
					case 0:
						lease, err := h.b.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
							Owner: owner, TTL: churnTTL, Preferred: owner,
						})
						if err != nil {
							fail(fmt.Errorf("TryAcquire: %w", err))
							continue
						}
						if lease != nil {
							note(lease)
							held = lease
						}
					case 1:
						if held == nil {
							continue
						}
						if _, err := h.b.Renew(ctx, "seat:ceo", owner, held.Epoch, churnTTL); err != nil {
							fail(fmt.Errorf("Renew: %w", err))
						}
					default:
						if held == nil {
							continue
						}
						if _, err := h.b.Release(ctx, "seat:ceo", owner, held.Epoch); err != nil {
							fail(fmt.Errorf("Release: %w", err))
						}
						held = nil
					}
				}
			}()
		}
		// Readers alongside the writers: the listings and the floor are
		// swept on every heartbeat of every node, so they race the
		// claims in production too.
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for range iterations {
					if _, err := h.b.Get(ctx, "seat:ceo"); err != nil {
						fail(fmt.Errorf("Get: %w", err))
					}
					if _, err := h.b.ListLive(ctx, coord.SeatPrefix); err != nil {
						fail(fmt.Errorf("ListLive: %w", err))
					}
					if _, err := h.b.ListOwned(ctx, "node-0:1"); err != nil {
						fail(fmt.Errorf("ListOwned: %w", err))
					}
					if _, err := h.b.PreferredResources(ctx, coord.SeatPrefix, "node-0:1"); err != nil {
						fail(fmt.Errorf("PreferredResources: %w", err))
					}
					if _, _, err := h.b.FleetProtocolFloor(ctx); err != nil {
						fail(fmt.Errorf("FleetProtocolFloor: %w", err))
					}
				}
			}()
		}
		close(start)
		wg.Wait()

		for _, err := range failures {
			h.t.Errorf("%v", err)
		}
		if h.t.Failed() {
			return
		}

		// And the counter never rewound through all of it: the next
		// tenure starts above every token ever issued, whether the
		// previous one ended by release or by lapse.
		h.lapse()
		final := h.claim("seat:ceo", coord.AcquireOptions{Owner: "node-final:1", TTL: LongTTL})
		if final.Epoch <= maxEpoch {
			h.t.Fatalf("epoch %d after churn that reached %d — the counter rewound",
				final.Epoch, maxEpoch)
		}
	}},
}
