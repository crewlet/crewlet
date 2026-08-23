package coordtest

import (
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// churnTTL is short enough that leases genuinely lapse mid-churn, so the
// stress case exercises takeover and the epoch bumps that come with it rather
// than just hammering one uncontested hold.
const churnTTL = 2 * time.Millisecond

// contendedClaimBudget is how long a claimant keeps coming back for a
// DEFINITE answer while the suite is deliberately stampeding one resource.
//
// A backend may answer unknown at any moment — that is the third answer, not
// an error the suite gets to disallow — and a compare-and-swap store under a
// stampede reaches it honestly: it loses every swap it attempts inside its
// retry budget and cannot say whether the winner is a peer or a record that
// lapsed underneath it. (An embedded-NATS backend does exactly this, and the
// first version of these cases failed it for being right.) So the suite does
// what a caller does — comes back on the next sweep — rather than requiring a
// definite answer from a contended store on the first try. Ten seconds is
// orders of magnitude beyond the microseconds this takes in-process and the
// few round trips it takes out of it, and it stays inside stallBudget.
const contendedClaimBudget = 10 * time.Second

// claimUntilDefinite retries an unknown answer until the backend gives a real
// one: a lease, or a definite refusal. It reports the last error if the budget
// runs out with the store still unable to answer.
func claimUntilDefinite(h *harness, resource string, opts coord.AcquireOptions) (*coord.Lease, error) {
	deadline := time.Now().Add(contendedClaimBudget)
	var last error
	for attempt := 0; ; attempt++ {
		lease, err := h.b.TryAcquire(h.ctx, resource, opts)
		if err == nil {
			return lease, nil
		}
		last = err
		if h.ctx.Err() != nil || !time.Now().Before(deadline) {
			return nil, fmt.Errorf("no definite answer in %v (%d attempts): %w",
				contendedClaimBudget, attempt+1, last)
		}
		// A backoff, because the point of coming back is to arrive when
		// the contention that caused the unknown has moved on.
		time.Sleep(time.Millisecond)
	}
}

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
		// this: one winner, at one epoch, and every loser eventually told
		// so definitively — see claimUntilDefinite for why "eventually"
		// is the honest word.
		const claimants = 32
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
				lease, err := claimUntilDefinite(h, "seat:ceo", coord.AcquireOptions{
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
		h.await(&wg, "32 claimants racing for one resource")

		for _, err := range failures {
			h.t.Errorf("a claimant never got a definite answer: %v", err)
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
				lease, err := claimUntilDefinite(h, "seat:ceo", coord.AcquireOptions{
					Owner: "node-a:1", TTL: LongTTL,
				})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					failures = append(failures, err)
				case lease == nil:
					failures = append(failures, fmt.Errorf("an owner was definitively refused its own live lease"))
				default:
					epochs[lease.Epoch]++
				}
			}()
		}
		close(start)
		h.await(&wg, "16 concurrent claims by one owner")

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
		ctx := h.ctx
		var mu sync.Mutex
		holder := map[int64]string{}
		var maxEpoch int64
		var unknown int
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
		// An unknown answer is not a failure here, and a suite that
		// treated it as one would certify only backends that serialise
		// every call behind a single lock. A heartbeat that cannot reach
		// the store keeps what it holds and comes back next tick; these
		// workers do the same, and the safety invariants below hold
		// however many calls went unanswered.
		unanswered := func() {
			mu.Lock()
			defer mu.Unlock()
			unknown++
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
							unanswered()
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
							unanswered()
						}
					default:
						if held == nil {
							continue
						}
						if _, err := h.b.Release(ctx, "seat:ceo", owner, held.Epoch); err != nil {
							unanswered()
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
						unanswered()
					}
					if _, err := h.b.ListLive(ctx, coord.SeatPrefix); err != nil {
						unanswered()
					}
					if _, err := h.b.ListOwned(ctx, "node-0:1"); err != nil {
						unanswered()
					}
					if _, err := h.b.PreferredResources(ctx, coord.SeatPrefix, "node-0:1"); err != nil {
						unanswered()
					}
					if _, _, err := h.b.FleetProtocolFloor(ctx); err != nil {
						unanswered()
					}
				}
			}()
		}
		close(start)
		h.await(&wg, "acquire/renew/release churn with concurrent readers")

		for _, err := range failures {
			h.t.Errorf("%v", err)
		}
		if h.t.Failed() {
			return
		}
		// Tolerating unknowns must not let a store that answers nothing
		// pass by doing nothing: the churn has to have actually minted
		// tenures for the epoch invariants above to have been tested.
		if maxEpoch == 0 {
			h.t.Fatalf("no claim in the whole churn was answered (%d unknown) — nothing "+
				"was certified here", unknown)
		}
		if unknown > 0 {
			h.t.Logf("%d of the churn's calls answered unknown; safety held across them", unknown)
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
