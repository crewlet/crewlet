package coord_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// stubLister is a store whose listing a case controls, counting every call.
type stubLister struct {
	mu     sync.Mutex
	leases []coord.Lease
	err    error
	calls  atomic.Int64
	listed chan struct{}
}

func newStubLister(leases ...coord.Lease) *stubLister {
	return &stubLister{leases: leases, listed: make(chan struct{}, 64)}
}

func (s *stubLister) ListLive(_ context.Context, class coord.Class) ([]coord.Lease, error) {
	s.calls.Add(1)
	defer func() {
		select {
		case s.listed <- struct{}{}:
		default:
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	if class != coord.ClassNode {
		return nil, errors.New("listed the wrong class")
	}
	return append([]coord.Lease(nil), s.leases...), s.err
}

func (s *stubLister) set(err error, leases ...coord.Lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases, s.err = leases, err
}

// awaitListing waits for the lister's next call to RETURN.
//
// That is not the moment the view has the answer: the view records a listing
// after the call returns, under its own lock, so a case that reads the view
// straight after this can see the answer from before it. A case asserting what
// a listing changed waits for the change itself ([awaitAnswer]).
func (s *stubLister) awaitListing(t *testing.T) {
	t.Helper()
	select {
	case <-s.listed:
	case <-time.After(5 * time.Second):
		t.Fatal("the view never listed")
	}
}

// awaitAnswer waits until the view's answer satisfies ok, and fails naming the
// last answer if it never does.
//
// A POLL ON THE ANSWER ITSELF, because that is what a case asserts: the only
// other signal is the lister's call returning, which comes before the view has
// recorded what it returned — and a case that read the view on that signal
// failed whenever the reader won the race (in about one run in thirty under
// the race detector).
func awaitAnswer(t *testing.T, view *coord.LeaseView, what string,
	ok func(leases []coord.Lease, err error) bool) {

	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		leases, _, err := view.Leases()
		if ok(leases, err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the view never answered %s: it answers %d lease(s), %v",
				what, len(leases), err)
		}
		time.Sleep(time.Millisecond)
	}
}

// holding is an [awaitAnswer] condition: n leases and no error.
func holding(n int) func([]coord.Lease, error) bool {
	return func(leases []coord.Lease, err error) bool { return err == nil && len(leases) == n }
}

// fakeClock is a clock a case moves by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func node(id string) coord.Lease {
	return coord.Lease{Resource: coord.ClassNode.Resource(id), Owner: id}
}

// runView starts a view and stops it with the case, asserting it stops.
func runView(t *testing.T, view *coord.LeaseView) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- view.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("a stopped view returned %v, want its context's end", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("a view outlived its context")
		}
	})
}

// A VIEW ANSWERS FROM MEMORY between listings, which is its whole reason to
// exist: a stateless node's every tool call used to list every presence lease
// in the fleet, across its leaf link, for an answer that changes only when a
// node comes or goes.
func TestAViewAnswersFromMemoryBetweenListings(t *testing.T) {
	t.Parallel()
	store := newStubLister(node("a"), node("b"))
	view, err := coord.NewLeaseView(store, coord.ClassNode,
		coord.ViewOptions{Every: time.Hour, Trust: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	runView(t, view)
	awaitAnswer(t, view, "its first listing", holding(2))
	for range 100 {
		leases, _, err := view.Leases()
		if err != nil || len(leases) != 2 {
			t.Fatalf("the view answered %d lease(s), %v", len(leases), err)
		}
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("a hundred reads listed the store %d times, want once", got)
	}
}

// A VIEW THAT HAS NOT LISTED, OR CANNOT, ANSWERS UNKNOWN — never an empty
// roster and never one older than a lease survives.
//
// An empty roster reads as "no data node is live" and a stale one names a node
// that let its lease lapse; both are answers, and the honest one is that the
// store could not be read — which is the answer this package gives everywhere
// else, and which a caller already knows how to act on.
func TestAViewThatCannotListAnswersUnknown(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)}
	store := newStubLister(node("a"))
	view, err := coord.NewLeaseView(store, coord.ClassNode, coord.ViewOptions{
		Every: 30 * time.Second, Trust: 45 * time.Second, Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := view.Leases(); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("a view that has never listed answered %v, want unknown", err)
	}
	if at := view.ListedAt(); !at.IsZero() {
		t.Fatalf("a view that has never listed says it listed at %v", at)
	}

	runView(t, view)
	awaitAnswer(t, view, "its first listing", holding(1))
	listed := clock.Now()

	// THE STORE STOPS ANSWERING. Inside the trust the last listing is
	// still the answer; past it, the answer is unknown and says why.
	store.set(errors.New("the store is unreachable"))
	clock.advance(30 * time.Second)
	view.Invalidate()
	store.awaitListing(t)
	if leases, _, err := view.Leases(); err != nil || len(leases) != 1 {
		t.Fatalf("a failed listing inside the trust replaced the answer: %d, %v",
			len(leases), err)
	}
	clock.advance(16 * time.Second)
	_, _, err = view.Leases()
	if !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("a listing older than a lease survives answered %v, want unknown", err)
	}
	// SAYING WHY once the failed listing is recorded, which is after the
	// store's call returned.
	const why = "the store is unreachable"
	awaitAnswer(t, view, "unknown, naming why", func(_ []coord.Lease, err error) bool {
		return errors.Is(err, coord.ErrUnavailable) && strings.Contains(err.Error(), why)
	})
	// WHEN IT LAST LISTED IS STILL SAID, past the trust, for a reader that
	// reports the view's age rather than acting on its answer.
	if at := view.ListedAt(); !at.Equal(listed) {
		t.Fatalf("a view past its trust says it last listed at %v, want %v", at, listed)
	}

	// AND IT RECOVERS WITH THE STORE.
	store.set(nil, node("a"), node("b"))
	view.Invalidate()
	awaitAnswer(t, view, "the recovered store's two leases", holding(2))
}

// heldLister is a store whose listing of one lease waits to be let go, saying
// when it is asked.
type heldLister struct {
	asked   chan struct{}
	release chan struct{}
}

func (h *heldLister) ListLive(ctx context.Context, _ coord.Class) ([]coord.Lease, error) {
	select {
	case h.asked <- struct{}{}:
	default:
	}
	select {
	case <-h.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []coord.Lease{node("a")}, nil
}

// A LISTING IS DATED WHEN IT WAS ASKED, and trusted for a TTL from then — never
// from when its answer arrived.
//
// A lease in the answer was live at some instant between the two, so it is
// renewed or lapsed a TTL after that instant. Dated on arrival, a listing that
// took half a TTL to answer was trusted half a TTL past anything the store
// said, naming as live a node whose lease could have lapsed — and the estate
// view, which judges this half's staleness by the same instant, read fresher
// than the store it was built from.
func TestAListingIsDatedWhenItWasAsked(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)}
	store := &heldLister{asked: make(chan struct{}, 1), release: make(chan struct{})}
	view, err := coord.NewLeaseView(store, coord.ClassNode, coord.ViewOptions{
		Every: 30 * time.Second, Trust: 45 * time.Second, Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	runView(t, view)
	<-store.asked
	asked := clock.Now()
	clock.advance(30 * time.Second)
	close(store.release)
	awaitAnswer(t, view, "the listing it asked for", holding(1))
	if at := view.ListedAt(); !at.Equal(asked) {
		t.Fatalf("a listing asked at %v and answered 30s later is dated %v", asked, at)
	}
	clock.advance(16 * time.Second)
	if _, _, err := view.Leases(); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("a listing asked 46s ago under a 45s trust answered %v, want unknown", err)
	}
}

// AN INVALIDATION LISTS AGAIN, and a stream of them is one listing per
// [coord.MinViewRefresh].
//
// A request to a node the view named went unanswered: a node that released its
// lease on a clean stop should leave the roster at once rather than at the
// next heartbeat. But a node that never answers would otherwise make every
// request a listing, which is what the view exists to stop.
func TestAnInvalidationListsAgainAndAStreamOfThemIsHeldApart(t *testing.T) {
	t.Parallel()
	store := newStubLister(node("a"), node("gone"))
	view, err := coord.NewLeaseView(store, coord.ClassNode,
		coord.ViewOptions{Every: time.Hour, Trust: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	runView(t, view)
	store.awaitListing(t)

	first := time.Now()
	store.set(nil, node("a"))
	for range 50 {
		view.Invalidate()
	}
	store.awaitListing(t)
	gap := time.Since(first)
	awaitAnswer(t, view, "the invalidated listing's one lease", holding(1))
	// NOT SOONER THAN THE FLOOR after the listing before it — a node that
	// never answers would otherwise make every failed request a listing.
	if gap < coord.MinViewRefresh*9/10 {
		t.Fatalf("the invalidated listing came %v after the last one, inside "+
			"the %v floor", gap, coord.MinViewRefresh)
	}
	// AND FIFTY INVALIDATIONS ARE ONE LISTING.
	time.Sleep(2 * coord.MinViewRefresh)
	if got := store.calls.Load(); got != 2 {
		t.Fatalf("fifty invalidations listed %d times, want one more than the first", got-1)
	}
}

// A VIEW IS REFUSED A TRUST SHORTER THAN ITS CADENCE, which would answer
// unknown between every two listings on a healthy store.
func TestAViewRefusesATrustShorterThanItsCadence(t *testing.T) {
	t.Parallel()
	if _, err := coord.NewLeaseView(newStubLister(), coord.ClassNode,
		coord.ViewOptions{Every: time.Minute, Trust: time.Second}); err == nil {
		t.Fatal("a view that trusts a listing for less than its cadence was built")
	}
	if _, err := coord.NewLeaseView(newStubLister(), "",
		coord.ViewOptions{Every: time.Minute, Trust: time.Minute}); err == nil {
		t.Fatal("a view of no class was built")
	}
}
