package coordtest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// MintHold is called by a backend as a claim that has won its record is about
// to mint its fencing epoch: the one point where a claim is neither granted
// nor refused. A backend under [RunMinting] calls it there with the claim's
// resource, or with the counter it mints on ([coord.CounterResource]) where
// that is all it knows (the twin's counter); every running store leaves it
// unset.
type MintHold func(ctx context.Context, resource string)

// RunMinting runs the cases about a claim HELD MID-MINT over the backends open
// builds, each with hold wired into its mint step: what a second claim of the
// same resource is answered while the first is still minting, a browser
// control's take among them.
//
// ITS OWN ENTRY POINT, beside [Run], because no backend can be held mid-claim
// from outside: each wires the hold into its own mint step, the memory twin
// through its epoch counter ([MintCounter]) and the KV store through a hook
// only its own tests set.
func RunMinting(t *testing.T, open func(t *testing.T, hold MintHold) BrowserControlStore) {
	t.Helper()
	for _, c := range mintingCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			gate := &mintGate{}
			b := open(t, gate.hold)
			if b == nil {
				t.Fatal("open returned a nil backend")
			}
			c.fn(t, b, gate)
		})
	}
}

// MintCounter is an epoch counter for the memory twin that holds each mint
// at hold first: what wires the twin into [RunMinting].
type MintCounter struct {
	hold   MintHold
	mu     sync.Mutex
	minted map[string]int64
}

// NewMintCounter is a counter calling hold before each mint.
func NewMintCounter(hold MintHold) *MintCounter {
	return &MintCounter{hold: hold, minted: map[string]int64{}}
}

// Next mints resource's next epoch, once hold lets it.
func (c *MintCounter) Next(ctx context.Context, resource string) (int64, error) {
	c.hold(ctx, resource)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minted[resource]++
	return c.minted[resource], nil
}

// mintGate holds the next mint of one resource until a case releases it.
type mintGate struct {
	mu       sync.Mutex
	resource string
	minting  chan struct{}
	release  chan struct{}
}

// arm holds the next mint of resource: minting closes as it is reached, and
// release lets it go.
func (g *mintGate) arm(resource string) (minting <-chan struct{}, release func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resource = resource
	g.minting, g.release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	r := g.release
	return g.minting, func() { once.Do(func() { close(r) }) }
}

// hold holds the mint the gate is armed for, matched by the counter it mints
// on, since the twin's counter is told no more than that ([MintHold]).
func (g *mintGate) hold(ctx context.Context, resource string) {
	g.mu.Lock()
	if coord.CounterResource(g.resource) != coord.CounterResource(resource) || g.release == nil {
		g.mu.Unlock()
		return
	}
	minting, release := g.minting, g.release
	g.resource, g.release = "", nil
	g.mu.Unlock()
	close(minting)
	select {
	case <-release:
	case <-ctx.Done():
	}
}

var mintingCases = []struct {
	name string
	fn   func(t *testing.T, b BrowserControlStore, gate *mintGate)
}{{
	// A CLAIM WHILE THE SAME OWNER'S CLAIM IS STILL MINTING is neither a
	// grant nor a refusal: the unknown arm, named. errors.Is answers it as
	// [coord.ErrClaimInFlight] for a caller that knows the sibling is its
	// own and that it ends, and as [coord.ErrUnavailable] for every caller
	// that branches on the unknown, so none of them changes. A PEER is
	// refused outright, since the record is held against it; and once the
	// mint is released the first claim holds, and the owner's next claim
	// renews it.
	name: "a claim while the same owner's claim is minting is ErrClaimInFlight",
	fn: func(t *testing.T, b BrowserControlStore, gate *mintGate) {
		ctx := t.Context()
		resource := coord.WorkerResource("conformance-minting")
		mine := coord.AcquireOptions{Owner: "node-a:boot-1", TTL: LongTTL, Ungated: true}
		minting, release := gate.arm(resource)
		defer release()
		type answer struct {
			lease *coord.Lease
			err   error
		}
		first := make(chan answer, 1)
		go func() {
			lease, _, err := b.TryAcquire(ctx, resource, mine)
			first <- answer{lease, err}
		}()
		select {
		case <-minting:
		case got := <-first:
			t.Fatalf("the first claim answered (%+v, %v) without minting an epoch", got.lease, got.err)
		case <-time.After(30 * time.Second):
			t.Fatal("the first claim never reached its mint")
		}

		lease, refusal, err := b.TryAcquire(ctx, resource, mine)
		if lease != nil || refusal != "" || !errors.Is(err, coord.ErrClaimInFlight) ||
			!errors.Is(err, coord.ErrUnavailable) {
			t.Errorf("the same owner's claim mid-mint = (%+v, %q, %v), want ErrClaimInFlight, "+
				"which is an ErrUnavailable", lease, refusal, err)
		}
		peer := coord.AcquireOptions{Owner: "node-b:boot-1", TTL: LongTTL, Ungated: true}
		if lease, refusal, err := b.TryAcquire(ctx, resource, peer); lease != nil ||
			refusal != coord.RefusedHeld || err != nil {
			t.Errorf("a peer's claim mid-mint = (%+v, %q, %v), want it refused held", lease, refusal, err)
		}

		release()
		got := <-first
		if got.err != nil || got.lease == nil || got.lease.Owner != mine.Owner {
			t.Fatalf("the first claim, released = (%+v, %v), want it held", got.lease, got.err)
		}
		if lease, _, err := b.TryAcquire(ctx, resource, mine); err != nil || lease == nil ||
			lease.Epoch != got.lease.Epoch {
			t.Errorf("the owner's next claim = (%+v, %v), want a renew at epoch %d", lease, err,
				got.lease.Epoch)
		}
	},
}, {
	// A TAKE WHILE ANOTHER OWNER'S TAKE IS MINTING is unknown, never held: the
	// record holds no tenure yet, so a held answer would name nobody, and the
	// person's panel asks again. Released, the first take holds, and a second
	// take is told so.
	name: "a take while another owner's take is minting is unknown, never held naming nobody",
	fn: func(t *testing.T, b BrowserControlStore, gate *mintGate) {
		ctx := t.Context()
		l, ada, grace := newLaunch(), newPerson("Ada Lovelace"), newPerson("Grace Hopper")
		minting, release := gate.arm(coord.BrowserControlResource(l.launch))
		defer release()
		type answer struct {
			control coord.BrowserControl
			taken   coord.BrowserControlTaken
			err     error
		}
		first := make(chan answer, 1)
		go func() {
			control, taken, err := b.TakeBrowserControl(ctx, l.take(ada), LongTTL)
			first <- answer{control, taken, err}
		}()
		select {
		case <-minting:
		case got := <-first:
			t.Fatalf("the first take answered (%q, %v) without minting an epoch", got.taken, got.err)
		case <-time.After(30 * time.Second): //fixedwait:liveness
			t.Fatal("the first take never reached its mint")
		}

		control, taken, err := b.TakeBrowserControl(ctx, l.take(grace), LongTTL)
		if taken != "" || !errors.Is(err, coord.ErrUnavailable) {
			t.Errorf("a take while another owner's mints = (%+v, %q, %v), want only an error wrapping "+
				"coord.ErrUnavailable", control.Current, taken, err)
		}
		release()
		got := <-first
		won, ok := got.control.Held()
		if got.err != nil || got.taken != coord.ControlTaken || !ok {
			t.Fatalf("the first take, released = (%q, %v), want it taken", got.taken, got.err)
		}
		control, taken, err = b.TakeBrowserControl(ctx, l.take(grace), LongTTL)
		if held, ok := control.Held(); err != nil || taken != coord.ControlHeld || !ok ||
			held.Owner != ada.owner || held.Epoch != won.Epoch {
			t.Errorf("the second take, once the first held = (%+v, %q, %v), want held by Ada at epoch %d",
				control.Current, taken, err, won.Epoch)
		}
	},
}}
