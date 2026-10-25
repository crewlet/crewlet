package coordtest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// RunShared certifies what ONE store answers through SEVERAL HANDLES on it —
// the shape a fleet actually runs, where every node holds its own client of
// one coordination store.
//
// [Run] hands each case one handle, and on a backend whose handles are all the
// same process that is the whole story. It is not on a store REPLICATED across
// the members of a cluster: a handle is connected to one member, and a read a
// member answers from its own copy can be behind a write the quorum already
// acknowledged. So a store certified through one handle on one member can
// still break "a backend must serve its own writes, per resource"
// ([coord.Backend]) — the promise every claim's own read-back rests on — the
// moment its handles are spread across members.
//
// open returns the handles, every one on the same EMPTY store, and at least
// one of them; a backend with a single process returns the same handle more
// than once. Run it against a replicated store with the handles on different
// members, or it certifies the easy case twice.
func RunShared(t *testing.T, open func(t *testing.T) []coord.Backend) {
	t.Helper()
	for _, c := range sharedCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			handles := open(t)
			if len(handles) == 0 {
				t.Fatal("RunShared: open returned no handle")
			}
			c.fn(t, handles)
		})
	}
}

var sharedCases = []struct {
	name string
	fn   func(t *testing.T, handles []coord.Backend)
}{
	{"a_claim_that_won_is_never_answered_as_lost", testAClaimThatWonIsNeverAnsweredAsLost},
	{"a_watch_hears_a_claim_made_through_any_handle", testAWatchHearsAClaimMadeThroughAnyHandle},
}

// sharedClaims is how many fresh resources each handle claims, and
// sharedConcurrency how many of one handle's are in flight at once.
//
// A RATE, NOT A CERTAINTY, and sized from the rate. The failure this catches
// is a read racing a replica that has not applied a write yet, which a quiet
// replica rarely loses: measured on the KV backend before its fix, with two
// handles on the members of a five-member cluster that hold no copy of the
// bucket, a claim answered a refusal for a lease it had won about once in a
// thousand claims at sixty-four in flight per handle, so two runs in three
// failed — against 743 in 10,000 under the fleet-scale load that found it (a
// hundred nodes claiming at once). A replica falls behind a burst far more
// readily than a trickle, which is why the claims go out concurrently, as a
// node's placement sweep sends them. The KV backend's DETERMINISTIC form of
// the same defect is its own cluster case, which cuts a member off and reads
// through it; this case is what holds every backend to the promise.
const sharedClaims = 500

const sharedConcurrency = 64

// A CLAIM THAT WON IS NEVER ANSWERED AS LOST, and what it won is what the
// claimant reads next.
//
// Every resource here is claimed by exactly one handle, once, so nobody else
// can hold it: a claim may answer UNKNOWN — the contract's third answer — but
// never a refusal, which tells the caller somebody else holds a lease that is
// in fact its own. And a claim that returned a lease is at once the claimant's
// to read (Get answers it at the same epoch) and to renew. Both halves are one
// promise, [coord.Backend]'s "a backend must serve its own writes": a claim's
// own read-back is a read of its write, and one answered by a copy that is
// behind reads the write as never made — and the lease is then held by this
// owner in the store until its TTL while the owner believes a peer has it.
// For a singleton duty that is the duty dark for a TTL on every node at once.
//
// Both gate settings are exercised, because they read the claimed record by
// two different paths.
func testAClaimThatWonIsNeverAnsweredAsLost(t *testing.T, handles []coord.Backend) {
	ctx, cancel := context.WithTimeout(t.Context(), stallBudget)
	defer cancel()
	var (
		won, unknown atomic.Int64
		wg           sync.WaitGroup
		mu           sync.Mutex
		failures     []string
	)
	fail := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(failures) < 10 {
			failures = append(failures, fmt.Sprintf(format, args...))
		}
	}
	for i, b := range handles {
		slots := make(chan struct{}, sharedConcurrency)
		for k := range sharedClaims {
			resource := coord.ClassSeat.Resource(fmt.Sprintf("shared-%d-%d", i, k))
			owner := fmt.Sprintf("handle-%d", i)
			slots <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				lease, refused, err := b.TryAcquire(ctx, resource, coord.AcquireOptions{
					Owner: owner, TTL: LongTTL, Ungated: k%2 == 0,
				})
				switch {
				case err != nil:
					unknown.Add(1)
					return
				case lease == nil:
					fail("%s: the only claim of %s was refused (%q) — nobody else "+
						"claims it, so the claimant was told a peer holds a lease that "+
						"is its own", owner, resource, refused)
					return
				}
				won.Add(1)
				got, err := b.Get(ctx, resource)
				switch {
				case err != nil:
					// Unknown is always permitted; nothing to judge.
				case got == nil || got.Owner != owner || got.Epoch != lease.Epoch:
					fail("%s: Get of %s straight after claiming it at epoch %d "+
						"answered %+v — the claimant cannot read its own write",
						owner, resource, lease.Epoch, got)
				}
				renewed, err := b.Renew(ctx, resource, owner, lease.Epoch, LongTTL)
				if err == nil && !renewed {
					fail("%s: Renew of %s straight after claiming it at epoch %d "+
						"answered definitively not held", owner, resource, lease.Epoch)
				}
			}()
		}
	}
	wg.Wait()
	for _, f := range failures {
		t.Error(f)
	}
	total := int64(len(handles) * sharedClaims)
	// A SUITE THAT SAW ONLY UNKNOWNS PROVED NOTHING: every assertion above
	// is about a claim that answered, and a store that could not answer
	// at all would pass them vacuously.
	if won.Load() < total*9/10 {
		t.Fatalf("only %d of %d uncontended claims won (%d unknown) — too few "+
			"answered for the case to have judged anything", won.Load(), total,
			unknown.Load())
	}
}

// A WATCH HEARS A CLAIM MADE THROUGH ANY HANDLE, which is what it is for: a
// node's view of the fleet hears a PEER join, and a peer writes through its
// own handle on whichever member it is connected to. A watch that heard only
// its own handle's writes would pass every case in [Run] and hear nobody.
//
// Every handle watches; then every handle claims a presence lease of its own,
// one after another; and every watch must hear every claim with its holder
// and epoch. The order ACROSS resources is not judged, so the case does not
// depend on how the claims interleave with the deliveries.
func testAWatchHearsAClaimMadeThroughAnyHandle(t *testing.T, handles []coord.Backend) {
	ctx, cancel := context.WithTimeout(t.Context(), stallBudget)
	defer cancel()
	watches := make([]<-chan coord.LeaseWrite, len(handles))
	for i, b := range handles {
		writes, err := b.WatchLive(ctx, coord.ClassNode)
		if err != nil {
			t.Fatalf("WatchLive through handle %d: %v", i, err)
		}
		watches[i] = writes
	}
	want := map[string]coord.LeaseWrite{}
	for j, b := range handles {
		resource := coord.NodeResource(fmt.Sprintf("watched-%d", j))
		owner := fmt.Sprintf("handle-%d", j)
		lease, refused, err := b.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: owner, TTL: LongTTL, Ungated: true,
		})
		if err != nil || lease == nil {
			t.Fatalf("claim of %s through handle %d: lease %v, refused %q, error %v",
				resource, j, lease, refused, err)
		}
		want[resource] = coord.LeaseWrite{Resource: resource, Owner: owner, Epoch: lease.Epoch}
	}
	for i, writes := range watches {
		heard := map[string]coord.LeaseWrite{}
		deadline := time.After(watchBound)
		for len(heard) < len(want) {
			select {
			case w, open := <-writes:
				if !open {
					t.Fatalf("the watch through handle %d closed having heard %v of %v",
						i, heard, want)
				}
				if _, claimed := want[w.Resource]; claimed {
					heard[w.Resource] = w
				}
			case <-deadline:
				t.Fatalf("the watch through handle %d heard %v of %v within %v",
					i, heard, want, watchBound)
			}
		}
		for resource, w := range want {
			if heard[resource] != w {
				t.Errorf("the watch through handle %d heard %+v, want %+v",
					i, heard[resource], w)
			}
		}
	}
}
