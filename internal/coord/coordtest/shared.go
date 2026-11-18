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

// RunShared certifies what ONE store answers through SEVERAL HANDLES on it,
// the shape a fleet runs: every node holds its own client of one store.
//
// [Run] hands each case one handle, enough only where all handles are one
// process. On a store REPLICATED across a cluster a handle is connected to
// one member, whose copy can be behind an acknowledged write, so a store
// certified through one handle can still break "a backend must serve its own
// writes, per resource" ([coord.Backend]), which every claim's read-back
// rests on. open returns at least one handle, all on the same EMPTY store
// (a single-process backend repeats one); give a replicated store handles on
// different members, or this certifies the easy case twice.
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
// A RATE, NOT A CERTAINTY, sized from the rate. The failure is a read racing
// a replica that has not applied a write yet: on the KV backend before its
// fix, with two handles on members of a five-member cluster holding no copy
// of the bucket, about one claim in a thousand at sixty-four in flight per
// handle was refused a lease it had won, so two runs in three failed (743 in
// 10,000 at fleet scale). A replica falls behind a burst far more than a
// trickle, so claims go out concurrently, as a placement sweep sends them.
// The KV backend's DETERMINISTIC form is its own cluster case.
const sharedClaims = 500

const sharedConcurrency = 64

// A CLAIM THAT WON IS NEVER ANSWERED AS LOST, and what it won is what the
// claimant reads next.
//
// Each resource is claimed by one handle, once, so nobody else can hold it: a
// claim may answer UNKNOWN (the third answer) but never a refusal, which says
// somebody else holds a lease that is its own. A returned lease is at once the
// claimant's to read (Get, same epoch) and renew. Both are [coord.Backend]'s
// "a backend must serve its own writes": a read-back from a copy that is behind
// reads the write as never made, so the owner believes a peer holds a lease it
// holds until its TTL (a singleton duty dark on every node). Both gate
// settings run, because they read the claimed record by two paths.
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
