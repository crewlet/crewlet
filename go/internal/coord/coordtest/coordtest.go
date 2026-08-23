// Package coordtest is the contract suite every coord.Backend must pass.
//
// One suite, every backend — the in-memory twin, the embedded KV, any
// external coordination store. A backend the suite has not certified does not
// exist as far as the engine is concerned, and a semantic divergence between
// two backends becomes a failing test here instead of a production-only
// surprise on the one node that happens to run the other one.
//
// The cases are named for the invariant they defend, and those names are the
// documentation: the epoch that must not reset on release, the release that
// must not clear its successor's lease, the renew that must not report loss
// because the store was unreachable for two seconds. The last one is not
// hypothetical — in the Python engine an unreachable store answered exactly
// as a peer holding the resource does, and a blip had a node logging "another
// process holds this node id's presence lease" at an operator while it
// quietly stopped refreshing its own presence.
//
// # What the suite does NOT require
//
// A backend may answer "unknown" at any moment. That is the contract's third
// answer, not a defect the suite gets to disallow, and a compare-and-swap
// store reaches it honestly under contention: it loses every swap inside its
// retry budget and genuinely cannot say whether a peer won or a record lapsed
// underneath it. So the contended cases come back for a definite answer the
// way a caller's next sweep does, instead of demanding one first time. An
// earlier version failed a correct KV backend for exactly this — which is
// what writing the memory twin's implementation strategy into a contract
// looks like from the outside.
//
// # Lapsing without sleeping
//
// A lease lapses when its TTL runs out, and a suite that waited out real TTLs
// would be both slow and flaky. So lapse is expressed as a SHORT TTL plus
// harness.lapse, which asks the backend to move its own clock (see Advancer)
// and falls back to sleeping for a backend whose clock it cannot reach. The
// lease a case intends to lapse gets ShortTTL; everything that must survive
// the lapse gets LongTTL. Nothing here pokes at a backend's records, so the
// same case certifies a store whose expiry is a per-key TTL and one where it
// is a column comparison.
package coordtest

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

const (
	// LongTTL is the TTL for any lease that must stay held for the whole
	// of a case. It is far longer than any lapse the suite simulates, so
	// travelling past a ShortTTL lease never disturbs it.
	//
	// It is also the MAXIMUM TTL the suite will ever ask a backend for, and
	// that is a promise, not an observation. A store sizes its retention to
	// the longest TTL it must honour — the embedded-NATS backend sets its
	// bucket from this very constant — and a backend that cannot honour a
	// TTL is right to refuse it rather than silently shorten it, since a
	// quietly-clamped deadline makes every heartbeat's next-tick arithmetic
	// wrong. A case that needs a second, distinguishable TTL takes a
	// FRACTION of this one; asking for a multiple makes a correct backend
	// fail an unrelated assertion with a confusing error.
	LongTTL = 5 * time.Minute

	// ShortTTL is the TTL for the lease a case intends to lapse.
	//
	// It has to outlast whatever a case does between claiming that lease
	// and calling lapse — a handful of store round trips, plus however
	// long the runtime deschedules a parallel goroutine under -race.
	// 100 ms is three orders of magnitude more than the in-process case
	// needs and comfortably more than a slow round trip, while keeping a
	// fully serialised sleeping run (-parallel 1) to a few seconds. It
	// holds against a real backend: the embedded-NATS store runs the whole
	// suite on the sleeping path, under -race, with no lapse-window
	// failure.
	ShortTTL = 100 * time.Millisecond

	// lapseMargin is how far past ShortTTL harness.lapse travels. On the
	// sleeping path the store's clock is the real one, and a sleep is only
	// ever a lower bound in one direction — this is the other one.
	lapseMargin = 50 * time.Millisecond

	// stallBudget bounds how long the suite will wait on a backend before
	// calling it stuck.
	//
	// A contract call that never returns has to fail as a NAMED CASE, not
	// as a package timeout. The Go test binary's ten-minute panic dumps
	// every goroutine in the process and is attributed to whichever
	// package the deadline happens to land in — which is exactly how a
	// hang in one queue backend was first reported against a different
	// one, costing the wrong author the investigation. The heaviest case
	// here issues a few thousand store round trips, so the budget has to
	// clear that on a contended real store (90 s allows ~30 ms apiece)
	// while still reporting well inside the default package timeout.
	stallBudget = 90 * time.Second
)

// Advancer is the optional hook a backend offers so the suite can outlast a
// TTL without sleeping through it.
//
// Optional, and deliberately so: a real coordination store's clock is its own
// and cannot be moved. Implementing it costs a backend one offset added to
// its now(); not implementing it costs the suite a sleep per lapsing case.
type Advancer interface {
	// Advance moves the STORE's clock forward.
	//
	// Every expiry decision a backend makes must be taken against that
	// clock and never against the caller's — a backend that compared the
	// caller's wall clock would pass this suite unchanged and then hand
	// two nodes the same seat the first time an NTP step separated them.
	Advance(d time.Duration)
}

// Run executes the contract suite against backends built by newBackend.
//
// newBackend is called once per case and must hand back an EMPTY store. The
// protocol gate is fleet-wide, so a leftover lease from one case silently
// refuses the next case's claims — a failure that reads as a broken gate
// rather than as a dirty fixture. A backend sharing one store across calls
// has to clear it; the suite checks and says so rather than letting it
// present as an invariant violation.
func Run(t *testing.T, newBackend func(t *testing.T) coord.Backend) {
	t.Helper()
	groups := []struct {
		name  string
		cases []testCase
	}{
		{"lease", leaseCases},
		{"read", readCases},
		{"protocol", protocolCases},
		{"tristate", tristateCases},
		{"concurrency", concurrencyCases},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			for _, c := range g.cases {
				t.Run(c.name, func(t *testing.T) {
					// Every case owns its own store, so they are
					// independent by construction — and on a backend
					// whose clock the suite cannot move, running them
					// in parallel overlaps the sleeps that lapsing
					// costs instead of summing them.
					t.Parallel()
					c.fn(newHarness(t, newBackend))
				})
			}
		})
	}
}

// testCase is one named invariant.
type testCase struct {
	name string
	fn   func(h *harness)
}

// harness is a backend plus the assertions the cases are written in. Every
// helper that cannot fail in a correct backend fails the test itself, so a
// case body reads as the invariant rather than as error plumbing.
type harness struct {
	t   *testing.T
	ctx context.Context
	b   coord.Backend
}

func newHarness(t *testing.T, newBackend func(t *testing.T) coord.Backend) *harness {
	t.Helper()
	b := newBackend(t)
	if b == nil {
		t.Fatal("newBackend returned a nil backend")
	}
	// Every call the case makes carries the stall budget as a deadline, so
	// a backend that honours cancellation reports a hang as an error on
	// the call that hung instead of as a process-wide timeout. Not the
	// test's own context: this one has to stay live for the goroutines the
	// concurrency cases join at the end, and it is cancelled after them.
	ctx, cancel := context.WithTimeout(context.Background(), stallBudget)
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, b: b}
	if live := h.listLive(""); len(live) != 0 {
		t.Fatalf("newBackend must return an empty store, got %d live lease(s): %v",
			len(live), resources(live))
	}
	return h
}

// claim acquires and fails the case unless the backend granted the lease.
func (h *harness) claim(resource string, opts coord.AcquireOptions) *coord.Lease {
	h.t.Helper()
	lease, err := h.b.TryAcquire(h.ctx, resource, opts)
	if err != nil {
		h.t.Fatalf("TryAcquire(%q, owner=%q): unexpected error: %v", resource, opts.Owner, err)
	}
	if lease == nil {
		h.t.Fatalf("TryAcquire(%q, owner=%q): refused, expected the claim to be granted",
			resource, opts.Owner)
	}
	if lease.Resource != resource || lease.Owner != opts.Owner {
		h.t.Fatalf("TryAcquire(%q, owner=%q) returned a lease for (%q, %q)",
			resource, opts.Owner, lease.Resource, lease.Owner)
	}
	if lease.Epoch < 1 {
		h.t.Fatalf("TryAcquire(%q): epoch %d — a fencing token starts at 1, and 0 is what an "+
			"unset column reads as", resource, lease.Epoch)
	}
	return lease
}

// refused asserts the DEFINITE refusal — (nil, nil). An error here means the
// backend collapsed "unknown" into "somebody else holds it", which is the
// conflation the whole tri-state exists to prevent.
func (h *harness) refused(resource string, opts coord.AcquireOptions) {
	h.t.Helper()
	lease, err := h.b.TryAcquire(h.ctx, resource, opts)
	if err != nil {
		h.t.Fatalf("TryAcquire(%q, owner=%q): a genuine refusal must be (nil, nil), got error: %v",
			resource, opts.Owner, err)
	}
	if lease != nil {
		h.t.Fatalf("TryAcquire(%q, owner=%q): granted at epoch %d, expected a refusal",
			resource, opts.Owner, lease.Epoch)
	}
}

func (h *harness) renew(resource, owner string, epoch int64, ttl time.Duration) bool {
	h.t.Helper()
	ok, err := h.b.Renew(h.ctx, resource, owner, epoch, ttl)
	if err != nil {
		h.t.Fatalf("Renew(%q, owner=%q, epoch=%d): unexpected error: %v", resource, owner, epoch, err)
	}
	return ok
}

func (h *harness) release(resource, owner string, epoch int64) bool {
	h.t.Helper()
	ok, err := h.b.Release(h.ctx, resource, owner, epoch)
	if err != nil {
		h.t.Fatalf("Release(%q, owner=%q, epoch=%d): unexpected error: %v", resource, owner, epoch, err)
	}
	return ok
}

func (h *harness) get(resource string) *coord.Lease {
	h.t.Helper()
	lease, err := h.b.Get(h.ctx, resource)
	if err != nil {
		h.t.Fatalf("Get(%q): unexpected error: %v", resource, err)
	}
	return lease
}

func (h *harness) listOwned(owner string) []coord.Lease {
	h.t.Helper()
	leases, err := h.b.ListOwned(h.ctx, owner)
	if err != nil {
		h.t.Fatalf("ListOwned(%q): unexpected error: %v", owner, err)
	}
	return leases
}

func (h *harness) listLive(prefix string) []coord.Lease {
	h.t.Helper()
	leases, err := h.b.ListLive(h.ctx, prefix)
	if err != nil {
		h.t.Fatalf("ListLive(%q): unexpected error: %v", prefix, err)
	}
	return leases
}

func (h *harness) preferred(prefix, nodeID string) map[string]struct{} {
	h.t.Helper()
	got, err := h.b.PreferredResources(h.ctx, prefix, nodeID)
	if err != nil {
		h.t.Fatalf("PreferredResources(%q, %q): unexpected error: %v", prefix, nodeID, err)
	}
	return got
}

func (h *harness) floor() (int, bool) {
	h.t.Helper()
	got, any, err := h.b.FleetProtocolFloor(h.ctx)
	if err != nil {
		h.t.Fatalf("FleetProtocolFloor: unexpected error: %v", err)
	}
	return got, any
}

// await joins the suite's own goroutines within the stall budget. what names
// what they were doing, so a stuck backend fails on a line that says which
// call never came back rather than on a goroutine dump.
func (h *harness) await(wg *sync.WaitGroup, what string) {
	h.t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stallBudget):
		h.t.Fatalf("%s: still inside the backend after %v — a coord.Backend call that "+
			"never returns takes the whole test binary down with it", what, stallBudget)
	}
}

// lapse outlasts a ShortTTL lease. See the package doc: the suite never edits
// a backend's records, it lets a TTL run out.
func (h *harness) lapse() {
	h.t.Helper()
	d := ShortTTL + lapseMargin
	if a, ok := h.b.(Advancer); ok {
		a.Advance(d)
		return
	}
	time.Sleep(d)
}

// --- assertion helpers ----------------------------------------------------

func (h *harness) mustHold(resource, owner string) *coord.Lease {
	h.t.Helper()
	lease := h.get(resource)
	if lease == nil {
		h.t.Fatalf("Get(%q): no live lease, expected %q to hold it", resource, owner)
	}
	if lease.Owner != owner {
		h.t.Fatalf("Get(%q): held by %q, expected %q", resource, lease.Owner, owner)
	}
	return lease
}

func (h *harness) mustBeUnheld(resource string) {
	h.t.Helper()
	if lease := h.get(resource); lease != nil {
		h.t.Fatalf("Get(%q): held by %q at epoch %d, expected nothing to hold it",
			resource, lease.Owner, lease.Epoch)
	}
}

// requireUnchanged asserts a record survived an operation the backend refused
// or rejected exactly as it was.
//
// A "no" has two halves and the suite used to check only one. Returning the
// right answer while writing to the record anyway is the shape a
// read-modify-write backend reaches by validating after the write instead of
// before it, and every field here is one a refusal must not touch.
//
// This generalises over the record only as far as it is kept in step with it:
// a field added to coord.Lease and not added here is a field every negative
// path may quietly write, and nothing fails to say so. Extend it with the
// struct.
func (h *harness) requireUnchanged(what string, before, after *coord.Lease) {
	h.t.Helper()
	if after == nil {
		h.t.Fatalf("%s: the record is gone", what)
		return
	}
	switch {
	case after.Owner != before.Owner:
		h.t.Fatalf("%s: owner moved %q -> %q", what, before.Owner, after.Owner)
	case after.Epoch != before.Epoch:
		h.t.Fatalf("%s: epoch moved %d -> %d", what, before.Epoch, after.Epoch)
	case !after.ExpiresAt.Equal(before.ExpiresAt):
		h.t.Fatalf("%s: deadline moved %v -> %v — a refusal must not write the record "+
			"at all, and the direction it moves is whichever way the refused caller's "+
			"TTL happened to point",
			what, before.ExpiresAt, after.ExpiresAt)
	case after.Preferred != before.Preferred:
		h.t.Fatalf("%s: placement hint moved %q -> %q", what, before.Preferred, after.Preferred)
	case after.Protocol != before.Protocol:
		h.t.Fatalf("%s: protocol moved %d -> %d", what, before.Protocol, after.Protocol)
	case !reflect.DeepEqual(after.Meta, before.Meta):
		h.t.Fatalf("%s: meta moved %v -> %v", what, before.Meta, after.Meta)
	}
}

// requireResources compares a listing against an expected set. Order is
// deliberately NOT part of the contract: coord.Backend does not promise one,
// and a suite that demanded it would fail a correct store for a detail no
// caller reads.
func (h *harness) requireResources(what string, got []coord.Lease, want ...string) {
	h.t.Helper()
	gotNames := resources(got)
	slices.Sort(gotNames)
	slices.Sort(want)
	if !slices.Equal(gotNames, want) {
		h.t.Fatalf("%s = %v, want %v", what, gotNames, want)
	}
}

func (h *harness) requireSet(what string, got map[string]struct{}, want ...string) {
	h.t.Helper()
	gotNames := make([]string, 0, len(got))
	for r := range got {
		gotNames = append(gotNames, r)
	}
	slices.Sort(gotNames)
	slices.Sort(want)
	if !slices.Equal(gotNames, want) {
		h.t.Fatalf("%s = %v, want %v", what, gotNames, want)
	}
}

func resources(leases []coord.Lease) []string {
	out := make([]string, 0, len(leases))
	for _, l := range leases {
		out = append(out, l.Resource)
	}
	return out
}
