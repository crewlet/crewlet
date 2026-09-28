package coordtest

import (
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// churnTTL is short enough that leases genuinely lapse mid-churn, so the
// stress case exercises takeover and the epoch bumps that come with it rather
// than just hammering one uncontested hold.
//
// Measured, because that is a claim about this SUITE rather than about a
// backend and so is one nobody is motivated to check: instrumenting the churn
// to report its final epoch gives 304 tenures against the twin and 313
// against the embedded broker. Hundreds of real ownership changes, which is
// what makes "no epoch is ever handed to two owners" a statement about
// contention rather than about a single uncontested hold. If a future edit
// lengthens this, re-measure — the case can degrade to proving nothing
// without failing.
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
// one: a lease, or a definite refusal and its reason. It reports the last error
// if the budget runs out with the store still unable to answer.
func claimUntilDefinite(h *harness, resource string, opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {
	deadline := time.Now().Add(contendedClaimBudget)
	var last error
	for attempt := 0; ; attempt++ {
		lease, refused, err := h.b.TryAcquire(h.ctx, resource, opts)
		if err == nil {
			return lease, refused, nil
		}
		last = err
		if h.ctx.Err() != nil || !time.Now().Before(deadline) {
			return nil, "", fmt.Errorf("no definite answer in %v (%d attempts): %w",
				contendedClaimBudget, attempt+1, last)
		}
		// A backoff, because the point of coming back is to arrive when
		// the contention that caused the unknown has moved on.
		time.Sleep(time.Millisecond)
	}
}

// concurrencyCases are the ones that matter most under -race.
//
// Correctness here cannot rest on a single-threaded scheduler: two callers can
// be inside a claim at the same instant, so a read-then-write over the store's
// records is never atomic by accident.
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
			wg.Go(func() {
				<-start
				lease, refused, err := claimUntilDefinite(h, "seat:ceo", coord.AcquireOptions{
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
				case refused != coord.RefusedHeld:
					// Every loser lost to the winner's hold, and a
					// seat host acts on the reason: read as a gate,
					// it would judge one on every sweep.
					failures = append(failures, fmt.Errorf("claimant %d: refused %q, want %q",
						i, refused, coord.RefusedHeld))
				}
			})
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
			wg.Go(func() {
				<-start
				lease, _, err := claimUntilDefinite(h, "seat:ceo", coord.AcquireOptions{
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
			})
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
			wg.Go(func() {
				owner := fmt.Sprintf("node-%d:1", i)
				var held *coord.Lease
				<-start
				for n := range iterations {
					switch n % 3 {
					case 0:
						lease, _, err := h.b.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
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
			})
		}
		// Readers alongside the writers: the listings and the floor are
		// swept on every heartbeat of every node, so they race the
		// claims in production too.
		for range 2 {
			wg.Go(func() {
				<-start
				for range iterations {
					if _, err := h.b.Get(ctx, "seat:ceo"); err != nil {
						unanswered()
					}
					if _, err := h.b.ListLive(ctx, coord.ClassSeat); err != nil {
						unanswered()
					}
					if _, err := h.b.ListOwned(ctx, "node-0:1"); err != nil {
						unanswered()
					}
					if _, err := h.b.PreferredResources(ctx, coord.ClassSeat, "node-0:1"); err != nil {
						unanswered()
					}
					if _, _, err := h.b.FleetProtocolFloor(ctx); err != nil {
						unanswered()
					}
				}
			})
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

	{"a_lease_renewed_throughout_a_listing_is_in_every_listing", func(h *harness) {
		// "A class listing may lag" is about a lease that CHANGED — a
		// peer claimed a second ago is discovered a second late. It is
		// not licence to drop one that was live before the listing
		// began and after it ended, and a renew is the write every live
		// lease takes on every heartbeat. Missing one is a live node
		// that looks gone (placement divides the seats by the rest), an
		// owner whose drain looks finished, and — for an older-protocol
		// peer — a floor that lets the newer build claim beside it.
		//
		// Measured on the embedded KV before its listings were
		// certified: a store that keeps one revision per key REMOVES the
		// revision a listing is about to read when the key is renewed,
		// and 23% of listings lost a lease that way.
		nodes := []string{"n0", "n1", "n2", "n3", "n4"}
		held := map[string]*coord.Lease{}
		for _, n := range nodes {
			held[n] = h.claim(coord.NodeResource(n), coord.AcquireOptions{
				Owner: n + ":1", TTL: LongTTL, Ungated: true, Protocol: 2,
			})
		}
		// The older build's presence, so the floor has a lease that
		// only it can see.
		held["old"] = h.claim(coord.NodeResource("old"), coord.AcquireOptions{
			Owner: "old:1", TTL: LongTTL, Ungated: true, Protocol: 1,
		})
		want := []string{"node:n0", "node:n1", "node:n2", "node:n3", "node:n4", "node:old"}

		c := startChurn([]string{"n0", "n2", "old"}, func(n string) error {
			l := held[n]
			ok, err := h.b.Renew(h.ctx, l.Resource, l.Owner, l.Epoch, LongTTL)
			if err == nil && !ok {
				err = fmt.Errorf("the renew reported the lease lost")
			}
			return err
		})
		raced, wrong := c.readThrough(h.ctx, func() (string, bool) {
			live, err := h.b.ListLive(h.ctx, coord.ClassNode)
			if err != nil {
				return fmt.Sprintf("ListLive: %v", err), true
			}
			if got := slices.Sorted(slices.Values(resources(live))); !slices.Equal(got, want) {
				return fmt.Sprintf("ListLive(node) = %v", got), false
			}
			owned, err := h.b.ListOwned(h.ctx, "old:1")
			if err != nil {
				return fmt.Sprintf("ListOwned: %v", err), true
			}
			if got := resources(owned); !slices.Equal(got, []string{"node:old"}) {
				return fmt.Sprintf("ListOwned(old:1) = %v", got), false
			}
			floor, found, err := h.b.FleetProtocolFloor(h.ctx)
			if err != nil {
				return fmt.Sprintf("FleetProtocolFloor: %v", err), true
			}
			if !found || floor != 1 {
				return fmt.Sprintf("FleetProtocolFloor = (%d, %v), want (1, true)", floor, found), false
			}
			return "", false
		})
		c.verdict(h.t, "the lease reads", raced, wrong)
	}},

	{"a_hint_rewritten_throughout_a_listing_is_in_every_listing", func(h *harness) {
		// The stickiness hint's record is rewritten whenever its resource
		// changes tenure, and a restarted node reads the hints to find
		// the seats it had warm. A listing that dropped a hint whose
		// record was mid-rewrite sends those seats to whichever node
		// sweeps first — the one thing the hint exists to prevent.
		seats := []string{"seat:s0", "seat:s1", "seat:s2", "seat:s3", "seat:s4"}
		// A cell per seat rather than a map of leases: the map is only
		// READ once the rewriters start, and each rewriter writes its own
		// seat's cell and nothing else.
		type tenure struct{ lease *coord.Lease }
		held := map[string]*tenure{}
		for _, s := range seats {
			held[s] = &tenure{h.claim(s, coord.AcquireOptions{
				Owner: "node-a:1", TTL: LongTTL, Preferred: "node-a",
			})}
		}
		// A new tenure per round: released, then claimed again naming the
		// same node, so the hint never changes and its record always does.
		c := startChurn([]string{"seat:s0", "seat:s2"}, func(s string) error {
			cell := held[s]
			if _, err := h.b.Release(h.ctx, s, cell.lease.Owner, cell.lease.Epoch); err != nil {
				return err
			}
			next, refused, err := h.b.TryAcquire(h.ctx, s, coord.AcquireOptions{
				Owner: "node-a:1", TTL: LongTTL, Preferred: "node-a",
			})
			if err == nil && next == nil {
				err = fmt.Errorf("refused (%q) a resource its own owner had just released", refused)
			}
			if err != nil {
				return err
			}
			cell.lease = next
			return nil
		})
		raced, wrong := c.readThrough(h.ctx, func() (string, bool) {
			hints, err := h.b.PreferredResources(h.ctx, coord.ClassSeat, "node-a")
			if err != nil {
				return fmt.Sprintf("PreferredResources: %v", err), true
			}
			if got := slices.Sorted(maps.Keys(hints)); !slices.Equal(got, seats) {
				return fmt.Sprintf("%v", got), false
			}
			return "", false
		})
		c.verdict(h.t, "PreferredResources", raced, wrong)
	}},
}
