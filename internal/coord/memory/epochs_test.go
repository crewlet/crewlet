package memory_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// scriptedCounter mints what a case tells it to, and can hold one resource's
// mint open until the case lets it go.
type scriptedCounter struct {
	mu     sync.Mutex
	next   map[string]int64
	fail   error
	hold   string
	minted chan struct{}
	gate   chan struct{}
}

func (c *scriptedCounter) Next(_ context.Context, resource string) (int64, error) {
	if resource == c.hold {
		close(c.minted)
		<-c.gate
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		err := c.fail
		c.fail = nil
		return 0, err
	}
	if c.next == nil {
		c.next = map[string]int64{}
	}
	c.next[resource]++
	return c.next[resource], nil
}

func acquire(t *testing.T, b *memory.Backend, resource, owner string) (*coord.Lease, coord.Refusal, error) {
	t.Helper()
	return b.TryAcquire(t.Context(), resource, coord.AcquireOptions{Owner: owner, TTL: time.Minute})
}

// A CLAIM THAT CANNOT MINT GIVES THE RESOURCE BACK. Left claiming, the
// resource would be held against every claimant, its own owner's retries
// included, and a node could never take its seat again.
func TestAClaimThatCannotMintGivesTheResourceBack(t *testing.T) {
	t.Parallel()
	counter := &scriptedCounter{fail: errors.New("the epochs bucket has no leader")}
	b := &memory.Backend{Epochs: counter}
	if lease, refused, err := acquire(t, b, "seat:a", "n1"); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("a claim whose token could not be minted = %v, %q, %v; want UNKNOWN", lease, refused, err)
	}
	lease, refused, err := acquire(t, b, "seat:a", "n2")
	if err != nil || lease == nil || lease.Epoch != 1 {
		t.Fatalf("the next claim = %v, %q, %v; want it granted at the counter's first token", lease, refused, err)
	}
}

// A COUNTER THAT GOES BACKWARDS IS REFUSED, as UNKNOWN: a token at or below
// one this store already handed out is held by a past tenure.
func TestATokenThatIsNotAboveTheLastIsRefused(t *testing.T) {
	t.Parallel()
	counter := &scriptedCounter{}
	b := &memory.Backend{Epochs: counter}
	held, _, err := acquire(t, b, "seat:a", "n1")
	if err != nil || held == nil {
		t.Fatalf("TryAcquire = %v, %v", held, err)
	}
	if ok, err := b.Release(t.Context(), "seat:a", "n1", held.Epoch); err != nil || !ok {
		t.Fatalf("Release = %v, %v", ok, err)
	}
	counter.mu.Lock()
	counter.next = nil // the counter was reset under the process
	counter.mu.Unlock()
	if lease, refused, err := acquire(t, b, "seat:a", "n2"); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("a claim minted at a reset counter's token = %v, %q, %v; want UNKNOWN", lease, refused, err)
	}
}

// WHILE A TOKEN IS MINTED the resource is held against a peer, and nothing
// else in the store waits on the mint: it is a round trip to the broker, and
// every renew of every seat on the node shares the store's lock.
func TestAMintHoldsItsResourceAndNothingElse(t *testing.T) {
	t.Parallel()
	counter := &scriptedCounter{hold: "seat:a", minted: make(chan struct{}), gate: make(chan struct{})}
	b := &memory.Backend{Epochs: counter}
	other, _, err := acquire(t, b, "seat:b", "n1")
	if err != nil || other == nil {
		t.Fatalf("TryAcquire(seat:b) = %v, %v", other, err)
	}

	type result struct {
		lease *coord.Lease
		err   error
	}
	done := make(chan result, 1)
	go func() {
		lease, _, err := acquire(t, b, "seat:a", "n1")
		done <- result{lease, err}
	}()
	<-counter.minted

	if lease, refused, err := acquire(t, b, "seat:a", "n2"); err != nil || lease != nil || refused != coord.RefusedHeld {
		t.Errorf("a peer's claim during the mint = %v, %q, %v; want RefusedHeld", lease, refused, err)
	}
	if lease, refused, err := acquire(t, b, "seat:a", "n1"); !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("the owner's second claim during the mint = %v, %q, %v; want UNKNOWN", lease, refused, err)
	}
	if ok, err := b.Renew(t.Context(), "seat:b", "n1", other.Epoch, time.Minute); err != nil || !ok {
		t.Errorf("a renew of another seat during the mint = %v, %v; want it renewed", ok, err)
	}
	if lease, err := b.Get(t.Context(), "seat:a"); err != nil || lease != nil {
		t.Errorf("Get during the mint = %v, %v; want unheld until the token is committed", lease, err)
	}

	close(counter.gate)
	got := <-done
	if got.err != nil || got.lease == nil || got.lease.Owner != "n1" || got.lease.Epoch != 1 {
		t.Fatalf("the minting claim = %v, %v; want it granted to n1 at 1", got.lease, got.err)
	}
}
